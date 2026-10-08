package kinds

import "github.com/paisans-software/paisans-stack/internal/config"

// MbinRedirectURI is the callback an operator registers at the identity
// provider for an Mbin app's client, and the one the rendered .env names.
//
// It lives here for the same reason MASRedirectURI does: internal/render
// writes it into a rendered file and internal/secretsgen tells an operator to
// register it before that file exists, and the two must agree.
//
// The path is the generic OIDC provider's verify route in the paisans fork,
// read from config/mbin_routes/security.yaml at tag v1.13.3+paisans
// (oauth_oidc_verify, path /oauth/oidc/verify). It is not /oauth/callback,
// which no route in that tree serves.
func MbinRedirectURI(hostname string) string {
	return "https://" + hostname + "/oauth/oidc/verify"
}

// MbinDashboardPath is where the identity provider's dashboard tile for an
// Mbin app sends a member. The bare host lands them signed out, with a log in
// button still to press; this route, oauth_oidc_connect in
// config/mbin_routes/security.yaml at tag v1.13.3+paisans, starts the OIDC
// flow, so picking the tile signs the member in.
const MbinDashboardPath = "/oauth/oidc/connect"

// OIDCClientSpec is what an app's client at the identity provider has to look
// like for the app to sign anyone in. `paisans apply` and `paisans oidc client
// create` create a client to it and refuse an existing one that differs.
type OIDCClientSpec struct {
	// CallbackURL is the one redirect URI the client must allow.
	CallbackURL string
	// LaunchURL is where Pocket ID's dashboard sends a member who picks the
	// app: the app's own address plus the kind's DashboardPath. A client
	// without one is not listed there. An app's sso_dashboard_link setting
	// replaces the path; see LaunchURL.
	LaunchURL string
	// PKCE is whether the app always sends a code challenge, in which case a
	// client with PKCE off refuses every sign in.
	PKCE bool
	// AdminGroupKey and MemberGroupKey name keys in the app's own `config`
	// whose values are group names the app reads from the `groups` claim.
	// Empty when the kind reads no such key.
	AdminGroupKey  string
	MemberGroupKey string
	// AdminGroupSetting and MemberGroupSetting name app `settings` keys
	// instead, for a kind whose group is an input the toolkit reasons about
	// rather than a passthrough key. A spec uses one pair or the other.
	AdminGroupSetting  string
	MemberGroupSetting string
}

// Groups is the admin group and the member group an app's declaration names,
// read from whichever of config or settings the kind keeps them in. Either is
// empty when the app names none.
func (s OIDCClientSpec) Groups(app config.App) (admin, member string) {
	read := func(configKey, settingKey string) string {
		if configKey != "" {
			v, _ := app.Config[configKey].(string)
			return v
		}
		if settingKey != "" {
			v, _ := app.Settings[settingKey].(string)
			return v
		}
		return ""
	}
	return read(s.AdminGroupKey, s.AdminGroupSetting), read(s.MemberGroupKey, s.MemberGroupSetting)
}

// AdminGroupSource is the configuration key an app's admin group is read
// from, for a message telling an operator where to set it.
func (s OIDCClientSpec) AdminGroupSource(app string) string {
	if s.AdminGroupKey != "" {
		return "apps." + app + ".config." + s.AdminGroupKey
	}
	return "apps." + app + ".settings." + s.AdminGroupSetting
}

// OIDCClient is the client spec for an app of a kind, and whether this
// toolkit knows one for it yet.
//
// Mbin's is the paisans fork's generic OIDC provider, read at tag
// v1.13.3+paisans: the callback is MbinRedirectURI; PKCE is on because the
// fork's OidcClient extends KnpU's OAuth2PKCEClient and always sends a code
// challenge (src/Security/Oidc/OidcClient.php:18); and OAUTH_OIDC_ADMIN_GROUP
// and OAUTH_OIDC_MEMBER_GROUP name the groups it reads from the `groups`
// claim (.env.example_docker:243-273).
func OIDCClient(kind config.Kind, hostname string) (OIDCClientSpec, bool) {
	switch kind {
	case config.KindMbin:
		return OIDCClientSpec{
			CallbackURL:    MbinRedirectURI(hostname),
			LaunchURL:      LaunchURL(kind, hostname, ""),
			PKCE:           true,
			AdminGroupKey:  "OAUTH_OIDC_ADMIN_GROUP",
			MemberGroupKey: "OAUTH_OIDC_MEMBER_GROUP",
		}, true
	case config.KindUptime:
		// The Paisans-Software/uptime fork at 1.1.0-oidc.2: the callback is
		// /login/oidc/callback (src/lib/oidc.js:26), and every sign in sends
		// an S256 code challenge (src/lib/oidc.js:96-103), so PKCE is on. Its
		// one group is settings.admin_group, used as both: its members are
		// the monitor's administrators, and nobody else may sign in, so the
		// client is restricted to it and Pocket ID refuses everyone else
		// before the monitor sees them.
		return OIDCClientSpec{
			CallbackURL:        "https://" + hostname + "/login/oidc/callback",
			LaunchURL:          LaunchURL(kind, hostname, ""),
			PKCE:               true,
			AdminGroupSetting:  "admin_group",
			MemberGroupSetting: "admin_group",
		}, true
	}
	return OIDCClientSpec{}, false
}

// MbinQueueSetting is the app setting that picks where an Mbin app's
// Messenger queues live: MbinQueuePostgres, the default, or MbinQueueRabbitMQ.
// README, "Mbin's queues are in Postgres by default", has the reasoning.
const MbinQueueSetting = "queue"

const (
	// MbinQueuePostgres keeps the queues as rows in the app's own database,
	// through Symfony Messenger's Doctrine transport. They survive losing an
	// apps site, and every site's consumers share them. It needs paisans fork
	// 1.14.0-paisans or later, which carries the Doctrine transport decorator:
	// on an older image the shipped messenger.yaml's AMQP options are refused
	// by the Doctrine transport at the first dispatch.
	MbinQueuePostgres = "postgres"
	// MbinQueueRabbitMQ runs a broker per stack, behind amqproxy, as upstream
	// does. Faster under load, and lost with its site.
	MbinQueueRabbitMQ = "rabbitmq"
)

// MbinQueue is the queue backend an Mbin app's settings choose, the default
// when they choose none. A value that is neither is returned as given, for
// validate to refuse by name.
func MbinQueue(settings map[string]any) string {
	if s, ok := settings[MbinQueueSetting].(string); ok && s != "" {
		return s
	}
	return MbinQueuePostgres
}
