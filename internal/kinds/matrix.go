package kinds

import (
	"crypto/sha256"
	"math/big"
)

// The synapse kind's authentication service needs two facts that three
// packages have to agree on, so they live here rather than in whichever one
// happened to need them first: internal/render writes them into a rendered
// configuration, and internal/secretsgen tells an operator what to register at
// the identity provider before that configuration exists.

// MASUpstreamProviderID is the identifier Matrix Authentication Service gives
// the identity provider it authenticates against.
//
// MAS requires a ULID there, and the value is not private: it appears in the
// callback URL registered at the identity provider, so the operator has to
// know it. Deriving it from the app's name rather than generating one keeps
// rendering deterministic, which is what stops a second `apply` from silently
// inventing a provider the registered callback no longer matches.
//
// A ULID is 26 Crockford base32 characters over 128 bits, which leaves two
// unused bits at the top, so the leading character of any value encoded this
// way is in 0 to 7 and the result always parses. It is not a real ULID in the
// sense of carrying a timestamp, and nothing here reads one back out of it.
func MASUpstreamProviderID(app string) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	sum := sha256.Sum256([]byte("paisans-stack upstream oauth2 provider:" + app))
	n := new(big.Int).SetBytes(sum[:16])
	mask := big.NewInt(31)
	digit := new(big.Int)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[digit.And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return string(out)
}

// MASRedirectURI is the callback an operator registers at the identity
// provider for a homeserver's client.
//
// It is not the `/oauth/callback` every other kind uses. The client belongs to
// the authentication service rather than to the application, and MAS serves
// one callback path per upstream provider, ending in that provider's id. A
// client registered with the wrong redirect URI fails at the end of the first
// sign in, after the member has already authenticated, which is the least
// informative moment for it to fail.
func MASRedirectURI(hostname, app string) string {
	return "https://" + hostname + "/upstream/callback/" + MASUpstreamProviderID(app)
}
