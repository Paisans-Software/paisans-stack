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
		// v0.17.2 is tagged on GitHub but no image was published for it; the
		// newest reference that resolves is v0.17.1.
		//
		// No database service: WriteFreely supports MySQL and SQLite and has
		// never supported Postgres, so it runs on a SQLite file in its own data
		// directory. That is what keeps one blog from adding a second database
		// engine to operate.
		{Name: "app", Image: "ghcr.io/writefreely/writefreely:v0.17.1", Purpose: "the application"},
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
// Only WriteFreely does not, and the difference is load bearing rather than
// cosmetic: an app that uses no Postgres needs no database role, no password
// and no place in the cluster, so requiring one would refuse a configuration
// that is entirely correct.
func UsesPostgres(kind config.Kind) bool {
	return Has(kind, PostgresService)
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
