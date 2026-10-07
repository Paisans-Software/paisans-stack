package kinds_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// The Pocket ID image the kind ships has its refusal text recorded. A bump
// changes the reference, the new one has no entry, and this fails until
// somebody reads the new release's bootstrap.go and records what it logs.
func TestDefaultPocketIDImageHasItsStandbyMarker(t *testing.T) {
	image, ok := kinds.DefaultImage(config.KindPocketID, "app")
	if !ok {
		t.Fatal("pocket-id ships no default app image")
	}
	marker, known := kinds.StandbyMarker(image)
	if !known {
		t.Fatalf("pocket-id runs %s, whose refusal text is not in kinds.PocketIDStandbyMarker. Read backend/internal/bootstrap/bootstrap.go at that tag, find what ErrClusterFull becomes, and record it", image)
	}
	if marker == "" {
		t.Fatalf("the marker for %s is empty, which every log line contains", image)
	}
}

// The wrapper quotes the marker in single quotes, and slog may quote the
// line it appears in, so a marker with a quote of either kind could never
// match, or would break the script.
func TestStandbyMarkersAreSafeToQuote(t *testing.T) {
	for image, marker := range kinds.PocketIDStandbyMarker {
		if strings.ContainsAny(marker, `'"\`) {
			t.Errorf("%s: marker %q contains a quote or a backslash", image, marker)
		}
	}
}

func TestAnUnknownImageGetsTheDefaultMarker(t *testing.T) {
	def, _ := kinds.DefaultImage(config.KindPocketID, "app")
	want := kinds.PocketIDStandbyMarker[def]
	got, known := kinds.StandbyMarker("registry.example.org/pocket-id:custom")
	if known || got != want {
		t.Errorf("StandbyMarker(custom) = %q, %v; want %q, false", got, known, want)
	}
}
