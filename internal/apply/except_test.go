package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// A held stack's files are neither compared nor written and its action does
// not run, while the mesh and every other stack apply as usual. The next
// whole apply sees the held stack's files as a first write, not a conflict.
func TestExceptHoldsAStackBackAndAppliesTheRest(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Changes {
		if c.Stack == "talk" {
			t.Errorf("talk is held back, yet %s is planned", c.Path)
		}
	}
	for _, a := range p.Actions {
		if a.Stack == "talk" {
			t.Errorf("talk is held back, yet it is acted on")
		}
	}
	if p.WireGuard == apply.WireGuardNone {
		t.Error("holding an app back held the mesh back too")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("/srv/paisans/f2a9/talk/compose.yaml") {
		t.Error("talk's stack was acted on")
	}
	if _, ok := host.files["/srv/paisans/f2a9/talk/.env"]; ok {
		t.Error("talk's files were written")
	}

	whole, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Conflicts()) != 0 {
		t.Fatalf("the whole apply after a held one sees conflicts: %v", whole.Conflicts())
	}
	if len(whole.Actions) != 1 || whole.Actions[0].Stack != "talk" {
		t.Errorf("the whole apply after a held one plans %v, want talk alone", whole.Actions)
	}
}

// A held stack a stopped apply still owes stays owed, so the apply that
// releases it force-recreates it.
func TestExceptKeepsAHeldStackOwed(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/paisans/f2a9/.paisans-pending.json"] = `{"version":1,"actions":[{"stack":"docs","recreate":true},{"stack":"talk","recreate":true}]}`
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	record, ok := host.files["/srv/paisans/f2a9/.paisans-pending.json"]
	if !ok || !strings.Contains(record, `"talk"`) || strings.Contains(record, `"docs"`) {
		t.Fatalf("after holding talk back the record of owed actions is %q, want talk alone", record)
	}
	next, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Actions) != 1 || next.Actions[0].Stack != "talk" || !next.Actions[0].Force {
		t.Errorf("releasing talk plans %+v, want talk force-recreated", next.Actions)
	}
}

// A later pass of the same apply, told what the earlier one did with After,
// does not repeat an --overwrite or a --recreate the earlier pass already
// carried out, and still carries out those it held back.
func TestAfterDoesNotRepeatAnOverwriteOrARecreate(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/paisans/f2a9/infra/compose.yaml"] += "# edited on the host\n"
	host.files["/srv/paisans/f2a9/talk/.env"] += "# edited on the host\n"
	options := []apply.Option{
		apply.Overwrite("/srv/paisans/f2a9/infra/compose.yaml", "/srv/paisans/f2a9/talk/.env"),
		apply.Recreate("infra", "talk"),
	}
	first, err := apply.Build("home-a", plan(t), acmeModule(t), host, append(options, apply.Except("talk"))...)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	second, err := apply.Build("home-a", plan(t), acmeModule(t), host, append(options, apply.After(first))...)
	if err != nil {
		t.Fatalf("the second pass refused what the first one did: %v", err)
	}
	if got := strings.Join(stacks(second), ","); got != "talk" {
		t.Errorf("the second pass acts on %s, want talk alone", got)
	}
	if a, ok := actionOn(second, "talk"); !ok || !a.Force {
		t.Errorf("talk, named with --recreate and held back, is planned as %+v", a)
	}
	overwritten := false
	for _, c := range second.Changes {
		if c.Path == "/srv/paisans/f2a9/talk/.env" && c.Overwritten {
			overwritten = true
		}
		if c.Path == "/srv/paisans/f2a9/infra/compose.yaml" && c.Kind != apply.Unchanged {
			t.Errorf("the second pass plans infra's compose file again: %v", c.Kind)
		}
	}
	if !overwritten {
		t.Error("talk's held back --overwrite was dropped")
	}
}

// An app held back while HAProxy or Patroni moves under it still owes the
// restart that gives it fresh database connections, and gets it exactly once
// when it is released.
func TestADatabasePathMoveRestartsAHeldAppOnceItIsReleased(t *testing.T) {
	host := applied(t, "home-a")
	changed := planChanging(t, "srv/paisans/f2a9/infra/haproxy/haproxy.cfg")
	first, err := apply.Build("home-a", changed, acmeModule(t), host, databaseApps(t), apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := actionOn(first, "talk"); ok {
		t.Fatal("a held app is restarted")
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("/srv/paisans/f2a9/talk/compose.yaml") {
		t.Error("a held app was acted on")
	}
	if record := host.files["/srv/paisans/f2a9/.paisans-pending.json"]; !strings.Contains(record, `"talk"`) {
		t.Fatalf("the held restart is not owed: %q", record)
	}

	host.commands = nil
	second, err := apply.Build("home-a", changed, acmeModule(t), host, databaseApps(t), apply.After(first))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(second), ","); got != "talk" {
		t.Fatalf("releasing talk acts on %s, want talk alone", got)
	}
	if a, _ := actionOn(second, "talk"); a.Recreate || a.Force || !strings.Contains(a.Reason, "database path") {
		t.Errorf("talk is planned as %+v, want a restart for its database path", a)
	}
	if err := apply.Execute(second, host); err != nil {
		t.Fatal(err)
	}
	if n := countRan(host, "/srv/paisans/f2a9/talk/compose.yaml restart"); n != 1 {
		t.Errorf("talk was restarted %d time(s)", n)
	}
	if _, owed := host.files["/srv/paisans/f2a9/.paisans-pending.json"]; owed {
		t.Error("the restart is still owed after it ran")
	}
}

// An app held back stays held back through a database path move in a later
// pass, and its restart stays owed until it is released.
func TestAHeldAppKeepsItsOwedRestartWhileHeld(t *testing.T) {
	host := applied(t, "home-a")
	changed := planChanging(t, "srv/paisans/f2a9/infra/haproxy/haproxy.cfg")
	first, err := apply.Build("home-a", changed, acmeModule(t), host, databaseApps(t), apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	second, err := apply.Build("home-a", changed, acmeModule(t), host, databaseApps(t), apply.After(first), apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(second, host); err != nil {
		t.Fatal(err)
	}
	if record := host.files["/srv/paisans/f2a9/.paisans-pending.json"]; !strings.Contains(record, `"talk"`) {
		t.Fatalf("a still held app lost its owed restart: %q", record)
	}
}

// The images a held stack renders are the site's as much as any other's, so
// pruning a stack that shares their repository keeps them.
func TestPruneKeepsTheImagesAHeldStackRenders(t *testing.T) {
	host := supersededHost()
	rendered := plan(t)
	for i, file := range rendered.Files {
		if file.Path == "home-a/srv/paisans/f2a9/docs/compose.yaml" {
			rendered.Files[i].Content = strings.Replace(file.Content, "outlinewiki/outline:1.10.0", "ghcr.io/example-org/mbin:v1.9.0", 1)
		}
	}
	p, err := apply.Build("home-a", rendered, acmeModule(t), host, apply.Except("docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, prune := range p.Prunes {
		if prune.Ref == "ghcr.io/example-org/mbin:v1.9.0" {
			t.Error("an image the held docs stack renders is planned for pruning")
		}
	}
}

func countRan(h *fakeHost, sub string) int {
	n := 0
	for _, c := range h.commands {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// Refusal is what Execute refuses before writing anything, so a caller that
// runs several passes can ask it of the whole plan before the first one.
func TestRefusalNamesAConflictBeforeAnythingIsWritten(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/paisans/f2a9/talk/compose.yaml"] += "# edited on the host\n"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Refusal(p); err == nil || !strings.Contains(err.Error(), "/srv/paisans/f2a9/talk/compose.yaml") {
		t.Errorf("Refusal gave %v", err)
	}
	host.commands = nil
	if err := apply.Execute(p, host); err == nil {
		t.Fatal("Execute applied a plan with a conflict")
	}
	if len(host.commands) != 0 {
		t.Errorf("Execute ran %v before refusing", host.commands)
	}
}

// An operator's --recreate aimed at a held stack is owed rather than dropped,
// and a held --overwrite is reported rather than silently left undone.
func TestAHeldStackKeepsTheOperatorsRecreateAndReportsItsOverwrite(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/paisans/f2a9/talk/.env"] += "# edited on the host\n"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Overwrite("/srv/paisans/f2a9/talk/.env"), apply.Recreate("talk", "docs"), apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.HeldOverwrites, ",") != "/srv/paisans/f2a9/talk/.env" {
		t.Errorf("held overwrites %v", p.HeldOverwrites)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if record := host.files["/srv/paisans/f2a9/.paisans-pending.json"]; !strings.Contains(record, `"talk"`) {
		t.Fatalf("the held --recreate is not owed: %q", record)
	}
	next, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Overwrite("/srv/paisans/f2a9/talk/.env"))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionOn(next, "talk"); !ok || !a.Force {
		t.Errorf("releasing talk plans %+v, want it force-recreated", a)
	}
}

// An --only run leaves what a stopped apply owes outside its stacks owed,
// database path restarts included.
func TestOnlyKeepsWhatIsOwedOutsideIt(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/paisans/f2a9/.paisans-pending.json"] = `{"version":1,"actions":[{"stack":"docs","recreate":true},{"stack":"talk","database_path":true}]}`
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Only("auth"), apply.Recreate("auth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	record := host.files["/srv/paisans/f2a9/.paisans-pending.json"]
	if !strings.Contains(record, `"docs"`) || !strings.Contains(record, `"database_path": true`) {
		t.Errorf("an --only run dropped what is owed outside it: %q", record)
	}
}

// An owed stack the site no longer renders is dropped with a note, so an app
// removed from the configuration cannot wedge every later apply.
func TestAnOwedStackNoLongerRenderedIsDropped(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/paisans/f2a9/.paisans-pending.json"] = `{"version":1,"actions":[{"stack":"gone","recreate":true}]}`
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := actionOn(p, "gone"); ok {
		t.Error("a stack the site no longer renders is acted on")
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "gone") {
		t.Errorf("notes %v do not mention the dropped stack", p.Notes)
	}
}
