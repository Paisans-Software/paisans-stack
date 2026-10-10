package config_test

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

func problemOf(t *testing.T, err error) *ui.Problem {
	t.Helper()
	var p *ui.Problem
	if !errors.As(err, &p) {
		t.Fatalf("not a ui.Problem: %v", err)
	}
	return p
}

// A configuration that is not there says so, and what to do.
func TestMissingConfigIsAProblem(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "paisans.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("not fs.ErrNotExist: %v", err)
	}
	p := problemOf(t, err)
	if !strings.HasSuffix(p.Hint, "paisans.yaml does not exist") || !strings.Contains(p.Explain, "--config") {
		t.Errorf("got %#v", p)
	}
}

// A key the schema does not have is named as a key, with its line, rather
// than as a Go type's field.
func TestUnknownKeyIsAProblem(t *testing.T) {
	_, err := config.Load(write(t, minimal+"trusted_proxies: [10.0.0.0/8]\n"))
	p := problemOf(t, err)
	if !strings.HasSuffix(p.Hint, "paisans.yaml cannot be read as a configuration") {
		t.Errorf("hint: %q", p.Hint)
	}
	if !strings.Contains(p.Explain, "unknown key trusted_proxies") || strings.Contains(p.Explain, "config.Config") {
		t.Errorf("explanation: %q", p.Explain)
	}
	if !strings.Contains(p.Explain, "line ") {
		t.Errorf("the explanation does not give the line: %q", p.Explain)
	}
}

// YAML that does not parse says where.
func TestBrokenYAMLIsAProblem(t *testing.T) {
	_, err := config.Load(write(t, "version: 1\nsites: [\n"))
	p := problemOf(t, err)
	if !strings.Contains(p.Explain, "line") {
		t.Errorf("explanation: %q", p.Explain)
	}
}

// Every structural problem is one item of the explanation, and the
// LoadError stays for a caller that asks for it.
func TestStructuralProblemsAreAList(t *testing.T) {
	body := strings.Replace(minimal, "version: 1", "version: 2", 1)
	body = strings.Replace(body, "  domain: example.org\n", "", 1)
	_, err := config.Load(write(t, body))
	var le *config.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("not a LoadError: %v", err)
	}
	p := problemOf(t, err)
	if !strings.HasSuffix(p.Hint, "paisans.yaml is not a usable configuration") {
		t.Errorf("hint: %q", p.Hint)
	}
	items := 0
	for _, line := range strings.Split(p.Explain, "\n") {
		if strings.HasPrefix(line, "- ") {
			items++
		}
	}
	if items != len(le.Problems) || items < 2 {
		t.Errorf("%d items for %d problems:\n%s", items, len(le.Problems), p.Explain)
	}
}
