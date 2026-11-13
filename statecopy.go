package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateCopy keeps a consistent copy of the live database somewhere else, so
// the live db can sit in RAM (-dir on tmpfs) while the copy sits on a disk
// that may come and go (e.g. NAS arrays). The copy is never held open: it is
// only written briefly (VACUUM INTO a temp file, then rename), so the disk can
// be unmounted at any time without stopping tailgate.
type stateCopy struct {
	path   string // the copy, e.g. /state/tailgate/tailgate.db
	marker string // <dir>/state-copy.owned: this RAM dir's db may overwrite the copy
	db     *DB
	kick   chan struct{}

	mu      sync.Mutex
	owned   bool
	dirty   bool
	savedAt time.Time
	lastErr string
}

type CopyStatus struct {
	Path    string `json:"path"`
	SavedAt string `json:"saved_at,omitempty"`
	Pending bool   `json:"pending"`
	Error   string `json:"error,omitempty"`
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// prepareStateCopy runs before the live db is opened. If the live db is
// missing it loads the copy (waiting up to wait for the copy's folder to show up).
func prepareStateCopy(copyPath, livePath, dir string, wait time.Duration) *stateCopy {
	sc := &stateCopy{
		path:   copyPath,
		marker: filepath.Join(dir, "state-copy.owned"),
		kick:   make(chan struct{}, 1),
	}
	if exists(livePath) {
		// Daemon restart without a reboot: keep using the live db, and keep
		// whatever ownership of the copy it had.
		sc.owned = exists(sc.marker)
		log.Printf("state: using live db %s (copy %s, owned=%v)", livePath, copyPath, sc.owned)
		return sc
	}
	folder := filepath.Dir(copyPath)
	if !isDir(folder) && wait > 0 {
		log.Printf("state: waiting up to %s for %s", wait, folder)
		for deadline := time.Now().Add(wait); !isDir(folder) && time.Now().Before(deadline); {
			time.Sleep(2 * time.Second)
		}
	}
	switch {
	case exists(copyPath):
		if err := loadCopy(copyPath, livePath); err != nil {
			os.Remove(livePath)
			log.Printf("state: WARNING could not load %s: %v; starting with a fresh node (the copy will not be overwritten)", copyPath, err)
			return sc
		}
		log.Printf("state: loaded %s", copyPath)
		sc.setOwned()
	case isDir(folder):
		log.Printf("state: no copy at %s yet; starting fresh, it will be created", copyPath)
		sc.setOwned()
	default:
		log.Printf("state: WARNING %s is not available; starting with a fresh node. "+
			"If a copy appears there later it will NOT be overwritten; restart tailgate to load it.", folder)
	}
	return sc
}

func (sc *stateCopy) setOwned() {
	sc.owned = true
	if err := os.WriteFile(sc.marker, []byte(sc.path+"\n"), 0o600); err != nil {
		log.Printf("state: write %s: %v", sc.marker, err)
	}
}

// loadCopy copies the snapshot into place as the live db and checks it opens.
func loadCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".load"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	db, err := openDB(dst)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}

// markDirty schedules a save; called on every state or persistent-mapping change.
func (sc *stateCopy) markDirty() {
	sc.mu.Lock()
	sc.dirty = true
	sc.mu.Unlock()
	select {
	case sc.kick <- struct{}{}:
	default:
	}
}

// run saves shortly after changes (coalescing bursts), and retries every 30s
// while a change is still unsaved (e.g. until the arrays are mounted).
func (sc *stateCopy) run(ctx context.Context) {
	retry := time.NewTicker(30 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
		case <-sc.kick:
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		sc.save()
	}
}

// flush does a final synchronous save at shutdown.
func (sc *stateCopy) flush() { sc.save() }

func (sc *stateCopy) save() {
	sc.mu.Lock()
	if !sc.dirty {
		sc.mu.Unlock()
		return
	}
	sc.dirty = false
	sc.mu.Unlock()

	err := sc.write()

	sc.mu.Lock()
	defer sc.mu.Unlock()
	if err != nil {
		sc.dirty = true
		if err.Error() != sc.lastErr {
			log.Printf("state: copy skipped: %v (will retry)", err)
		}
		sc.lastErr = err.Error()
		return
	}
	if sc.lastErr != "" {
		log.Printf("state: copy saved to %s", sc.path)
	}
	sc.lastErr = ""
	sc.savedAt = time.Now()
}

var errFolderMissing = errors.New("folder not available")

func (sc *stateCopy) write() error {
	folder := filepath.Dir(sc.path)
	if !isDir(folder) {
		return fmt.Errorf("%s: %w", folder, errFolderMissing)
	}
	sc.mu.Lock()
	owned := sc.owned
	sc.mu.Unlock()
	if !owned {
		if exists(sc.path) {
			return fmt.Errorf("not overwriting %s: this run did not load it (restart tailgate to load it, or remove it)", sc.path)
		}
		sc.mu.Lock()
		sc.setOwned()
		sc.mu.Unlock()
	}
	// VACUUM INTO needs a new or empty target; create it ourselves so it is
	// 0600 from the start (it holds the node's private keys).
	tmp := sc.path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err // read-only or full: skip
	}
	f.Close()
	if err := sc.db.snapshotTo(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		f.Sync()
		f.Close()
	}
	if err := os.Rename(tmp, sc.path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(folder); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func (sc *stateCopy) status() CopyStatus {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	cs := CopyStatus{Path: sc.path, Pending: sc.dirty, Error: sc.lastErr}
	if !sc.savedAt.IsZero() {
		cs.SavedAt = sc.savedAt.Format(time.RFC3339)
	}
	return cs
}
