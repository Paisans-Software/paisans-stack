package kinds_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Every kind says whether it can be gated, so a new kind fails here until
// somebody decides, rather than defaulting to a gate it might not survive.
func TestEveryKindHasAGateRecord(t *testing.T) {
	for _, kind := range config.Kinds() {
		if !kinds.GateRecorded(kind) {
			t.Errorf("%s has no gate record", kind)
		}
	}
}

// Every path a gate record names is a Caddy path, and every auto-login target
// too: a target that did not start with / would be read by `redir` as a
// matcher, and the redirect would silently become an empty 200.
func TestGatePathsAreAbsolute(t *testing.T) {
	for _, kind := range config.Kinds() {
		spec := kinds.GateFor(kind)
		paths := slices.Concat(spec.OpenPaths, spec.InboxPaths, spec.TokenPaths)
		for _, rule := range spec.AutoLogin {
			paths = append(paths, rule.Target)
			paths = append(paths, rule.Paths...)
		}
		if spec.SignedFetch != nil {
			paths = append(paths, spec.SignedFetch.Probe)
		}
		for _, p := range paths {
			if !strings.HasPrefix(p, "/") {
				t.Errorf("%s: %q is not an absolute path", kind, p)
			}
		}
	}
}

// A kind that can be gated records nothing that would open it whole, and a
// kind that cannot be gated records nothing at all.
func TestNoGateRecordOpensTheFrontPage(t *testing.T) {
	for _, kind := range config.Kinds() {
		spec := kinds.GateFor(kind)
		open := kinds.OpenPathsFor(kind)
		if slices.Contains(open, "/") || slices.Contains(open, "/*") || slices.Contains(spec.TokenPaths, "/*") {
			t.Errorf("%s opens its whole site past the gate: %v %v", kind, open, spec.TokenPaths)
		}
		if !spec.Gateable && (len(spec.OpenPaths) > 0 || len(spec.TokenPaths) > 0 || len(spec.AutoLogin) > 0 || spec.SignedFetch != nil) {
			t.Errorf("%s cannot be gated, yet records what gets past a gate", kind)
		}
	}
}

// A dedicated health route is open past the gate, so a gated app can be
// checked through the edge; `/` is the front page and never is.
func TestADedicatedHealthRouteIsOpen(t *testing.T) {
	if !slices.Contains(kinds.OpenPathsFor(config.KindOutline), "/_health") {
		t.Error("outline's /_health is not open past the gate")
	}
	if slices.Contains(kinds.OpenPathsFor(config.KindMbin), "/") {
		t.Error("mbin's health route is its front page, and opening it ungates the app")
	}
}

// A federating kind that can be gated records how it refuses an unsigned
// read, because the gate leaves ActivityPub to the app.
func TestAGateableFederatingKindRecordsSignedFetch(t *testing.T) {
	for _, kind := range []config.Kind{config.KindMbin, config.KindWriteFreely} {
		if kinds.GateFor(kind).SignedFetch == nil {
			t.Errorf("%s federates and can be gated, but records no signed fetch", kind)
		}
		if len(kinds.GateFor(kind).InboxPaths) == 0 {
			t.Errorf("%s federates and can be gated, but records no inbox, so deliveries would meet the gate", kind)
		}
	}
}

// WriteFreely checks signatures only in private mode, so one with private
// turned off has no signed fetch, and one with federation off does not
// federate at all.
func TestWriteFreelySignedFetchNeedsPrivateMode(t *testing.T) {
	app := config.App{Kind: config.KindWriteFreely}
	if kinds.SignedFetchFor(app) == nil || !kinds.Federates(app) {
		t.Fatal("a default writefreely has no signed fetch, or does not federate")
	}
	app.Settings = map[string]any{"private": false}
	if kinds.SignedFetchFor(app) != nil {
		t.Error("a writefreely with private off still claims signed fetch")
	}
	app.Settings = map[string]any{"federation": false}
	if kinds.Federates(app) {
		t.Error("a writefreely with federation off still federates")
	}
	probe := kinds.GateFor(config.KindWriteFreely).SignedFetch.SignedFetchProbe("blog.example.org")
	if probe != "/api/collections/blog.example.org" {
		t.Errorf("writefreely probe is %q", probe)
	}
}
