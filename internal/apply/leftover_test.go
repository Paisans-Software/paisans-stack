package apply_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

func manifestOf(t *testing.T, host *fakeHost) map[string]render.ManifestFile {
	t.Helper()
	var m render.Manifest
	if err := json.Unmarshal([]byte(host.files["/srv/paisans/f2a9/.paisans-manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]render.ManifestFile{}
	for _, f := range m.Files {
		out[f.Path] = f
	}
	return out
}

func withoutDocs(c *config.Config) { delete(c.Apps, "docs") }

func applyWhole(t *testing.T, host *fakeHost, rendered *render.Plan, opts ...apply.Option) *apply.Plan {
	t.Helper()
	p, err := apply.Build("home-a", rendered, acmeModule(t), host, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	return p
}

// An app taken out of the configuration leaves its files on the host. A
// whole apply keeps their entries in the manifest, marked left over with the
// time it first saw them so, and deletes nothing. A later whole apply keeps
// that first time, and putting the app back clears the mark.
func TestAWholeApplyKeepsLeftoverFilesRecorded(t *testing.T) {
	first := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	defer apply.SetNow(func() time.Time { return first })()

	host := newHost()
	applyWhole(t, host, plan(t))
	const docsEnv = "srv/paisans/f2a9/docs/.env"
	if _, ok := manifestOf(t, host)[docsEnv]; !ok {
		t.Fatalf("the fixture's docs app wrote no %s", docsEnv)
	}

	p := applyWhole(t, host, planWith(t, withoutDocs))
	var listed bool
	for _, l := range p.Leftovers {
		if l.Path == docsEnv {
			listed = true
		}
		if !strings.HasPrefix(l.Path, "srv/paisans/f2a9/docs/") {
			t.Errorf("%s is listed left over, and the docs app is the only one removed", l.Path)
		}
	}
	if !listed {
		t.Errorf("the plan lists no left over %s: %+v", docsEnv, p.Leftovers)
	}
	entry := manifestOf(t, host)[docsEnv]
	if !entry.Leftover || entry.LeftoverSince != "2026-10-01T12:00:00Z" || entry.SHA256 == "" {
		t.Errorf("the docs app's entry after a whole apply = %+v, want it kept and marked left over", entry)
	}
	if _, ok := host.files["/"+docsEnv]; !ok {
		t.Error("apply deleted a left over file")
	}
	if manifestOf(t, host)["srv/paisans/f2a9/talk/.env"].Leftover {
		t.Error("a file still rendered is marked left over")
	}

	defer apply.SetNow(func() time.Time { return first.Add(48 * time.Hour) })()
	applyWhole(t, host, planWith(t, withoutDocs))
	if got := manifestOf(t, host)[docsEnv].LeftoverSince; got != "2026-10-01T12:00:00Z" {
		t.Errorf("a second whole apply moved the time it was first left over to %s", got)
	}

	back, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Conflicts()) != 0 || len(back.Leftovers) != 0 {
		t.Fatalf("putting the app back found conflicts %v and left overs %v", back.Conflicts(), back.Leftovers)
	}
	if err := apply.Execute(back, host); err != nil {
		t.Fatal(err)
	}
	for path, entry := range manifestOf(t, host) {
		if entry.Leftover || entry.LeftoverSince != "" {
			t.Errorf("%s is still marked left over after the app was rendered again", path)
		}
	}
}

// A scoped or partial apply marks nothing: every entry outside what it
// writes stays exactly as the manifest had it.
func TestAPartialApplyMarksNothingLeftOver(t *testing.T) {
	host := newHost()
	applyWhole(t, host, plan(t))
	before := manifestOf(t, host)

	p := applyWhole(t, host, planWith(t, withoutDocs), apply.Only("talk"), apply.Recreate("talk"))
	if len(p.Leftovers) != 0 {
		t.Errorf("an --only plan lists left overs: %v", p.Leftovers)
	}
	after := manifestOf(t, host)
	for path, entry := range before {
		if after[path] != entry {
			t.Errorf("%s changed in the manifest under --only talk: %+v, was %+v", path, after[path], entry)
		}
	}

	scoped, err := apply.Build("home-a", planWith(t, withoutDocs), acmeModule(t), host, apply.Scope("/srv/paisans/f2a9/talk/.env"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Leftovers) != 0 {
		t.Errorf("a scoped plan lists left overs: %v", scoped.Leftovers)
	}
	if err := apply.Execute(scoped, host); err != nil {
		t.Fatal(err)
	}
	for path, entry := range manifestOf(t, host) {
		if entry.Leftover {
			t.Errorf("a scoped apply marked %s left over", path)
		}
	}
	if len(manifestOf(t, host)) != len(before) {
		t.Error("a scoped apply dropped or added manifest entries")
	}
}
