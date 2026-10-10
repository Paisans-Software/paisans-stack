package siteremove

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The commands that clean around a kept Caddy, run by a real shell against
// a directory tree under a temporary root, so what they keep is what sh and
// find do rather than what a fake says they do.

const shellID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"

func shellTree(t *testing.T, files map[string]string, dirs, links []string) (base string) {
	t.Helper()
	base = t.TempDir()
	for rel, content := range files {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range dirs {
		if err := os.MkdirAll(filepath.Join(base, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i+1 < len(links); i += 2 {
		p := filepath.Join(base, links[i])
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, links[i+1]), p); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func runUnder(t *testing.T, base, command string) error {
	t.Helper()
	command = strings.ReplaceAll(command, "'/srv/paisans/", "'"+base+"/srv/paisans/")
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Logf("%s: %v: %s", command, err, out)
	}
	return err
}

func exists(base, rel string) bool {
	_, err := os.Lstat(filepath.Join(base, rel))
	return err == nil
}

func shellPlan() *Plan { return &Plan{Site: "vm", cfg: &config.Config{ID: shellID}} }

const r = "srv/paisans/f2a9/"

func TestDeleteBesideKeptKeepsCaddysPathsAlone(t *testing.T) {
	base := shellTree(t, map[string]string{
		r + ".paisans-manifest.json":          "{}",
		r + "infra/compose.yaml":              "services: {}",
		r + "infra/caddy/Caddyfile":           "{\n}\n",
		r + "infra/caddy/caddy.env":           "TOKEN=x",
		r + "infra/caddy/data/caddy/cert.crt": "CERT",
		r + "infra/caddy/snippets/talk.caddy": "route",
		r + "infra/caddy/snippets/.staged":    "x",
		r + "infra/caddy/other":               "x",
		r + "infra/patroni.env":               "x",
		r + "infra/postgres/PG_VERSION":       "18",
		r + "talk/.env":                       "x",
		r + ".hidden":                         "x",
		"outside/keep.txt":                    "owner",
		"srv/caddy.d/blog.caddy":              "blog",
	}, []string{r + "infra/caddy/config"}, []string{r + "link", "outside"})
	if err := runUnder(t, base, shellPlan().deleteBesideKept()); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".paisans-manifest.json", "infra/compose.yaml", "infra/caddy/Caddyfile", "infra/caddy/caddy.env", "infra/caddy/data/caddy/cert.crt", "infra/caddy/config", "infra/caddy/snippets"} {
		if !exists(base, r+rel) {
			t.Errorf("%s was deleted", rel)
		}
	}
	for _, rel := range []string{"infra/caddy/snippets/talk.caddy", "infra/caddy/snippets/.staged", "infra/caddy/other", "infra/patroni.env", "infra/postgres", "talk", ".hidden", "link"} {
		if exists(base, r+rel) {
			t.Errorf("%s is left", rel)
		}
	}
	for _, rel := range []string{"outside/keep.txt", "srv/caddy.d/blog.caddy"} {
		if !exists(base, rel) {
			t.Errorf("%s, outside the root, was deleted", rel)
		}
	}
}

// A directory the loops walk that is a symbolic link would take the loop
// outside the root: it stops before deleting anything.
func TestDeleteBesideKeptRefusesASymlinkedDirectory(t *testing.T) {
	base := shellTree(t, map[string]string{
		r + "infra/caddy/Caddyfile": "{\n}\n",
		r + "talk/.env":             "x",
		"srv/caddy.d/blog.caddy":    "blog",
	}, nil, []string{r + "infra/caddy/snippets", "srv/caddy.d"})
	if err := runUnder(t, base, shellPlan().deleteBesideKept()); err == nil {
		t.Error("a symlinked snippets directory was walked")
	}
	if !exists(base, "srv/caddy.d/blog.caddy") {
		t.Error("the owner's site was deleted through the link")
	}
	if !exists(base, r+"talk/.env") {
		t.Error("something was deleted before the refusal")
	}
}

func TestEmptyBesideKeptKeepsCaddysEmptyDirectories(t *testing.T) {
	base := shellTree(t, map[string]string{
		r + "infra/caddy/Caddyfile": "{\n}\n",
		r + "postgres/PG_VERSION":   "18",
	}, []string{r + "infra/caddy/config", r + "infra/caddy/snippets", r + "infra/caddy/data/caddy/locks", r + "talk/empty", r + "infra/etcd"}, nil)
	if err := runUnder(t, base, shellPlan().emptyBesideKept()); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"infra/caddy/config", "infra/caddy/snippets", "infra/caddy/data/caddy/locks", "postgres/PG_VERSION"} {
		if !exists(base, r+rel) {
			t.Errorf("%s was deleted", rel)
		}
	}
	for _, rel := range []string{"talk", "infra/etcd"} {
		if exists(base, r+rel) {
			t.Errorf("%s is left", rel)
		}
	}
}

// keptProbe needs GNU find's -printf, which a host has; it is skipped where
// find has none.
func TestKeptProbeListsWhatIsNotCaddys(t *testing.T) {
	if exec.Command("sh", "-c", "find . -maxdepth 0 -printf ''").Run() != nil {
		t.Skip("find has no -printf here")
	}
	base := shellTree(t, map[string]string{
		r + "infra/caddy/Caddyfile":           "{\n}\n",
		r + "infra/caddy/data/caddy/c.crt":    "CERT",
		r + "infra/caddy/snippets/talk.caddy": "route",
		r + "talk/.env":                       "x",
	}, []string{r + "postgres/empty"}, nil)
	p := shellPlan()
	command := strings.ReplaceAll(p.keptProbe(), "'/srv/paisans/", "'"+base+"/srv/paisans/")
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	answer := strings.ReplaceAll(string(out), base, "")
	others, empty, files := p.parseKeptProbe(answer)
	got := strings.Join(others, ",") + " | " + strings.Join(empty, ",") + " | " + strings.Join(files, ",")
	for _, want := range []string{"/srv/paisans/f2a9/talk/.env", "/srv/paisans/f2a9/postgres/empty"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s in %s", want, got)
		}
	}
	for _, not := range []string{"Caddyfile", "c.crt", "talk.caddy", "f2a9/infra,", "f2a9/infra/caddy,"} {
		if strings.Contains(got, not) {
			t.Errorf("%s in %s", not, got)
		}
	}
}
