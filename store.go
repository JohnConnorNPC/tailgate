package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"tailscale.com/ipn"
)

// One SQLite file holds both the port mappings and the tailscale node state
// (machine/node keys, prefs), so the whole gateway is a single .db file.
const schema = `
CREATE TABLE IF NOT EXISTS mappings (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	listen     TEXT    NOT NULL UNIQUE,
	target     TEXT    NOT NULL,
	persistent INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ts_state (
	key   TEXT PRIMARY KEY,
	value BLOB
);`

// Columns added after the first release, applied in place to older databases.
var migrations = []string{
	`ALTER TABLE mappings ADD COLUMN dir    TEXT    NOT NULL DEFAULT 'out'`,
	`ALTER TABLE mappings ADD COLUMN allow  TEXT    NOT NULL DEFAULT ''`,
	`ALTER TABLE mappings ADD COLUMN static INTEGER NOT NULL DEFAULT 0`,
}

type DB struct {
	*sql.DB
	// onChange, if set, is called after a change that should reach the
	// -state-copy (node state, or a persistent mapping).
	onChange func()
}

func (d *DB) notify() {
	if d.onChange != nil {
		d.onChange()
	}
}

func openDB(path string) (*DB, error) {
	// The db holds the node's private keys: keep it (and its -wal/-shm, which
	// SQLite creates with the main file's mode) owner-only.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &DB{DB: db}, nil
}

type mappingRow struct {
	ID         int64
	Dir        string // "out": local port -> tailnet host; "in": tailnet port -> local address
	Listen     string // "out": local HOST:PORT; "in": "tailnet:PORT"
	Target     string
	Allow      string // "in" only: comma-separated allow list ("" = anyone the ACL lets in)
	Persistent bool
	Static     bool // came from a -expose flag: re-created each start, not editable via API
	CreatedAt  time.Time
}

// purgeTransient drops non-persistent mappings left behind by a previous run
// (e.g. after a crash, where shutdown cleanup never ran). Static rows are
// transient too; their flags re-create them.
func (d *DB) purgeTransient() error {
	_, err := d.Exec(`DELETE FROM mappings WHERE persistent = 0`)
	return err
}

func (d *DB) loadMappings() ([]mappingRow, error) {
	rows, err := d.Query(`SELECT id, dir, listen, target, allow, persistent, static, created_at FROM mappings ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mappingRow
	for rows.Next() {
		var r mappingRow
		var ts int64
		if err := rows.Scan(&r.ID, &r.Dir, &r.Listen, &r.Target, &r.Allow, &r.Persistent, &r.Static, &ts); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) insertMapping(r mappingRow) (mappingRow, error) {
	r.CreatedAt = time.Now().Truncate(time.Second)
	res, err := d.Exec(`INSERT INTO mappings (dir, listen, target, allow, persistent, static, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Dir, r.Listen, r.Target, r.Allow, r.Persistent, r.Static, r.CreatedAt.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return r, fmt.Errorf("a mapping on %s already exists", r.Listen)
		}
		return r, err
	}
	r.ID, err = res.LastInsertId()
	return r, err
}

func (d *DB) deleteMapping(id int64) error {
	_, err := d.Exec(`DELETE FROM mappings WHERE id = ?`, id)
	return err
}

func (d *DB) setPersistent(id int64, p bool) error {
	_, err := d.Exec(`UPDATE mappings SET persistent = ? WHERE id = ?`, p, id)
	return err
}

// snapshotTo writes a consistent, self-contained copy of the database to path,
// which must not exist or be empty.
func (d *DB) snapshotTo(path string) error {
	_, err := d.Exec(`VACUUM INTO ?`, path)
	return err
}

// stateStore implements ipn.StateStore on top of the ts_state table.
type stateStore struct{ db *DB }

func (s stateStore) ReadState(id ipn.StateKey) ([]byte, error) {
	var b []byte
	err := s.db.QueryRow(`SELECT value FROM ts_state WHERE key = ?`, string(id)).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ipn.ErrStateNotExist
	}
	return b, err
}

func (s stateStore) WriteState(id ipn.StateKey, bs []byte) error {
	old, err := s.ReadState(id)
	switch {
	case bs == nil && errors.Is(err, ipn.ErrStateNotExist):
		return nil
	case bs != nil && err == nil && bytes.Equal(old, bs):
		return nil // unchanged: don't touch the db or the state copy
	}
	if bs == nil {
		_, err = s.db.Exec(`DELETE FROM ts_state WHERE key = ?`, string(id))
	} else {
		_, err = s.db.Exec(`INSERT INTO ts_state (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(id), bs)
	}
	if err == nil {
		s.db.notify()
	}
	return err
}
