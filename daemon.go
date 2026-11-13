package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

// MappingInfo is the API/JSON view of a mapping.
type MappingInfo struct {
	ID         int64     `json:"id"`
	Dir        string    `json:"dir"`
	Listen     string    `json:"listen"`
	Target     string    `json:"target"`
	Allow      string    `json:"allow,omitempty"`
	Persistent bool      `json:"persistent"`
	Static     bool      `json:"static,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Active     int64     `json:"active"`
	Total      int64     `json:"total"`
	Rejected   int64     `json:"rejected,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type mapping struct {
	mappingRow
	allow    []string
	err      string // bind error for a mapping that could not listen
	ln       net.Listener
	active   atomic.Int64
	total    atomic.Int64
	rejected atomic.Int64
}

func (m *mapping) info() MappingInfo {
	return MappingInfo{
		ID: m.ID, Dir: m.Dir, Listen: m.Listen, Target: m.Target, Allow: m.Allow,
		Persistent: m.Persistent, Static: m.Static, CreatedAt: m.CreatedAt,
		Active: m.active.Load(), Total: m.total.Load(), Rejected: m.rejected.Load(), Error: m.err,
	}
}

type createReq struct {
	Dir        string `json:"dir"` // "out" (default) or "in"
	Listen     string `json:"listen"`
	Target     string `json:"target"`
	Allow      string `json:"allow,omitempty"`
	Persistent bool   `json:"persistent"`
}

type gateway struct {
	ts          *tsnet.Server
	lc          *local.Client
	db          *DB
	dialTimeout time.Duration

	mu   sync.Mutex
	maps map[int64]*mapping
}

// ---- parsing ----

// normListen turns "8022" into "127.0.0.1:8022"; anything with a colon is used as-is.
func normListen(s string) (string, error) {
	if s == "" {
		return "127.0.0.1:0", nil
	}
	if !strings.Contains(s, ":") {
		if _, err := strconv.ParseUint(s, 10, 16); err != nil {
			return "", fmt.Errorf("bad listen %q: want PORT or HOST:PORT", s)
		}
		return "127.0.0.1:" + s, nil
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return "", fmt.Errorf("bad listen %q: %v", s, err)
	}
	return s, nil
}

func parsePort(s string) (string, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "tailnet:"), 10, 16)
	if err != nil || n == 0 {
		return "", fmt.Errorf("bad port %q", s)
	}
	return strconv.FormatUint(n, 10), nil
}

func checkHostPort(s, what string) error {
	if h, p, err := net.SplitHostPort(s); err != nil || h == "" || p == "" {
		return fmt.Errorf("bad %s %q: want HOST:PORT", what, s)
	}
	return nil
}

// parseAllow validates a comma-separated allow list and returns its entries:
// tag:NAME, a node name (short or FQDN), a user login (contains @), an IP or a CIDR.
func parseAllow(s string) ([]string, error) {
	var out []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		switch {
		case a == "":
			continue
		case strings.ContainsAny(a, " \t=") || a == "tag:":
			return nil, fmt.Errorf("bad allow entry %q", a)
		}
		out = append(out, a)
	}
	return out, nil
}

// parseExpose parses the -expose syntax: PORT[=TARGET][@ALLOW,...].
// TARGET may be HOST:PORT or just PORT (on 127.0.0.1); it defaults to 127.0.0.1:PORT.
func parseExpose(s string) (createReq, error) {
	spec, allow, _ := strings.Cut(s, "@")
	port, target, _ := strings.Cut(spec, "=")
	req := createReq{Dir: "in", Listen: port, Target: target, Allow: allow}
	if _, err := parsePort(port); err != nil {
		return req, fmt.Errorf("expose %q: %w", s, err)
	}
	if _, err := parseAllow(allow); err != nil {
		return req, fmt.Errorf("expose %q: %w", s, err)
	}
	return req, nil
}

// ---- mapping lifecycle ----

// bind opens the listener for a mapping row: a local socket for "out", a
// tailnet socket (all of the node's tailnet IPs, v4 and v6) for "in".
func (g *gateway) bind(r mappingRow) (net.Listener, error) {
	if r.Dir == "in" {
		port, err := parsePort(r.Listen)
		if err != nil {
			return nil, err
		}
		return g.ts.Listen("tcp", ":"+port)
	}
	return net.Listen("tcp", r.Listen)
}

func (g *gateway) add(req createReq, static bool) (*mapping, error) {
	r := mappingRow{Dir: req.Dir, Target: req.Target, Persistent: req.Persistent && !static, Static: static}
	var ln net.Listener
	var allow []string
	switch req.Dir {
	case "", "out":
		r.Dir = "out"
		listen, err := normListen(req.Listen)
		if err != nil {
			return nil, err
		}
		if err := checkHostPort(r.Target, "target"); err != nil {
			return nil, err
		}
		if req.Allow != "" {
			return nil, errors.New("allow lists only apply to exposed (inbound) ports")
		}
		if ln, err = net.Listen("tcp", listen); err != nil {
			return nil, err
		}
		// Store the real bound address so an auto-assigned port (":0") is
		// stable across restarts for persistent mappings.
		r.Listen = ln.Addr().String()
	case "in":
		port, err := parsePort(req.Listen)
		if err != nil {
			return nil, err
		}
		switch {
		case r.Target == "":
			r.Target = "127.0.0.1:" + port
		case !strings.Contains(r.Target, ":"):
			if _, err := parsePort(r.Target); err != nil {
				return nil, fmt.Errorf("bad target %q", r.Target)
			}
			r.Target = "127.0.0.1:" + r.Target
		}
		if err := checkHostPort(r.Target, "target"); err != nil {
			return nil, err
		}
		if allow, err = parseAllow(req.Allow); err != nil {
			return nil, err
		}
		r.Allow = strings.Join(allow, ",")
		r.Listen = "tailnet:" + port
		if ln, err = g.ts.Listen("tcp", ":"+port); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("bad dir %q: want out or in", req.Dir)
	}
	row, err := g.db.insertMapping(r)
	if err != nil {
		ln.Close()
		return nil, err
	}
	if row.Persistent {
		g.db.notify()
	}
	m := &mapping{mappingRow: row, allow: allow, ln: ln}
	g.mu.Lock()
	g.maps[m.ID] = m
	g.mu.Unlock()
	go g.serve(m)
	log.Printf("%s #%d %s -> %s%s (persistent=%v static=%v)", m.Dir, m.ID, m.Listen, m.Target, allowSuffix(m.Allow), m.Persistent, m.Static)
	return m, nil
}

func allowSuffix(a string) string {
	if a == "" {
		return ""
	}
	return " allow=" + a
}

func (g *gateway) restore() error {
	if err := g.db.purgeTransient(); err != nil {
		return err
	}
	rows, err := g.db.loadMappings()
	if err != nil {
		return err
	}
	for _, r := range rows {
		m := &mapping{mappingRow: r}
		m.allow, _ = parseAllow(r.Allow)
		if ln, err := g.bind(r); err != nil {
			m.err = err.Error()
			log.Printf("%s #%d %s -> %s: restore failed: %v", r.Dir, r.ID, r.Listen, r.Target, err)
		} else {
			m.ln = ln
			go g.serve(m)
			log.Printf("%s #%d %s -> %s%s (restored)", r.Dir, r.ID, r.Listen, r.Target, allowSuffix(r.Allow))
		}
		g.maps[r.ID] = m
	}
	return nil
}

var (
	errNotFound = errors.New("no such mapping")
	errStatic   = errors.New("mapping comes from a -expose flag; change the command line instead")
)

func (g *gateway) remove(id int64) error {
	g.mu.Lock()
	m, ok := g.maps[id]
	if ok && m.Static {
		g.mu.Unlock()
		return errStatic
	}
	delete(g.maps, id)
	g.mu.Unlock()
	if !ok {
		return errNotFound
	}
	if m.ln != nil {
		m.ln.Close()
	}
	log.Printf("unmap %s #%d %s -> %s", m.Dir, m.ID, m.Listen, m.Target)
	if err := g.db.deleteMapping(id); err != nil {
		return err
	}
	if m.Persistent {
		g.db.notify()
	}
	return nil
}

func (g *gateway) setPersistent(id int64, p bool) (*mapping, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.maps[id]
	if !ok {
		return nil, errNotFound
	}
	if m.Static {
		return nil, errStatic
	}
	if err := g.db.setPersistent(id, p); err != nil {
		return nil, err
	}
	m.Persistent = p
	g.db.notify()
	return m, nil
}

func (g *gateway) list() []MappingInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]MappingInfo, 0, len(g.maps))
	for _, m := range g.maps {
		out = append(out, m.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (g *gateway) closeAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.maps {
		if m.ln != nil {
			m.ln.Close()
		}
	}
}

func (g *gateway) serve(m *mapping) {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("%s #%d accept: %v", m.Dir, m.ID, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if m.Dir == "in" {
			go g.handleIn(m, c)
		} else {
			go g.handleOut(m, c)
		}
	}
}

// handleOut: local client -> tailnet target.
func (g *gateway) handleOut(m *mapping, c net.Conn) {
	defer c.Close()
	m.active.Add(1)
	m.total.Add(1)
	defer m.active.Add(-1)

	ctx, cancel := context.WithTimeout(context.Background(), g.dialTimeout)
	up, err := g.ts.Dial(ctx, "tcp", m.Target)
	cancel()
	if err != nil {
		log.Printf("out #%d %s: dial %s: %v", m.ID, c.RemoteAddr(), m.Target, err)
		return
	}
	defer up.Close()
	pipe(c, up)
}

// handleIn: tailnet peer -> local target. Every connection is logged with the
// peer's identity, since the local service only ever sees 127.0.0.1.
func (g *gateway) handleIn(m *mapping, c net.Conn) {
	defer c.Close()
	m.total.Add(1)
	src := c.RemoteAddr().String()
	srcIP := netip.Addr{}
	if ap, err := netip.ParseAddrPort(src); err == nil {
		srcIP = ap.Addr().Unmap()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	who, err := g.lc.WhoIs(ctx, src)
	cancel()
	id := describe(who)
	if err != nil {
		id = "whois failed: " + err.Error()
	}
	if !allowed(m.allow, srcIP, who) {
		m.rejected.Add(1)
		log.Printf("in #%d %s <- %s %s: REJECTED (allow=%s)", m.ID, m.Listen, src, id, m.Allow)
		return
	}
	m.active.Add(1)
	defer m.active.Add(-1)
	log.Printf("in #%d %s <- %s %s -> %s", m.ID, m.Listen, src, id, m.Target)

	start := time.Now()
	up, err := net.DialTimeout("tcp", m.Target, g.dialTimeout)
	if err != nil {
		log.Printf("in #%d %s <- %s: dial %s: %v", m.ID, m.Listen, src, m.Target, err)
		return
	}
	defer up.Close()
	rx, tx := pipe(c, up)
	log.Printf("in #%d %s <- %s closed after %s (rx %s, tx %s)", m.ID, m.Listen, src,
		time.Since(start).Round(time.Second), humanBytes(rx), humanBytes(tx))
}

func describe(w *apitype.WhoIsResponse) string {
	if w == nil || w.Node == nil {
		return "(unknown node)"
	}
	s := strings.TrimSuffix(w.Node.Name, ".")
	if len(w.Node.Tags) > 0 {
		s += " [" + strings.Join(w.Node.Tags, ",") + "]"
	} else if w.UserProfile != nil {
		s += " user=" + w.UserProfile.LoginName
	}
	return s
}

// allowed reports whether the peer matches the allow list (empty = everyone).
func allowed(list []string, src netip.Addr, w *apitype.WhoIsResponse) bool {
	if len(list) == 0 {
		return true
	}
	var tags []string
	var fqdn, login string
	if w != nil && w.Node != nil {
		tags = w.Node.Tags
		fqdn = strings.TrimSuffix(w.Node.Name, ".")
		if len(tags) == 0 && w.UserProfile != nil {
			login = w.UserProfile.LoginName
		}
	}
	short, _, _ := strings.Cut(fqdn, ".")
	for _, a := range list {
		if strings.HasPrefix(a, "tag:") {
			if slices.Contains(tags, a) {
				return true
			}
			continue
		}
		if p, err := netip.ParsePrefix(a); err == nil {
			if src.IsValid() && p.Contains(src) {
				return true
			}
			continue
		}
		if ip, err := netip.ParseAddr(a); err == nil {
			if ip == src {
				return true
			}
			continue
		}
		if strings.Contains(a, "@") {
			if login != "" && strings.EqualFold(a, login) {
				return true
			}
			continue
		}
		if fqdn != "" && (strings.EqualFold(a, fqdn) || strings.EqualFold(a, short)) {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1fGiB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1fMiB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1fKiB", float64(n)/k)
	}
	return fmt.Sprintf("%dB", n)
}

// pipe copies both ways, propagating half-closes; any hard error tears down
// both. It returns the bytes copied a->b and b->a.
func pipe(a, b net.Conn) (ab, ba int64) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		var err error
		*n, err = io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(b, a, &ab)
	go cp(a, b, &ba)
	wg.Wait()
	return ab, ba
}

// ---- health / status ----

type PeerInfo struct {
	Name   string   `json:"name"`
	DNS    string   `json:"dns"`
	IPs    []string `json:"ips"`
	OS     string   `json:"os"`
	Tags   []string `json:"tags,omitempty"`
	Online bool     `json:"online"`
}

type StatusInfo struct {
	State   string     `json:"state"`
	Control string     `json:"control"`
	Self    PeerInfo   `json:"self"`
	Peers   []PeerInfo `json:"peers"`
}

// Health is the small up/down summary served at /health and written to -status-file.
type Health struct {
	Up          bool        `json:"up"`
	State       string      `json:"state"`
	Name        string      `json:"name,omitempty"`
	IP4         string      `json:"ip4,omitempty"`
	IP6         string      `json:"ip6,omitempty"`
	Tags        []string    `json:"tags,omitempty"`
	PeersOnline int         `json:"peers_online"`
	Warnings    []string    `json:"warnings,omitempty"`
	PID         int         `json:"pid"`
	StateCopy   *CopyStatus `json:"state_copy,omitempty"`
}

func healthOf(ctx context.Context, lc *local.Client, sc *stateCopy) Health {
	h := Health{State: "Unknown", PID: os.Getpid()}
	if sc != nil {
		cs := sc.status()
		h.StateCopy = &cs
	}
	st, err := lc.Status(ctx)
	if err != nil {
		h.State = "Error: " + err.Error()
		return h
	}
	h.State = st.BackendState
	h.Up = st.BackendState == "Running"
	h.Warnings = st.Health
	if st.Self != nil {
		h.Name = strings.TrimSuffix(st.Self.DNSName, ".")
		for _, ip := range st.Self.TailscaleIPs {
			if ip.Is4() {
				h.IP4 = ip.String()
			} else {
				h.IP6 = ip.String()
			}
		}
		if st.Self.Tags != nil {
			h.Tags = st.Self.Tags.AsSlice()
		}
	}
	for _, p := range st.Peer {
		if p.Online {
			h.PeersOnline++
		}
	}
	return h
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// statusFileLoop rewrites path whenever the health summary changes; it checks
// every 5s, or right away when kicked.
func statusFileLoop(ctx context.Context, path string, lc *local.Client, sc *stateCopy, kick <-chan struct{}) {
	var last []byte
	var lastErr string
	for {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		h := healthOf(qctx, lc, sc)
		cancel()
		if ctx.Err() != nil {
			return
		}
		b, _ := json.MarshalIndent(h, "", "  ")
		b = append(b, '\n')
		if string(b) != string(last) {
			if err := writeFileAtomic(path, b); err != nil {
				if err.Error() != lastErr {
					log.Printf("status file: %v", err)
				}
				lastErr = err.Error()
			} else {
				last, lastErr = b, ""
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-time.After(5 * time.Second):
		}
	}
}

// ---- HTTP control API ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func peerInfo(p *ipnstate.PeerStatus) PeerInfo {
	pi := PeerInfo{Name: p.HostName, DNS: strings.TrimSuffix(p.DNSName, "."), OS: p.OS, Online: p.Online}
	for _, ip := range p.TailscaleIPs {
		pi.IPs = append(pi.IPs, ip.String())
	}
	if p.Tags != nil {
		pi.Tags = p.Tags.AsSlice()
	}
	return pi
}

func errCode(err error) int {
	switch {
	case errors.Is(err, errNotFound):
		return 404
	case errors.Is(err, errStatic):
		return 409
	}
	return 400
}

// connect handles "CONNECT host:port" on the control API: a one-shot tunnel to
// a tailnet host for `tailgate nc` / `tailgate ssh`, with no listening port.
func (g *gateway) connect(w http.ResponseWriter, r *http.Request) {
	addr := r.Host
	if err := checkHostPort(addr, "address"); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.dialTimeout)
	up, err := g.ts.Dial(ctx, "tcp", addr)
	cancel()
	if err != nil {
		log.Printf("connect %s: %v", addr, err)
		http.Error(w, fmt.Sprintf("dial %s: %v", addr, err), 502)
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", 500)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Time{}) // the server's header timeout must not cut the tunnel
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	if n := brw.Reader.Buffered(); n > 0 { // client bytes that arrived with the request
		b, _ := brw.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			return
		}
	}
	start := time.Now()
	log.Printf("connect -> %s", addr)
	tx, rx := pipe(c, up)
	log.Printf("connect -> %s closed after %s (tx %s, rx %s)", addr,
		time.Since(start).Round(time.Second), humanBytes(tx), humanBytes(rx))
}

func (g *gateway) handler(controlURL string, sc *stateCopy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mappings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, g.list())
	})
	mux.HandleFunc("POST /mappings", func(w http.ResponseWriter, r *http.Request) {
		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		m, err := g.add(req, false)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, m.info())
	})
	mux.HandleFunc("DELETE /mappings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err == nil {
			err = g.remove(id)
		}
		if err != nil {
			writeErr(w, errCode(err), err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("PATCH /mappings/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Persistent *bool `json:"persistent"`
		}
		id, err := pathID(r)
		if err == nil {
			err = json.NewDecoder(r.Body).Decode(&req)
		}
		if err == nil && req.Persistent == nil {
			err = errors.New(`body must set "persistent"`)
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		m, err := g.setPersistent(id, *req.Persistent)
		if err != nil {
			writeErr(w, errCode(err), err)
			return
		}
		writeJSON(w, 200, m.info())
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		h := healthOf(r.Context(), g.lc, sc)
		code := 200
		if !h.Up {
			code = 503
		}
		writeJSON(w, code, h)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st, err := g.lc.Status(r.Context())
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := StatusInfo{State: st.BackendState, Control: controlURL, Peers: []PeerInfo{}}
		if st.Self != nil {
			out.Self = peerInfo(st.Self)
		}
		for _, p := range st.Peer {
			out.Peers = append(out.Peers, peerInfo(p))
		}
		sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].DNS < out.Peers[j].DNS })
		writeJSON(w, 200, out)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			g.connect(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// unixPath reports whether an -api value names a unix socket.
func unixPath(addr string) (string, bool) {
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		return p, true
	}
	if strings.HasPrefix(addr, "/") || strings.HasPrefix(addr, "./") {
		return addr, true
	}
	return "", false
}

// listenAPI opens the control API listener: nil if addr is "", a unix socket
// created with the given mode, or a TCP address.
func listenAPI(addr string, mode os.FileMode) (net.Listener, error) {
	if addr == "" {
		return nil, nil
	}
	path, ok := unixPath(addr)
	if !ok {
		return net.Listen("tcp", addr)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is in use by another process", path)
		}
		os.Remove(path) // stale socket from a previous run
	}
	// Create the socket with the right mode from the start (no chmod race).
	old := syscall.Umask(int(0o777 &^ mode.Perm()))
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode.Perm()); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func defaultDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "tailgate")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "tailgate")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	controlURL := fs.String("control", os.Getenv("TAILGATE_CONTROL"), "headscale server URL (needed on first run; remembered in the database afterwards)")
	hostname := fs.String("hostname", envOr("TAILGATE_HOSTNAME", "tailgate"), "node name on the tailnet")
	dir := fs.String("dir", defaultDir(), "data dir: live database, lock (may be tmpfs when -state-copy is set)")
	keyFile := fs.String("authkey-file", "", "file holding a headscale pre-auth key (only needed for first login; $TS_AUTHKEY also works)")
	apiAddr := fs.String("api", apiDefault(), `control API: HOST:PORT, a unix socket (/path or unix:path), or "" to disable`)
	apiMode := fs.String("api-mode", "0600", "file mode of a unix-socket API")
	socksAddr := fs.String("socks", "127.0.0.1:1055", `SOCKS5 proxy into the tailnet ("" to disable)`)
	var exposes []createReq
	fs.Func("expose", "expose `PORT[=HOST:PORT][@ALLOW,...]` on the tailnet (repeatable), e.g. 22=127.0.0.1:22 or 443=127.0.0.1:443@tag:pbs", func(s string) error {
		r, err := parseExpose(s)
		if err == nil {
			exposes = append(exposes, r)
		}
		return err
	})
	copyPath := fs.String("state-copy", "", "keep a consistent copy of the database at this path; loaded at start when -dir has none, "+
		"rewritten after each state change, skipped while its folder is missing or read-only")
	copyWait := fs.Duration("state-copy-wait", 0, "at start with no live database, wait up to this long for the -state-copy folder to appear")
	statusFile := fs.String("status-file", "", "keep a small JSON health file (up/down, tailnet IPs) at this path")
	dialTimeout := fs.Duration("dial-timeout", 15*time.Second, "timeout for dialing a mapping's target")
	verbose := fs.Bool("v", false, "verbose tailscale logs")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	mode, err := strconv.ParseUint(*apiMode, 8, 32)
	if err != nil || mode > 0o777 {
		return fmt.Errorf("bad -api-mode %q", *apiMode)
	}

	// Never ship logs to Tailscale Inc.'s log server: this node belongs to headscale.
	envknob.SetNoLogsNoSupport()

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	// Two daemons sharing one node key would fight over the tailnet identity.
	lock, err := os.OpenFile(filepath.Join(*dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another tailgate is already running on %s", *dir)
	}
	// Grab the local ports before joining the tailnet, so a clash fails fast.
	apiLn, err := listenAPI(*apiAddr, os.FileMode(mode))
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	if apiLn != nil {
		defer apiLn.Close()
	}
	var sln net.Listener
	if *socksAddr != "" {
		if sln, err = net.Listen("tcp", *socksAddr); err != nil {
			return fmt.Errorf("socks listen: %w", err)
		}
		defer sln.Close()
	}

	livePath := filepath.Join(*dir, "tailgate.db")
	var sc *stateCopy
	if *copyPath != "" {
		sc = prepareStateCopy(*copyPath, livePath, *dir, *copyWait)
	}
	db, err := openDB(livePath)
	if err != nil {
		return err
	}
	defer db.Close()
	if sc != nil {
		sc.db = db
		db.onChange = sc.markDirty
	}

	// The control URL is remembered with the node state, so only the first
	// run has to name the server.
	const controlKey = "_tailgate/control-url"
	store := stateStore{db}
	if *controlURL == "" {
		if b, err := store.ReadState(controlKey); err == nil {
			*controlURL = string(b)
		}
	}
	if *controlURL == "" {
		return errors.New("no headscale server: pass -control https://your-headscale (or set $TAILGATE_CONTROL)")
	}
	if err := store.WriteState(controlKey, []byte(*controlURL)); err != nil {
		return err
	}

	var authKey string
	if *keyFile != "" {
		// Only needed for a first login, and it may live on a disk that isn't
		// mounted yet: an unreadable key file must not stop a node that has state.
		if b, err := os.ReadFile(*keyFile); err != nil {
			log.Printf("authkey: %v (continuing without it)", err)
		} else {
			authKey = strings.TrimSpace(string(b))
		}
	}

	tsLogf := logger.Discard
	if *verbose {
		tsLogf = log.Printf
	}
	ts := &tsnet.Server{
		Dir:        *dir,
		Store:      stateStore{db},
		Hostname:   *hostname,
		ControlURL: *controlURL,
		AuthKey:    authKey,
		Logf:       tsLogf,
		UserLogf:   log.Printf,
	}
	closeTS := sync.OnceFunc(func() { ts.Close() })
	defer closeTS()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := ts.Start(); err != nil {
		return err
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	var bg sync.WaitGroup
	statusKick := make(chan struct{}, 1)
	if *statusFile != "" {
		bg.Add(1)
		go func() { defer bg.Done(); statusFileLoop(ctx, *statusFile, lc, sc, statusKick) }()
	}
	if sc != nil {
		bg.Add(1)
		go func() { defer bg.Done(); sc.run(ctx) }()
	}

	var g *gateway
	var api *http.Server
	// finish is the orderly shutdown, also used when stopped before Up completes.
	finish := func() {
		log.Printf("shutting down")
		if api != nil {
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			api.Shutdown(sctx)
			cancel()
		}
		if g != nil {
			g.closeAll()
		}
		bg.Wait()
		closeTS()
		if err := db.purgeTransient(); err != nil {
			log.Printf("purge transient mappings: %v", err)
		}
		if sc != nil {
			sc.flush()
		}
		if *statusFile != "" {
			b, _ := json.MarshalIndent(Health{State: "Stopped", PID: os.Getpid()}, "", "  ")
			writeFileAtomic(*statusFile, append(b, '\n'))
		}
	}

	log.Printf("connecting to %s as %q ...", *controlURL, *hostname)
	st, err := ts.Up(ctx)
	if err != nil {
		if ctx.Err() != nil { // signalled while still connecting
			finish()
			return nil
		}
		return err
	}
	select {
	case statusKick <- struct{}{}:
	default:
	}
	var ips []string
	for _, ip := range st.TailscaleIPs {
		ips = append(ips, ip.String())
	}
	log.Printf("up: %s %v", strings.TrimSuffix(st.Self.DNSName, "."), ips)

	g = &gateway{ts: ts, lc: lc, db: db, dialTimeout: *dialTimeout, maps: map[int64]*mapping{}}
	if err := g.restore(); err != nil {
		return fmt.Errorf("restore mappings: %w", err)
	}
	for _, req := range exposes {
		if _, err := g.add(req, true); err != nil {
			log.Printf("expose %s: %v", req.Listen, err) // keep running: other exposes may matter more
		}
	}

	if apiLn != nil {
		api = &http.Server{Handler: g.handler(*controlURL, sc), ReadHeaderTimeout: 10 * time.Second}
		go api.Serve(apiLn)
		log.Printf("control API on %s", apiLn.Addr())
	} else {
		log.Printf("control API disabled")
	}
	if sln != nil {
		ss := &socks5.Server{Logf: logger.WithPrefix(log.Printf, "socks5: "), Dialer: ts.Dial}
		go ss.Serve(sln)
		log.Printf("SOCKS5 proxy on %s", sln.Addr())
	}

	<-ctx.Done()
	finish()
	return nil
}
