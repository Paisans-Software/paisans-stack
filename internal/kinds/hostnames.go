package kinds

import (
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// PrimaryRole is the hostname every app has, held in App.Hostname rather than
// in the Hostnames map.
const PrimaryRole = "primary"

// WellknownRole is the hostname a homeserver's delegation documents are served
// on. It is named here rather than spelled as a literal in three packages
// because it decides more than routing: the name in every user identifier is
// the name the delegation is served on, so it is also what server_name must
// be.
const WellknownRole = "wellknown"

// MediaRole is the hostname an app's stored objects are served on. Every kind
// that keeps objects in S3 has one, and unlike any other role it exists
// whether or not it is declared: MediaHostname derives it when the app does
// not name one under `hostnames`.
//
// It is one hostname per app rather than one for the deployment. A hostname
// can be pointed by DNS at a CDN, another provider or another Garage, one app
// at a time, and a path under a shared hostname cannot; and two apps' user
// uploads on two hostnames are two browser origins, so a file one app stored
// cannot script against another's.
const MediaRole = "media"

// MediaSuffix is what a derived media hostname appends to the label of the
// app's own hostname.
const MediaSuffix = "-media"

// extraRoles is the additional hostname roles each kind understands, and it is
// deliberately a closed set. A role is not a label: it selects which Caddy
// snippet that hostname gets, so a role the kind ships no snippet for would
// render a host block importing a file that does not exist.
var extraRoles = map[config.Kind][]string{
	// A homeserver answers its API on the primary name and its delegation
	// documents on whatever name appears in user identifiers, which is usually
	// the apex. Routing them identically would publish the whole API on the
	// apex as well.
	config.KindSynapse: {WellknownRole},

	// Every kind that stores objects serves them on a hostname of its own.
	// The set has to agree with UsesObjectStorage, and a test holds it to
	// that: a kind that stores objects without this role would publish URLs
	// nothing routes, and a kind with the role and no bucket would route a
	// hostname to nothing.
	config.KindMbin:        {MediaRole},
	config.KindOutline:     {MediaRole},
	config.KindWriteFreely: {MediaRole},
}

// HostnameRoles returns the roles a kind understands, sorted, always including
// the primary.
func HostnameRoles(kind config.Kind) []string {
	out := append([]string{PrimaryRole}, extraRoles[kind]...)
	sort.Strings(out)
	return out
}

// HasHostnameRole reports whether a kind understands a role.
func HasHostnameRole(kind config.Kind, role string) bool {
	if role == PrimaryRole {
		return true
	}
	for _, known := range extraRoles[kind] {
		if known == role {
			return true
		}
	}
	return false
}

// SnippetFor returns the template filename a role's routing lives in.
func SnippetFor(role string) string {
	if role == PrimaryRole {
		return "caddy.snippet.tmpl"
	}
	return "caddy.snippet." + role + ".tmpl"
}

// MediaHostname is where an app's stored objects are served from, and empty
// for a kind that stores none.
//
// An app that declares `hostnames.media` gets exactly that. Otherwise the name
// is derived as a sibling of the app's own hostname: the first label of that
// hostname, then MediaSuffix, under the community's domain. talk.example.org
// stores its objects at talk-media.example.org.
//
// A sibling and never a child. media.talk.example.org would sit under the
// app's own host, so a cookie the app scopes to its host with a Domain
// attribute would be sent with every media request, and a file served there
// would share that cookie scope. And never a name for the backend, such as
// garage. or s3.: the name is the app's, and what answers it may change.
//
// The derivation is deliberately not validated here. A derived name can still
// be wrong (a label too long once the suffix is added, or one that collides
// with another app's hostname), and internal/validate refuses those by the
// key an operator has to edit, which this package cannot name.
func MediaHostname(app config.App, domain string) string {
	if !HasHostnameRole(app.Kind, MediaRole) {
		return ""
	}
	if declared := strings.TrimSpace(app.Hostnames[MediaRole]); declared != "" {
		return declared
	}
	label, _, _ := strings.Cut(app.Hostname, ".")
	if label == "" || domain == "" {
		return ""
	}
	return label + MediaSuffix + "." + domain
}

// MediaHostnameIsDerived reports whether an app's media hostname comes from
// the derivation rather than from `hostnames.media`, so an error message can
// say which one the operator has to change.
func MediaHostnameIsDerived(app config.App) bool {
	return HasHostnameRole(app.Kind, MediaRole) && strings.TrimSpace(app.Hostnames[MediaRole]) == ""
}
