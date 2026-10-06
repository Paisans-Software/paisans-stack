package kinds

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
