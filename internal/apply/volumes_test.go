package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// The fixture's Mbin image is an operator override, so only the apply time
// check can know what it declares. Declared at the path its template mounts as
// tmpfs, as the real image does, it passes.
func TestADeclaredVolumeTheTemplateMountsPasses(t *testing.T) {
	host := newHost()
	host.volumes = map[string]string{talkImage: `{"/app/var/":{}}`, "docker.io/library/rabbitmq:3.13.7-management-alpine": `{"/var/lib/rabbitmq":{}}`}
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Volumes == nil || len(p.Volumes.Uncovered) != 0 {
		t.Fatalf("covered volumes were reported uncovered: %+v", p.Volumes)
	}
	found := false
	for _, ref := range p.Volumes.Checked {
		found = found || ref == talkImage
	}
	if !found {
		t.Errorf("the Mbin image was not inspected: %v", p.Volumes.Checked)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
}

// An image declaring a path its service does not mount refuses the apply
// before anything is written, and the refusal names the image, the service,
// the path and the fix. Both services running the image are named, since each
// would leak its own volume.
func TestAnUncoveredDeclaredVolumeRefusesBeforeAnyWrite(t *testing.T) {
	host := newHost()
	host.volumes = map[string]string{talkImage: `{"/app/var/":{},"/app/uploads":{}}`}
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Volumes == nil || len(p.Volumes.Uncovered) != 2 {
		t.Fatalf("want app and messenger uncovered at /app/uploads, got %+v", p.Volumes)
	}
	for i, service := range []string{"app", "messenger"} {
		u := p.Volumes.Uncovered[i]
		if u.Stack != "talk" || u.Service != service || u.Image != talkImage || u.Path != "/app/uploads" {
			t.Errorf("uncovered[%d] is %+v", i, u)
		}
	}
	host.commands = nil
	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("an apply went ahead with a declared volume nothing mounts")
	}
	for _, want := range []string{"talk/app runs " + talkImage, "talk/messenger", "VOLUME /app/uploads", "bind under /srv/talk/ for state, a tmpfs for throwaway", "nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if len(host.files) != 0 || len(host.commands) != 0 {
		t.Errorf("a refused apply wrote %d file(s) and ran %v", len(host.files), host.commands)
	}
}

// An image the host does not have yet cannot be inspected by Build. It is
// owed, and Execute pulls it and checks it before the first write.
func TestAnAbsentImageIsPulledAndCheckedBeforeAnyWrite(t *testing.T) {
	host := newHost()
	host.absent = map[string]bool{talkImage: true}
	host.free = 10 << 30
	host.volumes = map[string]string{talkImage: `{"/srv/elsewhere":{}}`}
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Volumes == nil || len(p.Volumes.Owed) != 1 || p.Volumes.Owed[0] != talkImage || len(p.Volumes.Uncovered) != 0 {
		t.Fatalf("the absent image is not owed: %+v", p.Volumes)
	}
	err = apply.Execute(p, host)
	if err == nil || !strings.Contains(err.Error(), "VOLUME /srv/elsewhere") {
		t.Fatalf("a pulled image declaring an unmounted path was not refused: %v", err)
	}
	if len(host.pulled) != 1 || host.pulled[0] != talkImage {
		t.Errorf("pulled %v, want only %s", host.pulled, talkImage)
	}
	if len(host.files) != 0 {
		t.Errorf("a refused apply wrote %d file(s)", len(host.files))
	}

	// The same pull of an image that declares what the template mounts goes
	// on to apply.
	host = newHost()
	host.absent = map[string]bool{talkImage: true}
	host.free = 10 << 30
	host.volumes = map[string]string{talkImage: `{"/app/var/":{}}`}
	p, err = apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if len(host.pulled) != 1 {
		t.Errorf("pulled %v", host.pulled)
	}
}

// Nothing moves, so nothing is inspected: a stack no apply acts on creates no
// container and so cannot make a volume.
func TestAnApplyWithNothingToDoChecksNoVolumes(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	p, err = apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Volumes != nil || host.ran(".Config.Volumes") {
		t.Errorf("an apply with nothing to do inspected volumes: %+v", p.Volumes)
	}
}

// A probe that does not answer for an image is an error, not a pass.
func TestAnUnansweredVolumeProbeIsAnError(t *testing.T) {
	host := newHost()
	host.fail = ".Config.Volumes"
	if _, err := apply.Build("home-a", plan(t), acmeModule(t), host); err == nil {
		t.Error("a failed volume probe was read as nothing declared")
	}
}
