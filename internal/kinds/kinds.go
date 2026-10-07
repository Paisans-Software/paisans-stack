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
		// The paisans fork, not upstream: it carries the generic OIDC
		// provider this kind's .env configures, which upstream does not
		// have. Released 2026-10-05 as 1.13.3-paisans. Checked against
		// ghcr.io on 2026-10-06 by requesting the manifest for that exact tag
		// with an anonymous pull token, which resolved (200, an OCI image
		// index, sha256:42e65e245b1026e0384a3834b7e49721123115e5acc11a63b29cd4bdf6eb6ef4).
		// The index carries linux/amd64 only, beside an attestation
		// manifest, so an arm64 apps site cannot run it.
		{Name: "app", Image: "ghcr.io/paisans-software/mbin:1.13.3-paisans", Purpose: "the application, and its messenger consumers"},
		// The three sidecars upstream's compose.yaml runs beside it. The
		// messenger consumers are not here: they run the application's own
		// code, so they always take the app's image, and a key that let them
		// diverge would only be a way to run two versions of one schema.
		//
		// Upstream names a floating tag for each; these are the releases
		// those tags resolved to when checked against Docker Hub on
		// 2026-10-06, by requesting the manifest for each tag and comparing
		// digests. 3-management-alpine and 3.13.7-management-alpine returned
		// the same OCI image index, as did trixie and 9.1.2-trixie, and
		// latest and 3.2.0. RabbitMQ 3.13 is the last 3.x series and upstream
		// still runs it; a move to 4.x is a broker upgrade to make
		// deliberately, not a default to drift into.
		{Name: "amqproxy", Image: "docker.io/cloudamqp/amqproxy:3.2.0", Purpose: "the AMQP connection pool in front of the broker"},
		{Name: "rabbitmq", Image: "docker.io/library/rabbitmq:3.13.7-management-alpine", Purpose: "the message broker for federation and background work"},
		{Name: "valkey", Image: "docker.io/valkey/valkey:9.1.2-trixie", Purpose: "the cache"},
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
		//
		// This digest predates writefreely-wisp#171, which makes S3 storage
		// direct only and adds `[storage] image_url_base`. The template
		// renders that key and the gateway serves the bucket on the blog's
		// media hostname, but this build does not read the key: it keeps
		// streaming images through /uploads/ with its own credential, which
		// still works and does not use the media hostname. Move the pin to a
		// build containing #171 once it is merged, which is when the media
		// hostname starts carrying the blog's images.
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
// account here, and a remote instance caches the URL it was given. So are
// WriteFreely's, meaning the wisp fork: with `[storage] type = s3` the fork
// writes images to the bucket and never reads them back to serve them. It
// requires `[storage] image_url_base`, publishes every image URL under it, and
// answers its own /uploads/ route with a redirect there, so the bucket has to
// answer a reader with no credential or no image on the blog loads at all.
//
// Outline's are not, because its bucket holds the attachments of documents
// that are readable only to members, and its server presigns every read it
// issues.
//
// This is deliberately not a configuration key. Garage's website access is per
// bucket and opt in, so the only way Outline's bucket becomes world readable
// is a change to this function, which is a code change with a review rather
// than a line somebody edits at two in the morning.
//
// A kind storing objects (UsesObjectStorage) is not the same question as one
// whose objects are fetched anonymously: a kind can keep its uploads in S3
// while serving them through its own application route, or through
// signatures it issues, never exposing its bucket. Outline is the second
// case, and the fork used to be the first, before it became direct only.
func ServesObjectsPublicly(kind config.Kind) bool {
	return kind == config.KindMbin || kind == config.KindWriteFreely
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

// BucketName is the bucket an app that stores objects keeps them in: the
// `s3_bucket` setting when one is declared, and the app's name with
// "-uploads" otherwise.
//
// It is here because three packages need the same answer: the renderer, which
// gives the app the name; the provisioner, which creates the bucket; and
// validate, which checks the name against what the app will do with it. Three
// copies of one default is how the bucket the app writes to and the bucket the
// provisioner created stop being the same bucket.
func BucketName(name string, app config.App) string {
	if v, ok := app.Settings["s3_bucket"].(string); ok && v != "" {
		return v
	}
	return name + "-uploads"
}
