package kinds_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Mbin and Outline keep uploads in S3; Synapse is deliberately excluded
// because its media store is a directory the homeserver owns, not a bucket.
// The other kinds have no uploads at all.
func TestUsesObjectStorage(t *testing.T) {
	cases := map[config.Kind]bool{
		config.KindMbin:        true,
		config.KindOutline:     true,
		config.KindSynapse:     false,
		config.KindElement:     false,
		config.KindOAuth2Proxy: false,
		config.KindPocketID:    false,
		config.KindWriteFreely: false,
	}
	for kind, want := range cases {
		if got := kinds.UsesObjectStorage(kind); got != want {
			t.Errorf("UsesObjectStorage(%s) = %v, want %v", kind, got, want)
		}
	}
}

// Mbin's media must be fetchable by a federating server with no credential,
// which is why its bucket is public. Outline's bucket holds document
// attachments and must never be.
func TestOnlyMbinServesObjectsPublicly(t *testing.T) {
	for _, kind := range config.Kinds() {
		want := kind == config.KindMbin
		if got := kinds.ServesObjectsPublicly(kind); got != want {
			t.Errorf("%s: ServesObjectsPublicly is %v, want %v", kind, got, want)
		}
	}
	if kinds.ServesObjectsPublicly(config.KindOutline) {
		t.Fatal("outline's bucket holds document attachments and must never be world readable")
	}
}
