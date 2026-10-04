package kinds_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Mbin, Outline and the writefreely-wisp fork keep uploads in S3; Synapse is
// deliberately excluded because its media store is a directory the homeserver
// owns, not a bucket. The other kinds have no uploads at all.
func TestUsesObjectStorage(t *testing.T) {
	cases := map[config.Kind]bool{
		config.KindMbin:        true,
		config.KindOutline:     true,
		config.KindSynapse:     false,
		config.KindElement:     false,
		config.KindOAuth2Proxy: false,
		config.KindPocketID:    false,
		config.KindWriteFreely: true,
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

// The wisp fork supports Postgres and S3, so the blog joins the same cluster
// and the same object store as everything else. Its objects are not public:
// the fork streams images through its own /uploads/ route rather than emitting
// an S3 URL, so nothing anonymous ever reaches its bucket.
func TestWriteFreelyUsesPostgresAndPrivateObjectStorage(t *testing.T) {
	if !kinds.UsesPostgres(config.KindWriteFreely) {
		t.Error("the wisp fork keeps its data in Postgres")
	}
	if !kinds.UsesObjectStorage(config.KindWriteFreely) {
		t.Error("the wisp fork keeps uploaded images in S3")
	}
	if kinds.ServesObjectsPublicly(config.KindWriteFreely) {
		t.Fatal("the fork serves its own images, so its bucket must never be world readable")
	}
	var sawPostgres bool
	for _, s := range kinds.Services(config.KindWriteFreely) {
		if s.Name == kinds.PostgresService {
			sawPostgres = true
		}
	}
	if !sawPostgres {
		t.Error("a pinned blog runs its own Postgres beside itself, so the kind needs that service")
	}
}

// Each kind reads its configuration from a different file in a different
// syntax, so a passthrough key means something different per kind. This is a
// fact about the applications rather than a choice, and getting it wrong puts
// a key in a file nothing reads.
func TestEachKindsConfigFileAndFormat(t *testing.T) {
	cases := map[config.Kind]struct {
		format kinds.Format
		file   string
	}{
		config.KindMbin:        {kinds.ConfigEnv, ".env"},
		config.KindOutline:     {kinds.ConfigEnv, ".env"},
		config.KindPocketID:    {kinds.ConfigEnv, ".env"},
		config.KindOAuth2Proxy: {kinds.ConfigEnv, ".env"},
		config.KindWriteFreely: {kinds.ConfigINI, "config.ini"},
		config.KindSynapse:     {kinds.ConfigYAML, "homeserver.yaml"},
		config.KindElement:     {kinds.ConfigJSON, "config.json"},
	}
	for _, kind := range config.Kinds() {
		want, known := cases[kind]
		if !known {
			t.Errorf("%s is not in this table, so nobody decided where its configuration lives", kind)
			continue
		}
		if got := kinds.ConfigFormat(kind); got != want.format {
			t.Errorf("%s: format is %q, want %q", kind, got, want.format)
		}
		if got := kinds.ConfigFile(kind); got != want.file {
			t.Errorf("%s: file is %q, want %q", kind, got, want.file)
		}
	}
}
