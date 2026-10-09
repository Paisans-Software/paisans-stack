package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
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
