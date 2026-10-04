// Package kinds records what the toolkit knows about each application it can
// render: the services that kind's compose template defines, and the image
// each of those services runs unless the configuration says otherwise.
//
// It exists as its own package because two callers need the same answer and
// neither should own it. internal/validate refuses an `images` key that names
// a service the kind does not have, and internal/render resolves a declared
// reference against the default. If either kept its own copy they would drift,
// and the drift would show up as a configuration that validates and then
// renders a compose file with a service nobody declared.
//
// Every default below was checked against the registry that serves it, on
// 2026-09-08, by requesting the manifest for that exact tag. None is recalled.
package kinds

import (
	"sort"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// Service is one container in a kind's compose template.
type Service struct {
	// Name is the compose service name, which is also the key an operator
	// writes under `images`.
	Name string
	// Image is the reference this service runs when the configuration does not
	// override it. An empty value means the default is computed rather than
	// fixed; postgres is the only such case, because it follows
	// cluster.postgres_version.
	Image string
	// Purpose is one line, quoted back when an operator names a service that
	// does not exist so the message can list what does.
	Purpose string
}

// PostgresService is the database container a pinned app runs beside itself. A
// clustered app has no such service: it connects to the local HAProxy, so
// declaring an image for it renders nothing. That is a warning rather than a
// refusal, because the declaration is harmless and an app's placement can
// change.
const PostgresService = "postgres"

// catalogue is every kind and its services.
//
// The service lists are short because the compose templates are short. They
// grow when a template gains a container, and that is the point of keying the
// map on service names: a key that does not exist here is a typo, and the
// toolkit says so rather than ignoring it.
var catalogue = map[config.Kind][]Service{
	config.KindElement: {
		// No database: Element is a static client that talks to a homeserver
		// the browser reaches directly.
		//
		// v1.12.27 checked against ghcr.io on 2026-09-15 by requesting the
		// manifest for that exact tag, which resolved (200, an OCI image
		// index). The registry's tag list was paged through in full and
		// v1.12.27 is the newest non release candidate tag; v1.12.28-rc.1
		// exists but is a release candidate, not a release.
		{Name: "app", Image: "ghcr.io/element-hq/element-web:v1.12.27", Purpose: "the web client"},
	},
	config.KindMbin: {
		{Name: "app", Image: "ghcr.io/mbinorg/mbin:v1.10.1", Purpose: "the application"},
		{Name: PostgresService, Purpose: "its own database, when the app is pinned"},
	},
	config.KindOAuth2Proxy: {
		// Two instances, deliberately: one gates visitors who have
		// authenticated, one gates members. Which applications sit behind
		// which is the community's access policy, declared per app.
		//
		// Ports are fixed here rather than derived: provisional runs on 4180,
		// this kind's primary port in internal/render/plan.go's appPort, and
		// members runs on the next port up, 4181. Both are spelled out in the
		// compose and Caddy snippet templates too, because a Service here
		// carries no port field and a port nobody declared is a port nobody
		// can route to.
		//
		// v7.15.4 checked against quay.io on 2026-09-15 by requesting the tag
		// through quay.io's API, which resolved (an OCI image index with ten
		// child manifests).
		{Name: "provisional", Image: "quay.io/oauth2-proxy/oauth2-proxy:v7.15.4", Purpose: "the gate for anyone signed in, listening on 4180"},
		{Name: "members", Image: "quay.io/oauth2-proxy/oauth2-proxy:v7.15.4", Purpose: "the gate for members, listening on 4181"},
	},
	config.KindOutline: {
		{Name: "app", Image: "outlinewiki/outline:1.10.0", Purpose: "the application"},
		{Name: PostgresService, Purpose: "its own database, when the app is pinned"},
	},
	config.KindPocketID: {
		{Name: "app", Image: "ghcr.io/pocket-id/pocket-id:v2.14.0", Purpose: "the application"},
		{Name: PostgresService, Purpose: "its own database, when the app is pinned"},
	},
	config.KindSynapse: {
		// Three containers, because a homeserver does not authenticate anyone
		// any more. Synapse is a resource server, Matrix Authentication
		// Service owns the login, and the identity provider is upstream of
		// MAS rather than of Synapse.
		//
		// NOTE THE MISSING `v`. Synapse tags as vX.Y.Z and MAS tags as X.Y.Z.
		// Checked against ghcr.io on 2026-09-15 by requesting the manifest for
		// each: `1.24.0` resolved (200), `v1.24.0` did not (404), and neither
		// did `1.25.0`. The registry's tag list was paged through in full and
		// 1.24.0 is the newest release; 1.25.0-rc.0 exists but is a release
		// candidate.
		{Name: "app", Image: "ghcr.io/element-hq/synapse:v1.160.0", Purpose: "the homeserver, which only serves the API"},
		{Name: "mas", Image: "ghcr.io/element-hq/matrix-authentication-service:1.24.0", Purpose: "Matrix Authentication Service, which owns the login"},
		{Name: PostgresService, Purpose: "its own database, when the app is pinned"},
	},
	config.KindWriteFreely: {
		// This kind means the writefreely-wisp fork, not upstream WriteFreely.
		// The fork adds Postgres and S3 support upstream does not have, so the
		// blog joins the same Patroni cluster and the same Garage node as
		// everything else instead of a SQLite file nothing else can reach.
		// Upstream's image has no [database] Postgres driver and no [storage]
		// section at all, so it will reject the configuration this kind
		// renders; do not point this kind at ghcr.io/writefreely/writefreely.
		//
		// The digest below is a `develop` build, not a release: no tagged
		// release of the fork carries Postgres or S3 support yet. Resolved
		// against ghcr.io on 2026-10-04 with `docker manifest inspect`, which
		// returned a multi architecture index covering linux/amd64 and
		// linux/arm64, and confirmed by pulling the amd64 manifest and
		// checking the binary with `strings` for the `s3_secret_access_key`
		// ini tag and the Postgres `sslmode` field. Bump this once a release
		// carries the same features, and drop this paragraph when it does.
		{Name: "app", Image: "ghcr.io/josephquigley/writefreely-wisp@sha256:4d21f45879bd98c8485eb8169ea57fbab925f0cbd5a38ac3c3bdd79901d809ea", Purpose: "the application"},
		{Name: PostgresService, Purpose: "its own database, when the app is pinned"},
	},
}

// Services returns a kind's services in compose order, which is the order they
// are declared above rather than alphabetical: `app` first is what an operator
// expects to read.
func Services(kind config.Kind) []Service {
	return append([]Service(nil), catalogue[kind]...)
}

// Has reports whether a kind defines a service.
func Has(kind config.Kind, service string) bool {
	for _, s := range catalogue[kind] {
		if s.Name == service {
			return true
		}
	}
	return false
}

// DefaultImage returns the reference a service runs when the configuration
// says nothing. The second result is false when the kind computes it instead
// of shipping one.
func DefaultImage(kind config.Kind, service string) (string, bool) {
	for _, s := range catalogue[kind] {
		if s.Name == service {
			return s.Image, s.Image != ""
		}
	}
	return "", false
}

// UsesPostgres reports whether a kind stores its data in Postgres at all.
//
// Two kinds do not: Element, which is a static client with no server side
// state at all, and oauth2-proxy, which keeps a session in a cookie. The
// difference is load bearing rather than cosmetic in two directions. An app
// that uses no Postgres needs no database role, no password and no place in
// the cluster, so requiring one would refuse a configuration that is entirely
// correct. It is also what cluster-placement-without-a-cluster reads: a kind
// answering false here can only be pinned, because cluster placement would
// render it onto every apps site with storage of its own.
func UsesPostgres(kind config.Kind) bool {
	return Has(kind, PostgresService)
}

// UsesObjectStorage reports whether a kind keeps uploads in S3 rather than on
// local disk.
//
// Synapse is deliberately absent. Its media store is a directory the
// homeserver owns, which is also why the synapse kind is pinned to one node.
func UsesObjectStorage(kind config.Kind) bool {
	switch kind {
	case config.KindMbin, config.KindOutline, config.KindWriteFreely:
		return true
	default:
		return false
	}
}

// ServesObjectsPublicly reports whether a kind's objects must be readable
// without a credential.
//
// Mbin's are: a federating server fetching an image is a machine with no
// account here, and a remote instance caches the URL it was given. Outline's
// are not, because its bucket holds the attachments of documents that are
// readable only to members, and its server presigns every read it issues.
//
// This is deliberately not a configuration key. Garage's website access is per
// bucket and opt in, so the only way Outline's bucket becomes world readable
// is a change to this function, which is a code change with a review rather
// than a line somebody edits at two in the morning.
//
// A kind storing objects (UsesObjectStorage) is not the same question as one
// whose objects are fetched anonymously: a kind can keep its uploads in S3
// while serving them through its own application route, never exposing its
// bucket directly. WriteFreely (meaning the wisp fork) is exactly that case:
// it streams images through its own /uploads/ route with http.ServeContent
// and never emits or presigns an S3 URL, so its bucket gets no website access
// even though the kind uses object storage.
func ServesObjectsPublicly(kind config.Kind) bool {
	return kind == config.KindMbin
}

// ServiceNames returns a kind's service names, sorted, for an error message
// that has to list them.
func ServiceNames(kind config.Kind) []string {
	var out []string
	for _, s := range catalogue[kind] {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// Format is the syntax of the file a kind reads its configuration from.
type Format string

const (
	// ConfigNone is the zero value: a kind that renders no configuration file.
	// Every kind today renders one, so nothing returns it yet; it is declared so
	// that a kind added later without one has an honest answer to give.
	ConfigNone Format = ""
	ConfigEnv  Format = "env"
	ConfigINI  Format = "ini"
	ConfigYAML Format = "yaml"
	ConfigJSON Format = "json"
)

// configFiles is where each kind's passthrough configuration lives. The file
// names are the rendered names, relative to the stack, rather than the template
// names: the renderer drops `.tmpl` and then `.secret`, so
// writefreely/config.ini.secret.tmpl is `config.ini` on the host.
//
// Synapse and writefreely also render a `.env`, but only for their Postgres
// container's password. The application reads homeserver.yaml and config.ini,
// so those are where a key means anything.
var configFiles = map[config.Kind]struct {
	format Format
	file   string
}{
	config.KindMbin:        {ConfigEnv, ".env"},
	config.KindOutline:     {ConfigEnv, ".env"},
	config.KindPocketID:    {ConfigEnv, ".env"},
	config.KindOAuth2Proxy: {ConfigEnv, ".env"},
	config.KindWriteFreely: {ConfigINI, "config.ini"},
	config.KindSynapse:     {ConfigYAML, "homeserver.yaml"},
	config.KindElement:     {ConfigJSON, "config.json"},
}

// ConfigFormat is the syntax of the file a kind reads its configuration from,
// or ConfigNone when it has none.
func ConfigFormat(kind config.Kind) Format { return configFiles[kind].format }

// ConfigFile is the rendered path of that file, relative to the stack.
func ConfigFile(kind config.Kind) string { return configFiles[kind].file }
