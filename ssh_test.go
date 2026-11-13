package main

import (
	"strings"
	"testing"
)

func TestSSHDestIndex(t *testing.T) {
	for _, tc := range []struct {
		args string
		want int
	}{
		{"root@nas", 0},
		{"-v root@nas", 1},
		{"-p 22 root@nas", 2},
		{"-p22 root@nas", 1},
		{"-vp 22 root@nas uptime", 2},
		{"-vp22 root@nas", 1},
		{"-o StrictHostKeyChecking=no -i key root@nas ls -l", 4},
		{"-L 8080:localhost:80 -N root@nas", 3},
		{"-- root@nas", 1},
		{"-v", -1},
		{"-p", -1},
	} {
		if got := sshDestIndex(strings.Fields(tc.args)); got != tc.want {
			t.Errorf("sshDestIndex(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestSSHDest(t *testing.T) {
	for in, want := range map[string]string{
		"root@nas:2222":          "ssh://root@nas:2222",
		"nas:22":                 "ssh://nas:22",
		"root@nas":               "root@nas",
		"root@[fd7a::5]:22":      "ssh://root@[fd7a::5]:22",
		"root@fd7a::5":           "root@fd7a::5",
		"ssh://root@nas:22":      "ssh://root@nas:22",
		"root@100.64.0.8:22":     "ssh://root@100.64.0.8:22",
		"a@b@nas.tailnet.int:22": "ssh://a@b@nas.tailnet.int:22",
	} {
		if got := sshDest(in); got != want {
			t.Errorf("sshDest(%q) = %q, want %q", in, got, want)
		}
	}
}
