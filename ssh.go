package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// apiDial opens a raw connection to the daemon's API (TCP or unix socket).
func apiDial(addr string) (net.Conn, error) {
	if addr == "" {
		return nil, errors.New("no API address (the daemon's API may be disabled)")
	}
	network, target := "tcp", addr
	if p, ok := unixPath(addr); ok {
		network, target = "unix", p
	}
	c, err := net.DialTimeout(network, target, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable (is `tailgate serve` running?): %w", err)
	}
	return c, nil
}

// tunnel asks the daemon for a CONNECT tunnel to addr on the tailnet.
// The returned reader must be used for reads (it may hold early bytes).
func tunnel(api, addr string) (net.Conn, *bufio.Reader, error) {
	c, err := apiDial(api)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.Close()
		return nil, nil, fmt.Errorf("%s", strings.TrimSpace(string(msg)))
	}
	return c, br, nil
}

// cmdNC: tailgate nc HOST PORT (or HOST:PORT) - stdin/stdout <-> tailnet host.
// Works as an ssh ProxyCommand: -o ProxyCommand='tailgate nc %h %p'
func cmdNC(args []string) error {
	fs := clientFlags("nc")
	pos := parseAll(fs, args)
	var addr string
	switch len(pos) {
	case 1:
		addr = pos[0]
	case 2:
		addr = net.JoinHostPort(pos[0], pos[1])
	default:
		return fmt.Errorf("usage: tailgate nc HOST PORT")
	}
	if err := checkHostPort(addr, "address"); err != nil {
		return err
	}
	c, br, err := tunnel(fs.Lookup("api").Value.String(), addr)
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		io.Copy(c, os.Stdin)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	io.Copy(os.Stdout, br)
	return nil
}

// sshValueOpts are ssh's single-letter options that take an argument.
const sshValueOpts = "BbcDEeFIiJLlmOoPpQRSWw"

// sshDestIndex finds the destination among ssh's arguments.
func sshDestIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return i
		}
		// A cluster like -vp22 or -vp 22: the first value-taking letter eats
		// the rest of the cluster, or the next argument if it's last.
		for j := 1; j < len(a); j++ {
			if strings.IndexByte(sshValueOpts, a[j]) >= 0 {
				if j == len(a)-1 {
					i++
				}
				break
			}
		}
	}
	return -1
}

// sshDest rewrites user@host:port (or user@[v6]:port) as an ssh:// URI, which
// ssh understands; anything else is passed through.
func sshDest(d string) string {
	if strings.HasPrefix(d, "ssh://") {
		return d
	}
	user, host := "", d
	if i := strings.LastIndex(d, "@"); i >= 0 {
		user, host = d[:i+1], d[i+1:]
	}
	switch {
	case strings.HasPrefix(host, "[") && strings.Contains(host, "]:"),
		strings.Count(host, ":") == 1:
		if _, port, err := net.SplitHostPort(host); err == nil && port != "" {
			return "ssh://" + user + host
		}
	}
	return d
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdSSH: tailgate ssh [-api ADDR] [ssh options] [user@]host[:port] [command]
// Runs the system ssh with a ProxyCommand that tunnels through the daemon.
func cmdSSH(args []string) error {
	api := apiDefault()
	if len(args) > 0 {
		if v, ok := strings.CutPrefix(args[0], "-api="); ok {
			api, args = v, args[1:]
		} else if args[0] == "-api" && len(args) > 1 {
			api, args = args[1], args[2:]
		}
	}
	i := sshDestIndex(args)
	if i < 0 {
		return fmt.Errorf("usage: tailgate ssh [-api ADDR] [ssh options] [user@]host[:port] [command]")
	}
	args = append([]string(nil), args...)
	args[i] = sshDest(args[i])

	// Fail early with a clear message rather than an ssh kex error.
	c, err := apiDial(api)
	if err != nil {
		return err
	}
	c.Close()

	self, err := os.Executable()
	if err != nil {
		return err
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	proxy := fmt.Sprintf("%s nc -api %s %%h %%p", shellQuote(self), shellQuote(api))
	argv := append([]string{"ssh", "-o", "ProxyCommand=" + proxy}, args...)
	return syscall.Exec(sshPath, argv, os.Environ())
}
