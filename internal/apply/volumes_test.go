package apply_test

import (
	"strings"
	"testing"
	"time"

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

// After a recreate's health gate passes, the anonymous volumes its previous
// containers mounted and nothing mounts now are removed, so an image that
// slipped past the guard still does not leak. A named volume is never a
// candidate, and an anonymous one the new containers took over is not
// dangling, so it stays.
func TestARecreateRemovesTheVolumesItsOldContainersLeft(t *testing.T) {
	const (
		left  = "3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191"
		taken = "4e69f2bcb465d8db8ce062bb7928930be85a8324d7829d34be34fc3b5b4bf68f"
		other = "8cce176c65a4f3a4a255ca46dc4588b38d917bffc8f0c13fd4d5335d4fc8f830"
	)
	host := newHost()
	host.stackVolumes = map[string]string{"talk": left + "\n" + taken + "\npaisans-talk_named\n"}
	host.dangling = left + "\n" + other + "\npaisans-talk_named\n"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if len(host.removedVolumes) != 1 || host.removedVolumes[0] != left {
		t.Errorf("removed %v, want only %s: not the one the new containers mount, not another stack's, not a named one", host.removedVolumes, left)
	}
	// The record is taken before the recreate, and the removal after the
	// health gate.
	var record, up, health, rm int
	for i, c := range host.commands {
		switch {
		case strings.Contains(c, "/srv/talk/compose.yaml ps -aq"):
			record = i
		case c == "docker compose -f /srv/talk/compose.yaml up -d":
			up = i
		case strings.Contains(c, "/srv/talk/compose.yaml ps --all --format json"):
			health = i
		case strings.HasPrefix(c, "docker volume rm "):
			rm = i
		}
	}
	if !(record < up && up < health && health < rm) {
		t.Errorf("order is record %d, up %d, health %d, rm %d", record, up, health, rm)
	}
}

// A stack that fails its health gate keeps its old volumes: the apply stops
// there, and what the old containers held may be wanted.
func TestAnUnhealthyRecreateRemovesNoVolumes(t *testing.T) {
	restore := apply.SetHealthWait(0, 1, func(time.Duration) {})
	defer restore()
	host := newHost()
	host.stackVolumes = map[string]string{"talk": "3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191\n"}
	host.dangling = "3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191\n"
	host.ps = map[string]string{"talk": `{"Service":"app","Name":"talk-app-1","State":"exited","Health":""}`}
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err == nil {
		t.Fatal("an exited container passed the health gate")
	}
	if len(host.removedVolumes) != 0 {
		t.Errorf("an unhealthy stack's old volumes were removed: %v", host.removedVolumes)
	}
}
