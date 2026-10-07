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
// and so must the blog's images now that the wisp fork serves them only from
// image_url_base. Outline's bucket holds document attachments and must never
// be.
func TestMbinAndTheBlogServeObjectsPubliclyAndOutlineDoesNot(t *testing.T) {
	for _, kind := range config.Kinds() {
		want := kind == config.KindMbin || kind == config.KindWriteFreely
		if got := kinds.ServesObjectsPublicly(kind); got != want {
			t.Errorf("%s: ServesObjectsPublicly is %v, want %v", kind, got, want)
		}
	}
	if kinds.ServesObjectsPublicly(config.KindOutline) {
		t.Fatal("outline's bucket holds document attachments and must never be world readable")
	}
}

// The wisp fork supports Postgres and S3, so the blog joins the same cluster
// and the same object store as everything else. With S3 the fork is direct
// only: it writes images and never serves them, so a reader with no credential
// has to be able to read its bucket.
func TestWriteFreelyUsesPostgresAndPublicObjectStorage(t *testing.T) {
	if !kinds.UsesPostgres(config.KindWriteFreely) {
		t.Error("the wisp fork keeps its data in Postgres")
	}
	if !kinds.UsesObjectStorage(config.KindWriteFreely) {
		t.Error("the wisp fork keeps uploaded images in S3")
	}
	if !kinds.ServesObjectsPublicly(config.KindWriteFreely) {
		t.Fatal("the fork never serves its own images under S3, so its bucket must be readable by a reader with no credential")
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

// The media role and object storage are one fact stated twice, and they must
// agree: a kind that stores objects without the role would publish URLs
// nothing routes, and a kind with the role and no bucket would route a
// hostname to nothing.
func TestEveryKindThatStoresObjectsHasAMediaRole(t *testing.T) {
	for _, kind := range config.Kinds() {
		if got, want := kinds.HasHostnameRole(kind, kinds.MediaRole), kinds.UsesObjectStorage(kind); got != want {
			t.Errorf("%s: HasHostnameRole(media) is %v, UsesObjectStorage is %v", kind, got, want)
		}
	}
}

// The derived name is a sibling of the app's own hostname, under the
// community's domain, and never a child of it or a name for the backend. A
// declared name wins outright.
func TestMediaHostnameIsDerivedAsASiblingAndCanBeOverridden(t *testing.T) {
	for _, tc := range []struct {
		name string
		app  config.App
		want string
	}{
		{"mbin", config.App{Kind: config.KindMbin, Hostname: "talk.example.org"}, "talk-media.example.org"},
		{"outline", config.App{Kind: config.KindOutline, Hostname: "docs.example.org"}, "docs-media.example.org"},
		{"writefreely", config.App{Kind: config.KindWriteFreely, Hostname: "blog.example.org"}, "blog-media.example.org"},
		// The label is the first one, and the result is under the domain
		// rather than under whatever the rest of the app's hostname was.
		{"deeper hostname", config.App{Kind: config.KindMbin, Hostname: "talk.eu.example.org"}, "talk-media.example.org"},
		{"declared", config.App{Kind: config.KindOutline, Hostname: "docs.example.org", Hostnames: map[string]string{"media": "attachments.example.org"}}, "attachments.example.org"},
		{"stores nothing", config.App{Kind: config.KindPocketID, Hostname: "id.example.org"}, ""},
		{"synapse keeps media on disk", config.App{Kind: config.KindSynapse, Hostname: "chat.example.org"}, ""},
	} {
		if got := kinds.MediaHostname(tc.app, "example.org"); got != tc.want {
			t.Errorf("%s: MediaHostname = %q, want %q", tc.name, got, tc.want)
		}
	}
	if !kinds.MediaHostnameIsDerived(config.App{Kind: config.KindMbin, Hostname: "talk.example.org"}) {
		t.Error("an app with no hostnames.media has a derived media hostname")
	}
	if kinds.MediaHostnameIsDerived(config.App{Kind: config.KindMbin, Hostname: "talk.example.org", Hostnames: map[string]string{"media": "m.example.org"}}) {
		t.Error("an app that declares hostnames.media does not have a derived one")
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
		config.KindUptime:      {kinds.ConfigEnv, ".env"},
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
