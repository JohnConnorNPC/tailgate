package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"tailscale.com/tailcfg"
)

// PingReply is one ping result from GET /ping.
type PingReply struct {
	Name      string  `json:"name"`
	IP        string  `json:"ip"`
	Via       string  `json:"via,omitempty"` // "direct 1.2.3.4:41641", "DERP(syd)" or "peer relay ..."
	Direct    bool    `json:"direct"`
	LatencyMS float64 `json:"latency_ms"`
}

// resolvePeer turns a tailnet IP, MagicDNS name or short name into a peer's IP.
func (g *gateway) resolvePeer(ctx context.Context, host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip, nil
	}
	st, err := g.lc.Status(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	want := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range st.Peer {
		fqdn := strings.ToLower(strings.TrimSuffix(p.DNSName, "."))
		short, _, _ := strings.Cut(fqdn, ".")
		if (want == fqdn || want == short || want == strings.ToLower(p.HostName)) && len(p.TailscaleIPs) > 0 {
			return p.TailscaleIPs[0], nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no peer named %q (see `tailgate status`)", host)
}

// ping handles GET /ping?host=NAME[&type=tsmp]: one tailscale-level ping.
func (g *gateway) ping(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		writeErr(w, 400, errors.New("missing host"))
		return
	}
	pt := tailcfg.PingDisco
	if strings.EqualFold(r.URL.Query().Get("type"), "tsmp") {
		pt = tailcfg.PingTSMP
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ip, err := g.resolvePeer(ctx, host)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	pr, err := g.lc.Ping(ctx, ip, pt)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("timeout")
		}
		writeErr(w, 504, err)
		return
	}
	if pr.Err != "" {
		writeErr(w, 502, errors.New(pr.Err))
		return
	}
	rep := PingReply{Name: pr.NodeName, IP: pr.NodeIP, LatencyMS: pr.LatencySeconds * 1000}
	switch {
	case pr.IsLocalIP:
		rep.Via, rep.Direct = "local (this node)", true
	case pr.Endpoint != "":
		rep.Via, rep.Direct = "direct "+pr.Endpoint, true
	case pr.PeerRelay != "":
		rep.Via = "peer relay " + pr.PeerRelay
	case pr.DERPRegionCode != "":
		rep.Via = "DERP(" + pr.DERPRegionCode + ")"
	}
	writeJSON(w, 200, rep)
}

// cmdPing: tailgate ping [-c N] [-tsmp] HOST
func cmdPing(args []string) error {
	fs := clientFlags("ping")
	count := fs.Int("c", 4, "number of pings (0 = until interrupted)")
	tsmp := fs.Bool("tsmp", false, "ping through the full tunnel (TSMP) instead of a path check (disco)")
	pos := parseAll(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: tailgate ping [-c N] [-tsmp] HOST")
	}
	c, err := newClient(fs)
	if err != nil {
		return err
	}
	q := "/ping?host=" + url.QueryEscape(pos[0])
	if *tsmp {
		q += "&type=tsmp"
	}
	ok := 0
	for i := 0; *count == 0 || i < *count; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}
		var r PingReply
		if err := c.do("GET", q, nil, &r); err != nil {
			if strings.HasPrefix(err.Error(), "no peer named") || strings.HasPrefix(err.Error(), "daemon not reachable") {
				return err
			}
			fmt.Printf("ping %s: %v\n", pos[0], err)
			continue
		}
		ok++
		via := ""
		if r.Via != "" { // TSMP pings don't report the path
			via = " via " + r.Via
		}
		fmt.Printf("pong from %s (%s)%s in %.0fms\n", r.Name, r.IP, via, r.LatencyMS)
	}
	if ok == 0 {
		os.Exit(1)
	}
	return nil
}
