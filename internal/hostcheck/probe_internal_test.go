package hostcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// hostSites runs the host sites probe against dir in place of
// render.HostSitesDir.
func hostSites(t *testing.T, dir string) (string, error) {
	t.Helper()
	out, err := exec.Command("sh", "-c", strings.ReplaceAll(hostSitesProbe, render.HostSitesDir, dir)).CombinedOutput()
	return string(out), err
}

// A site file is listed even when it is a symbolic link whose target is
// gone: the owner's Caddy still names it, and a Caddy that serves one is
// kept rather than removed.
func TestHostSitesProbeListsABrokenLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.caddy"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "b.caddy")); err != nil {
		t.Fatal(err)
	}
	out, err := hostSites(t, dir)
	if err != nil || !strings.Contains(out, "a.caddy") || !strings.Contains(out, "b.caddy") {
		t.Errorf("err %v, out %q", err, out)
	}
}

// A directory that is there and cannot be read is an error, never an empty
// listing: read as empty, a Caddy serving the owner's sites would be removed.
func TestHostSitesProbeRefusesAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.caddy"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if out, err := hostSites(t, dir); err == nil {
		t.Errorf("an unreadable directory listed as %q", out)
	}
}

// A host with no such directory has no sites.
func TestHostSitesProbeWithoutTheDirectory(t *testing.T) {
	out, err := hostSites(t, filepath.Join(t.TempDir(), "absent"))
	if err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("err %v, out %q", err, out)
	}
}
