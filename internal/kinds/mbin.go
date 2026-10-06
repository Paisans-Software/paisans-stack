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

// OIDCClientSpec is what an app's client at the identity provider has to look
// like for the app to sign anyone in. `paisans oidc client create` creates a
// client to it and refuses an existing one that differs.
type OIDCClientSpec struct {
	// CallbackURL is the one redirect URI the client must allow.
	CallbackURL string
	// PKCE is whether the app always sends a code challenge, in which case a
	// client with PKCE off refuses every sign in.
	PKCE bool
	// AdminGroupKey and MemberGroupKey name keys in the app's own `config`
	// whose values are group names the app reads from the `groups` claim.
	// Empty when the kind reads no such key.
	AdminGroupKey  string
	MemberGroupKey string
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
			PKCE:           true,
			AdminGroupKey:  "OAUTH_OIDC_ADMIN_GROUP",
			MemberGroupKey: "OAUTH_OIDC_MEMBER_GROUP",
		}, true
	}
	return OIDCClientSpec{}, false
}
