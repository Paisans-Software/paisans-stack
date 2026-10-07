package apply_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// Obviously fake: an ed25519 key whose 32 bytes are all zero, and all one.
const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB bob@example.org"
)

// The argument list is the whole of what the transport decides about a
// connection, so it is pinned exactly.
func TestSSHArgsForASection(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", Port: 2222, PublicKeys: []string{keyA, keyB}}
	got := tr.SSHArgs([]string{"/tmp/k/key-1.pub", "/tmp/k/key-2.pub"}, "uptime")
	want := []string{"-p", "2222", "-o", "IdentitiesOnly=yes", "-i", "/tmp/k/key-1.pub", "-i", "/tmp/k/key-2.pub", "ubuntu@203.0.113.10", "uptime"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ssh args:\n got %q\nwant %q", got, want)
	}
	if d := tr.Describe(); d != "ubuntu@203.0.113.10 port 2222" {
		t.Errorf("Describe() = %q", d)
	}
}

func TestSSHArgsDefaultToPort22(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "vm.example.org", PublicKeys: []string{keyA}}
	got := tr.SSHArgs([]string{"k1"}, "true")
	if got[0] != "-p" || got[1] != "22" {
		t.Fatalf("an undeclared port is not 22: %q", got)
	}
	if d := tr.Describe(); d != "ubuntu@vm.example.org" {
		t.Errorf("Describe() = %q, want no port when it is 22", d)
	}
}

// --ssh is an escape hatch: given verbatim, with nothing of the section added.
func TestSSHArgsDestinationOverrideIsVerbatim(t *testing.T) {
	tr := apply.SSHTransport{Destination: "jump-alias", User: "ubuntu", Host: "203.0.113.10", Port: 2222, PublicKeys: []string{keyA}}
	if got, want := tr.SSHArgs(nil, "true"), []string{"jump-alias", "true"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("override args: got %q, want %q", got, want)
	}
	args, cleanup, err := apply.SSHCommand(tr, "true")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if want := []string{"ssh", "jump-alias", "true"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("override command: got %q, want %q", args, want)
	}
}

// The key files exist, private, for as long as the command needs them, and
// not after.
func TestSSHCommandWritesPrivateKeyFilesAndRemovesThem(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", PublicKeys: []string{keyA, keyB}}
	args, cleanup, err := apply.SSHCommand(tr, "true")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for i, a := range args {
		if a == "-i" {
			files = append(files, args[i+1])
		}
	}
	if len(files) != 2 {
		t.Fatalf("want one -i per key, got %q", args)
	}
	dir := filepath.Dir(files[0])
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("key directory mode %o, want 700", info.Mode().Perm())
	}
	for i, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o, want 600", f, info.Mode().Perm())
		}
		body, _ := os.ReadFile(f)
		if want := []string{keyA, keyB}[i] + "\n"; string(body) != want {
			t.Errorf("%s holds %q, want %q", f, body, want)
		}
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("key directory survives cleanup: %v", err)
	}
}

func TestSSHCommandRefusesASectionWithoutKeys(t *testing.T) {
	_, _, err := apply.SSHCommand(apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10"}, "true")
	if err == nil || !strings.Contains(err.Error(), "no public key") {
		t.Fatalf("want a refusal naming the missing key, got %v", err)
	}
}
