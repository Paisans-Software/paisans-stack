package apply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Connection sharing.
//
// Opening an ssh connection costs a TCP handshake, a key exchange and an
// authentication, over a second each time, and a command that reaches a host
// sends it dozens of remote commands. So every ssh this package runs shares
// one connection per destination: the first to a destination becomes its
// master (ControlMaster=auto), and the rest send their command over it
// through a socket in a private directory. ControlPersist keeps a master open
// for a short while after its last command, which is what lets the next
// command use it at all, since each ssh exits when its command does.
//
// The options are given on the command line, so they take precedence over any
// ControlMaster, ControlPath or ControlPersist in the operator's ssh config:
// the socket is always one this process made, in a directory only this user
// can enter, and CloseConnections closes every master before the process
// exits.
//
// A master that has gone is not an error. ssh with ControlMaster=auto, finding
// no socket or one nobody answers on, removes the stale socket and connects
// afresh, becoming the master itself, without printing anything.

// controlPersist is how long a master stays open after its last command. Long
// enough to span the gap between two commands, which is the time the toolkit
// spends between them; short enough that a master CloseConnections never
// reached (a process killed outright) does not linger.
const controlPersist = "60"

// controlPathMax is the longest directory the sockets may go in. A unix socket
// path holds 104 bytes on macOS (108 on Linux), the terminating NUL included.
// In the directory ssh puts "/", %C (a SHA-1 in hex, 40 characters) and,
// while it sets the master up, a "." and 16 random characters after it.
const controlPathMax = 104 - 1 - 1 - 40 - 1 - 16

// muxBases are where the socket directory may be made, in order of
// preference. The first is the operating system's temporary directory, which
// on macOS is too long a path for a socket, so /tmp follows it. A package
// variable so that a test can choose.
var muxBases = func() []string { return []string{os.TempDir(), "/tmp"} }

// runMuxExit asks one master to exit. A package variable so that a test can
// see what is asked without an ssh binary.
var runMuxExit = func(args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	_ = cmd.Run()
}

var mux struct {
	sync.Mutex
	made bool
	// dir is the socket directory, or "" when none could be made, in which
	// case nothing is shared and every ssh connects on its own as before.
	dir string
	// masters is every destination a command was sent to, keyed by its
	// arguments joined, holding what ssh needs to name its master again.
	masters map[string][]string
}

// muxDir is the socket directory, made on first use: a fresh directory of
// MkdirTemp's, which is 0700, so no other local user can reach a master
// through it.
func muxDir() string {
	mux.Lock()
	defer mux.Unlock()
	if !mux.made {
		mux.made = true
		mux.dir = makeMuxDir()
	}
	return mux.dir
}

func makeMuxDir() string {
	for _, base := range muxBases() {
		// MkdirTemp appends the pattern's prefix and up to ten digits.
		if base == "" || !plainPath(base) || len(filepath.Clean(base))+len("/psns-")+10 > controlPathMax {
			continue
		}
		dir, err := os.MkdirTemp(base, "psns-")
		if err != nil {
			continue
		}
		if len(dir) > controlPathMax || os.Chmod(dir, 0o700) != nil {
			os.Remove(dir)
			continue
		}
		return dir
	}
	return ""
}

// plainPath reports whether path can go in an ssh -o option as it is: ssh
// splits the option's value at whitespace and quotes and expands % and ~ in
// a ControlPath.
func plainPath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/._-+", r):
		default:
			return false
		}
	}
	return true
}

// muxArgs are the options that share a connection, or none when there is no
// socket directory.
func muxArgs() []string {
	dir := muxDir()
	if dir == "" {
		return nil
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + dir + "/%C",
		"-o", "ControlPersist=" + controlPersist,
	}
}

// rememberMaster records that a command is about to be sent to the
// destination at the end of args, so CloseConnections can ask its master to
// exit. args are the options and destination without the command; options
// that do not change the socket's name (key files, timeouts) may be among
// them or not.
func rememberMaster(args []string) {
	mux.Lock()
	defer mux.Unlock()
	if mux.dir == "" {
		return
	}
	if mux.masters == nil {
		mux.masters = map[string][]string{}
	}
	mux.masters[strings.Join(args, "\x00")] = append([]string(nil), args...)
}

// CloseConnections asks every shared connection this process opened to
// close, then removes their sockets' directory. Every command calls it before
// it exits, whatever its outcome. Without it a master would stay open for
// ControlPersist after the process ended. A destination that never became a
// master, or whose master has already gone, makes ssh fail quietly, and that
// is ignored. Calling it again is harmless, and a later ssh makes a fresh
// directory.
func CloseConnections() {
	mux.Lock()
	dir, masters := mux.dir, mux.masters
	mux.made, mux.dir, mux.masters = false, "", nil
	mux.Unlock()
	if dir == "" {
		return
	}
	for _, args := range masters {
		dest := args[len(args)-1]
		opts := args[:len(args)-1]
		exit := append(append([]string(nil), opts...), "-O", "exit", dest)
		runMuxExit(exit)
	}
	os.RemoveAll(dir)
}
