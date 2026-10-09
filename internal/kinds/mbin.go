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
	// PKCE is whether the app always sends a code challenge. A Pocket ID
	// client with PKCE on refuses every request without one
	// (oidc/authorization_service.go:750-755 at v2.14.0) and one with it off
	// accepts a challenge when sent, so this is on only for an app that
	// always sends one, and off for an app that never does.
	PKCE bool
	// AdminGroupSetting and MemberGroupSetting name the app `settings` keys
	// holding the admin group and the member group. The member group is the
	// one a person must be in to sign in, and the client is restricted at
	// Pocket ID to it and the admin group. A kind whose admin group is not an
	// operator's choice leaves AdminGroupSetting empty.
	AdminGroupSetting  string
	MemberGroupSetting string
	// DefaultAdminGroup and DefaultMemberGroup hold when the settings are
	// absent. A member facing kind always has a default member group, so an
	// empty declaration never gets an unrestricted client.
	DefaultAdminGroup  string
	DefaultMemberGroup string
	// ReadsGroups is whether the app reads the groups claim itself, mapping
	// the admin group to its administrators and refusing a sign in outside
	// the member group. For a kind that does not, the restriction at Pocket
	// ID is the whole control.
	ReadsGroups bool
}

// Groups is the admin group and the member group an app's client admits:
// the app's settings, or the kind's defaults. A blank or non-string setting
// counts as absent (see groupSetting).
func (s OIDCClientSpec) Groups(app config.App) (admin, member string) {
	return groupSetting(app.Settings, s.AdminGroupSetting, s.DefaultAdminGroup),
		groupSetting(app.Settings, s.MemberGroupSetting, s.DefaultMemberGroup)
}

// AdminGroupSource is the configuration key an app's admin group is read
// from, for a message telling an operator where to set it, or the kind when
// it is not a setting.
func (s OIDCClientSpec) AdminGroupSource(app string) string {
	if s.AdminGroupSetting == "" {
		return "apps." + app + ".kind"
	}
	return "apps." + app + ".settings." + s.AdminGroupSetting
}

// OIDCClient is the client spec for an app of a kind, and whether this
// toolkit knows one for it yet. Every spec's callback is read off the pinned
// image's source and cited, because a client with the wrong one fails at the
// end of the first sign in, after the member has authenticated.
func OIDCClient(kind config.Kind, hostname string) (OIDCClientSpec, bool) {
	switch kind {
	case config.KindMbin:
		// The paisans fork's generic OIDC provider, read at tag
		// v1.13.3+paisans: the callback is MbinRedirectURI; PKCE is on because
		// the fork's OidcClient extends KnpU's OAuth2PKCEClient and always
		// sends a code challenge (src/Security/Oidc/OidcClient.php:18); and
		// OAUTH_OIDC_ADMIN_GROUP and OAUTH_OIDC_MEMBER_GROUP, which the .env
		// template renders from these two settings, name the groups it reads
		// from the `groups` claim (.env.example_docker:243-273). The groups
		// scope is requested only when one of them is set
		// (src/Provider/Oidc.php:45-46), which is why both have defaults: an
		// empty pair would leave the client unrestricted and the claim
		// unread.
		return OIDCClientSpec{
			CallbackURL:        MbinRedirectURI(hostname),
			LaunchURL:          LaunchURL(kind, hostname, ""),
			PKCE:               true,
			AdminGroupSetting:  "admin_group",
			MemberGroupSetting: "member_group",
			DefaultAdminGroup:  AdminsGroup,
			DefaultMemberGroup: MembersGroup,
			ReadsGroups:        true,
		}, true
	case config.KindOutline:
		// outline v1.10.0: the callback is ${URL}/auth/${config.id}.callback
		// with the provider id oidc (plugins/oidc/server/auth/oidcRouter.ts:63,
		// registered at :249-253). PKCE is off because the toolkit renders the
		// four explicit OIDC_*_URI values, and the manual configuration branch
		// never passes pkce (plugins/oidc/server/auth/oidc.ts:27-35; only the
		// OIDC_ISSUER_URL branch does, at :55). Outline reads no groups claim,
		// so the restriction at Pocket ID is the whole control.
		return OIDCClientSpec{
			CallbackURL:        "https://" + hostname + "/auth/oidc.callback",
			LaunchURL:          LaunchURL(kind, hostname, ""),
			PKCE:               false,
			MemberGroupSetting: "member_group",
			DefaultAdminGroup:  AdminsGroup,
			DefaultMemberGroup: MembersGroup,
		}, true
	case config.KindWriteFreely:
		// writefreely-wisp ff9dceb: the callback is app.Host +
		// "/oauth/callback/generic", built in configureGenericOauth
		// (oauth.go:249) and not configurable. Nothing in the fork sends a
		// code challenge, so PKCE is off. WriteFreely reads no groups claim
		// either; the restriction at Pocket ID is the whole control.
		return OIDCClientSpec{
			CallbackURL:        "https://" + hostname + "/oauth/callback/generic",
			LaunchURL:          LaunchURL(kind, hostname, ""),
			PKCE:               false,
			MemberGroupSetting: "member_group",
			DefaultAdminGroup:  AdminsGroup,
			DefaultMemberGroup: MembersGroup,
		}, true
	case config.KindUptime:
		// The Paisans-Software/uptime fork at 1.1.0-oidc.2: the callback is
		// /login/oidc/callback (src/lib/oidc.js:26), and every sign in sends
		// an S256 code challenge (src/lib/oidc.js:96-103), so PKCE is on. Its
		// one group is settings.admin_group, used as both: its members are
		// the monitor's administrators, and nobody else may sign in, so the
		// client is restricted to it and Pocket ID refuses everyone else
		// before the monitor sees them. It has no default: validate requires
		// it (uptime-needs-an-admin-group).
		return OIDCClientSpec{
			CallbackURL:        "https://" + hostname + "/login/oidc/callback",
			LaunchURL:          LaunchURL(kind, hostname, ""),
			PKCE:               true,
			AdminGroupSetting:  "admin_group",
			MemberGroupSetting: "admin_group",
			ReadsGroups:        true,
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
