package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// registryFake is a host's registry, for commands under test: it answers
// the claim and the read, and records which it got.
type registryFake struct {
	claims, reads int
	sudo          bool
}

func (f *registryFake) Run(command string) (string, error) {
	if strings.Contains(command, "flock") && strings.Contains(command, registry.Path) {
		f.claims++
	}
	return "", nil
}

func (f *registryFake) ReadFile(path string) (string, bool, error) {
	if path == registry.Path {
		f.reads++
	}
	return "", false, nil
}

func (f *registryFake) Describe() string { return "registry fake" }

func withRegistryFake(t *testing.T) *registryFake {
	t.Helper()
	fake := &registryFake{}
	saved := registryHost
	registryHost = func(_ string, _ config.Site, _ string, sudo bool) registry.Runner {
		fake.sudo = sudo
		return fake
	}
	t.Cleanup(func() { registryHost = saved })
	return fake
}

// A dry run's registry read is a step, so a spinner shows while ssh answers.
func TestClaimDryRunReadsInAStep(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	rec := &ui.Recorder{}
	one := map[string]registry.Runner{"vm": &registryFake{}}
	if err := claimHosts(rec, cfg, false, one); err != nil {
		t.Fatal(err)
	}
	if !rec.Has("done", "check site's claim") {
		t.Errorf("one site's read is not a step:\n%s", rec.Lines())
	}
	rec = &ui.Recorder{}
	two := map[string]registry.Runner{"vm": &registryFake{}, "home-a": &registryFake{}}
	if err := claimHosts(rec, cfg, false, two); err != nil {
		t.Fatal(err)
	}
	if !rec.Has("done", "check home-a's claim") || !rec.Has("done", "check vm's claim") {
		t.Errorf("each site's read is not a step of its own:\n%s", rec.Lines())
	}
}
