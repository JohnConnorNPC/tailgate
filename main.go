// tailgate: a userspace tailscale node (tsnet) joined to headscale that maps
// local TCP ports onto tailnet hosts and exposes local ports on the tailnet,
// without touching the system tailscaled.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `tailgate - map ports between this machine and a headscale tailnet (userspace; system tailscale untouched)

daemon:
  tailgate serve [flags]                 run the node; see 'tailgate serve -h' (-expose, -api, -state-copy, -status-file, ...)

client (talks to the daemon's API: default 127.0.0.1:7070; -api or $TAILGATE_API, HOST:PORT or /path/to/socket):
  tailgate map [-p] [LISTEN] HOST:PORT   outbound: local LISTEN (PORT or ADDR:PORT; omitted = random port on
                                         127.0.0.1) -> HOST:PORT on the tailnet
  tailgate expose [-p] [-allow LIST] PORT[=HOST:PORT]
                                         inbound: tailnet PORT on this node -> local HOST:PORT (default 127.0.0.1:PORT)
                                         LIST: comma-separated tag:NAME, node name, user@, IP or CIDR
  tailgate ls                            list mappings
  tailgate rm ID...                      remove mappings
  tailgate persist ID [on|off]           toggle whether a mapping is persistent
  tailgate status                        tailnet status and peers
  tailgate health                        one-line up/down; exit status 0 when up
  tailgate ssh [ssh opts] [user@]host[:port] [command]
                                         one-off ssh through the daemon (system ssh + ProxyCommand; no port mapped)
  tailgate nc HOST PORT                  stdin/stdout <-> tailnet HOST:PORT (e.g. ProxyCommand='tailgate nc %h %p')

  -p = persistent (kept in the database and restored on every start)

profiles (several tailnets side by side; each has its own data dir and API socket):
  tailgate -n NAME <command>             use profile NAME (or $TAILGATE_PROFILE), e.g. tailgate -n work serve ...
  tailgate profiles                      list profiles on this machine
`

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		io.WriteString(os.Stderr, usage)
		os.Exit(2)
	}
	rest, err := takeProfile(os.Args[1:])
	if err == nil && len(rest) == 0 {
		err = fmt.Errorf("missing command (try: tailgate help)")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailgate:", err)
		os.Exit(2)
	}
	cmd, args := rest[0], rest[1:]
	switch cmd {
	case "profiles":
		err = cmdProfiles()
	case "serve":
		err = cmdServe(args)
	case "map", "add":
		err = cmdMap(args)
	case "expose":
		err = cmdExpose(args)
	case "ls", "list":
		err = cmdList(args)
	case "rm", "unmap":
		err = cmdRemove(args)
	case "persist":
		err = cmdPersist(args)
	case "status":
		err = cmdStatus(args)
	case "health":
		err = cmdHealth(args)
	case "ssh":
		err = cmdSSH(args)
	case "nc":
		err = cmdNC(args)
	case "-h", "--help", "help":
		io.WriteString(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailgate:", err)
		os.Exit(1)
	}
}

// parseAll parses flags even when they appear after positional args.
func parseAll(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type client struct {
	base string
	hc   *http.Client
}

func newClient(fs *flag.FlagSet) (*client, error) {
	addr := fs.Lookup("api").Value.String()
	if addr == "" {
		return nil, errors.New("no API address (the daemon's API may be disabled)")
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if path, ok := unixPath(addr); ok {
		hc.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}
		return &client{base: "http://tailgate", hc: hc}, nil
	}
	return &client{base: "http://" + addr, hc: hc}, nil
}

func (c *client) request(method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable (is `tailgate serve` running?): %w", err)
	}
	return resp, nil
}

func (c *client) do(method, path string, body, out any) error {
	resp, err := c.request(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func clientFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.String("api", apiDefault(), "daemon API: HOST:PORT or unix socket path")
	return fs
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func persistCol(m MappingInfo) string {
	if m.Static {
		return "flag"
	}
	return yn(m.Persistent)
}

func printMapping(m MappingInfo) {
	fmt.Printf("#%d  %s  %s -> %s%s  persistent=%s\n", m.ID, m.Dir, m.Listen, m.Target, allowSuffix(m.Allow), persistCol(m))
}

func cmdMap(args []string) error {
	fs := clientFlags("map")
	persist := fs.Bool("p", false, "persistent: keep this mapping across daemon restarts")
	fs.BoolVar(persist, "persist", false, "same as -p")
	pos := parseAll(fs, args)
	req := createReq{Dir: "out"}
	switch len(pos) {
	case 1:
		req.Target = pos[0]
	case 2:
		req.Listen, req.Target = pos[0], pos[1]
	default:
		return fmt.Errorf("usage: tailgate map [-p] [LISTEN] HOST:PORT")
	}
	req.Persistent = *persist
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	var m MappingInfo
	if err := c.do("POST", "/mappings", req, &m); err != nil {
		return err
	}
	printMapping(m)
	return nil
}

func cmdExpose(args []string) error {
	fs := clientFlags("expose")
	persist := fs.Bool("p", false, "persistent: keep this mapping across daemon restarts")
	fs.BoolVar(persist, "persist", false, "same as -p")
	allow := fs.String("allow", "", "comma-separated allow list: tag:NAME, node name, user@, IP or CIDR")
	pos := parseAll(fs, args)
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("usage: tailgate expose [-p] [-allow LIST] PORT[=HOST:PORT] (or PORT HOST:PORT)")
	}
	req, err := parseExpose(pos[0])
	if err != nil {
		return err
	}
	if len(pos) == 2 {
		if req.Target != "" {
			return fmt.Errorf("target given twice")
		}
		req.Target = pos[1]
	}
	if *allow != "" {
		req.Allow = strings.Trim(req.Allow+","+*allow, ",")
	}
	req.Persistent = *persist
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	var m MappingInfo
	if err := c.do("POST", "/mappings", req, &m); err != nil {
		return err
	}
	printMapping(m)
	return nil
}

func cmdList(args []string) error {
	fs := clientFlags("ls")
	parseAll(fs, args)
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	var ms []MappingInfo
	if err := c.do("GET", "/mappings", nil, &ms); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tDIR\tLISTEN\tTARGET\tALLOW\tPERSIST\tACTIVE\tTOTAL\tREJECTED\tERROR")
	for _, m := range ms {
		allow := m.Allow
		if allow == "" {
			allow = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			m.ID, m.Dir, m.Listen, m.Target, allow, persistCol(m), m.Active, m.Total, m.Rejected, m.Error)
	}
	return tw.Flush()
}

func cmdRemove(args []string) error {
	fs := clientFlags("rm")
	pos := parseAll(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("usage: tailgate rm ID...")
	}
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	for _, id := range pos {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return fmt.Errorf("bad id %q", id)
		}
		if err := c.do("DELETE", "/mappings/"+id, nil, nil); err != nil {
			return fmt.Errorf("#%s: %w", id, err)
		}
		fmt.Printf("removed #%s\n", id)
	}
	return nil
}

func cmdPersist(args []string) error {
	fs := clientFlags("persist")
	pos := parseAll(fs, args)
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("usage: tailgate persist ID [on|off]")
	}
	on := true
	if len(pos) == 2 {
		switch strings.ToLower(pos[1]) {
		case "on", "yes", "true", "1":
		case "off", "no", "false", "0":
			on = false
		default:
			return fmt.Errorf("want on or off, got %q", pos[1])
		}
	}
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	var m MappingInfo
	if err := c.do("PATCH", "/mappings/"+pos[0], map[string]bool{"persistent": on}, &m); err != nil {
		return err
	}
	printMapping(m)
	return nil
}

func cmdStatus(args []string) error {
	fs := clientFlags("status")
	parseAll(fs, args)
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	var st StatusInfo
	if err := c.do("GET", "/status", nil, &st); err != nil {
		return err
	}
	fmt.Printf("control: %s\nstate:   %s\nself:    %s  %s\ntags:    %s\n\n",
		st.Control, st.State, st.Self.DNS, strings.Join(st.Self.IPs, " "), strings.Join(st.Self.Tags, ","))
	if len(st.Peers) == 0 {
		fmt.Println("no peers visible: the headscale ACL policy grants this node nothing (check rules for its tags/user)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tIP\tOS\tONLINE\tTAGS")
	for _, p := range st.Peers {
		ip := ""
		if len(p.IPs) > 0 {
			ip = p.IPs[0]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.DNS, ip, p.OS, yn(p.Online), strings.Join(p.Tags, ","))
	}
	return tw.Flush()
}

func cmdHealth(args []string) error {
	fs := clientFlags("health")
	parseAll(fs, args)
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	resp, err := c.request("GET", "/health", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return err
	}
	line := fmt.Sprintf("%s %s %s %s", map[bool]string{true: "up", false: "down"}[h.Up], h.State, h.IP4, h.IP6)
	if h.StateCopy != nil {
		cs := h.StateCopy
		switch {
		case cs.Error != "":
			line += "  state-copy: " + cs.Error
		case cs.SavedAt != "":
			line += "  state-copy: saved " + cs.SavedAt
		}
	}
	fmt.Println(strings.Join(strings.Fields(line), " "))
	if !h.Up {
		os.Exit(1)
	}
	return nil
}
