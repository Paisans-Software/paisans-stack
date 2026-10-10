package apply_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// TestMain closes whatever socket directory the package's tests made, without
// running ssh to do it.
func TestMain(m *testing.M) {
	var exits [][]string
	restore := apply.SetMux([]string{os.TempDir(), "/tmp"}, &exits)
	code := m.Run()
	restore()
	os.Exit(code)
}

// Every ssh is given a socket to share its connection through, in a private
// directory short enough for a unix socket path.
func TestSSHArgsShareTheConnection(t *testing.T) {
	var exits [][]string
	t.Cleanup(apply.SetMux([]string{shortBase(t)}, &exits))
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", PublicKeys: []string{keyA}}
	args := tr.SSHArgs([]string{"k1"}, "true")
	if len(args) < 6 || args[0] != "-o" || args[1] != "ControlMaster=auto" || args[4] != "-o" || args[5] != "ControlPersist=60" {
		t.Fatalf("no connection sharing options first: %q", args)
	}
	path, ok := strings.CutPrefix(args[3], "ControlPath=")
	if !ok || filepath.Base(path) != "%C" {
		t.Fatalf("ControlPath is not a directory's %%C: %q", args[3])
	}
	dir := filepath.Dir(path)
	if len(dir) > apply.ControlPathMax {
		t.Errorf("socket directory %q is %d bytes, over %d", dir, len(dir), apply.ControlPathMax)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode %v, want a 0700 directory", info.Mode())
	}
	// One directory for the whole process, and the --ssh route shares it.
	other := apply.SSHTransport{Destination: "jump-alias"}.SSHArgs(nil, "true")
	if other[3] != args[3] {
		t.Errorf("two directories in one process: %q and %q", args[3], other[3])
	}
}

// The longest socket path ssh makes in the directory fits macOS's 104 bytes.
func TestControlPathFitsAUnixSocket(t *testing.T) {
	// "/", a 40 character %C, then ssh's "." and 16 characters while it binds.
	if n := apply.ControlPathMax + 1 + 40 + 1 + 16; n+1 > 104 {
		t.Fatalf("socket path %d bytes and its NUL do not fit 104", n)
	}
}

// A base too long for a socket path, or one ssh would misread, is passed
// over for the next; with none usable, nothing is shared and ssh connects
// as it always did.
func TestTheSocketDirectoryFallsBack(t *testing.T) {
	short := shortBase(t)
	long := "/" + strings.Repeat("x", apply.ControlPathMax)
	var exits [][]string
	t.Cleanup(apply.SetMux([]string{long, "/tmp/has space", "/tmp/~tilde", "/tmp/50%", short}, &exits))
	args := apply.MuxArgs()
	if len(args) != 6 || !strings.HasPrefix(args[3], "ControlPath="+short+"/psns-") {
		t.Fatalf("want the short base, got %q", args)
	}

	t.Cleanup(apply.SetMux([]string{long}, &exits))
	if args := apply.MuxArgs(); args != nil {
		t.Fatalf("want no sharing without a usable base, got %q", args)
	}
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10"}
	if got, want := tr.SSHArgs([]string{"k1"}, "true"), []string{"-p", "22", "-o", "IdentitiesOnly=yes", "-i", "k1", "ubuntu@203.0.113.10", apply.RemoteCommand("true")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unshared args:\n got %q\nwant %q", got, want)
	}
}

// CloseConnections asks each destination's master to exit, once each, named
// as it was opened but without the key files, then removes the directory.
func TestCloseConnectionsExitsEveryMasterAndRemovesTheDirectory(t *testing.T) {
	var exits [][]string
	t.Cleanup(apply.SetMux([]string{shortBase(t)}, &exits))
	(&sshScript{answers: []sshAnswer{{out: "ok\n"}}}).install(t)
	section := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", Port: 2222, PublicKeys: []string{keyA}}
	escape := apply.SSHTransport{Destination: "ssh://admin@[2001:db8::1]:2200"}
	for _, tr := range []apply.SSHTransport{section, escape, section} {
		if _, err := tr.Run("true"); err != nil {
			t.Fatal(err)
		}
	}
	mux := apply.MuxArgs()
	dir := filepath.Dir(strings.TrimPrefix(mux[3], "ControlPath="))
	apply.CloseConnections()

	want := map[string]bool{
		strings.Join(append(append([]string(nil), mux...), "-p", "2222", "-o", "IdentitiesOnly=yes", "-O", "exit", "ubuntu@203.0.113.10"), " "): true,
		strings.Join(append(append([]string(nil), mux...), "-O", "exit", "ssh://admin@[2001:db8::1]:2200"), " "):                                true,
	}
	if len(exits) != len(want) {
		t.Fatalf("want %d exits, got %q", len(want), exits)
	}
	for _, e := range exits {
		if !want[strings.Join(e, " ")] {
			t.Errorf("unexpected exit %q", e)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("socket directory survives CloseConnections: %v", err)
	}

	// Closing again asks nothing, and a later ssh gets a fresh directory.
	exits = nil
	apply.CloseConnections()
	if len(exits) != 0 {
		t.Errorf("second close asked %q", exits)
	}
	if again := apply.MuxArgs(); again == nil || again[3] == mux[3] {
		t.Errorf("want a fresh directory after closing, got %q", again)
	}
}

// Nothing ever shared means nothing to ask: no directory, no ssh.
func TestCloseConnectionsWithoutSharingDoesNothing(t *testing.T) {
	var exits [][]string
	t.Cleanup(apply.SetMux(nil, &exits))
	apply.CloseConnections()
	if len(exits) != 0 {
		t.Fatalf("asked %q", exits)
	}
}

// shortBase is a directory short enough to hold the sockets: t.TempDir is
// under $TMPDIR, which on macOS is too long.
func shortBase(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "psnst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
