package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
)

// profile names one tailgate instance on this machine ("" = the default one).
// A named profile gets its own data dir and a unix-socket API derived from
// the name, so several tailnets can run side by side with nothing to remember:
//
//	tailgate -n work serve -control https://...
//	tailgate -n work ssh root@host
var profile string

var profileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// takeProfile strips a leading "-n NAME" / "-n=NAME" from args, falling back
// to $TAILGATE_PROFILE.
func takeProfile(args []string) ([]string, error) {
	name := os.Getenv("TAILGATE_PROFILE")
	if len(args) > 0 {
		a := args[0]
		if strings.HasPrefix(a, "--") { // accept --n too
			a = a[1:]
		}
		switch {
		case a == "-n" && len(args) > 1:
			name, args = args[1], args[2:]
		case strings.HasPrefix(a, "-n="):
			name, args = a[3:], args[1:]
		case a == "-n":
			return nil, fmt.Errorf("-n needs a profile name")
		}
	}
	if name != "" && !profileRE.MatchString(name) {
		return nil, fmt.Errorf("bad profile name %q (letters, digits, . _ -)", name)
	}
	profile = name
	return args, nil
}

func dataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

func profileDir(name string) string {
	if name == "" {
		return filepath.Join(dataHome(), "tailgate")
	}
	return filepath.Join(dataHome(), "tailgate-"+name)
}

func defaultDir() string { return profileDir(profile) }

// profileSocket is where a named profile's API lives.
func profileSocket(name string) string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "tailgate-"+name+".sock")
	}
	return filepath.Join(profileDir(name), "api.sock")
}

func apiDefault() string {
	if v := os.Getenv("TAILGATE_API"); v != "" {
		return v
	}
	if profile != "" {
		return profileSocket(profile)
	}
	return "127.0.0.1:7070"
}

// socksDefault: only the default profile gets the well-known SOCKS port;
// named profiles choose their own with -socks.
func socksDefault() string {
	if profile != "" {
		return ""
	}
	return "127.0.0.1:1055"
}

// running reports whether a daemon holds the lock in dir.
func running(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// cmdProfiles lists the profiles that have a data dir here.
func cmdProfiles() error {
	ents, err := os.ReadDir(dataHome())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() || (n != "tailgate" && !strings.HasPrefix(n, "tailgate-")) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dataHome(), n, "tailgate.db")); err != nil {
			continue
		}
		names = append(names, strings.TrimPrefix(strings.TrimPrefix(n, "tailgate"), "-"))
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROFILE\tRUNNING\tDIR")
	for _, n := range names {
		show := n
		if n == "" {
			show = "(default)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", show, yn(running(profileDir(n))), profileDir(n))
	}
	return tw.Flush()
}
