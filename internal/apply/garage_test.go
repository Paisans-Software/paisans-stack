package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// deployedAt rewrites the host's garage.toml as if Garage there had been
// installed at another replication factor, and records it as ours, so the
// only thing different from the render is the factor.
func deployedAt(t *testing.T, host *fakeHost, factor string) {
	t.Helper()
	const path = "/srv/paisans/f2a9/infra/garage/garage.toml"
	current, ok := host.files[path]
	if !ok {
		t.Fatal("the first apply wrote no garage.toml")
	}
	host.files[path] = strings.Replace(current, "replication_factor = 2", "replication_factor = "+factor, 1)
}

// Garage refuses to start when its configuration names a different
// replication factor from the one its stored layout was built with, so apply
// must not write one: it would restart object storage into that refusal.
func TestApplyRefusesToChangeGarageReplication(t *testing.T) {
	host := newHost()
	rendered := plan(t)
	first, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	deployedAt(t, host, "1")

	_, err = apply.Build("home-a", rendered, acmeModule(t), host)
	if err == nil {
		t.Fatal("apply planned a garage.toml at a different replication factor")
	}
	for _, want := range []string{"replication 1", "says 2", "storage add --change-replication"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}

	// storage add's reset is the one caller allowed past it, scoped to the
	// one file.
	scoped, err := apply.Build("home-a", rendered, acmeModule(t), host,
		apply.Scope("srv/paisans/f2a9/infra/garage/garage.toml"), apply.ReplicationChange(), apply.Overwrite("/srv/paisans/f2a9/infra/garage/garage.toml"))
	if err != nil {
		t.Fatalf("the reset could not plan the new garage.toml: %v", err)
	}
	if len(scoped.Writes()) != 1 {
		t.Fatalf("the reset should write exactly garage.toml, got %v", scoped.Writes())
	}
}

// A first install has no deployed file to disagree with.
func TestGarageReplicationIsReadFromTheRenderedFile(t *testing.T) {
	rendered := plan(t)
	for _, f := range rendered.Files {
		if f.Path != "home-a/"+"srv/paisans/f2a9/infra/garage/garage.toml" {
			continue
		}
		n, ok := apply.GarageReplication(f.Content)
		if !ok || n != 2 {
			t.Fatalf("read replication %d (%v) from the rendered garage.toml", n, ok)
		}
		return
	}
	t.Fatal("the fixture renders no garage.toml for home-a")
}
