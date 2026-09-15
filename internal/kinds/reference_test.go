package kinds_test

import (
	"testing"

	"github.com/josephquigley/paisans-stack/internal/config"
	"github.com/josephquigley/paisans-stack/internal/kinds"
)

// A reference pins when two runs of apply would fetch the same bytes. The
// cases that matter are the ones that look pinned and are not.
func TestPinning(t *testing.T) {
	cases := []struct {
		ref    string
		pinned bool
	}{
		{"ghcr.io/mbinorg/mbin:v1.10.1", true},
		{"outlinewiki/outline:1.10.0", true},
		{"ghcr.io/mbinorg/mbin@sha256:" + hex64, true},
		{"registry.example.org:5000/org/app:v1.2.3", true},
		{"ghcr.io/mbinorg/mbin:latest", false},
		{"ghcr.io/mbinorg/mbin", false},
		// A registry port is not a tag, so this names no build.
		{"registry.example.org:5000/org/app", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			ref := kinds.ParseReference(tc.ref)
			if got := ref.Pinned(); got != tc.pinned {
				t.Fatalf("ParseReference(%q).Pinned() = %v, want %v (parsed as %+v)", tc.ref, got, tc.pinned, ref)
			}
			if tc.pinned && ref.Floating() != "" {
				t.Fatalf("a pinned reference explained itself as floating: %q", ref.Floating())
			}
			if !tc.pinned && ref.Floating() == "" {
				t.Fatal("a floating reference gave no reason, so the refusal would say nothing")
			}
		})
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Every kind ships a default for its application service, because an adopter
// who never opens the images stanza has to get a tested set. None of those
// defaults may float, or the toolkit would refuse configuration it produces
// itself.
func TestDefaultsArePinned(t *testing.T) {
	for _, kind := range []config.Kind{
		config.KindMbin, config.KindOutline, config.KindPocketID,
		config.KindSynapse, config.KindWriteFreely,
	} {
		ref, ok := kinds.DefaultImage(kind, "app")
		if !ok {
			t.Errorf("%s ships no default image for its app service", kind)
			continue
		}
		if !kinds.ParseReference(ref).Pinned() {
			t.Errorf("%s defaults to %q, which does not name one build", kind, ref)
		}
	}
}

// The database default is computed from cluster.postgres_version rather than
// fixed, so every database in a deployment is one major version.
func TestPostgresHasNoFixedDefault(t *testing.T) {
	if _, ok := kinds.DefaultImage(config.KindMbin, kinds.PostgresService); ok {
		t.Fatal("postgres ships a fixed default, which would let two apps disagree about the major version")
	}
	if !kinds.Has(config.KindMbin, kinds.PostgresService) {
		t.Fatal("postgres is not a declarable service, so a pinned app could not choose its database image")
	}
}
