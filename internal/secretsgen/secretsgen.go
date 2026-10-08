// Package secretsgen fills in the secrets a deployment needs and never invents
// the ones it cannot.
//
// Three kinds of secret live in one file, and only one of them belongs here.
// Generated secrets are made by this package, never typed and never seen: a
// WireGuard private key per site, the cluster's three role passwords, a
// database password per app, and Garage's keys. Pasted secrets are issued by
// somebody else, a DNS token being the example, and captured secrets are minted
// by a running service, an OIDC client secret being the example. This package
// leaves both alone and reports them as missing so an operator knows what is
// still owed.
//
// The rule that matters most here is that nothing already set is ever
// replaced. Regenerating a WireGuard key silently breaks every peer that
// trusted the old one; regenerating a database password leaves an application
// unable to reach a database whose role still has the old one. `init` is
// therefore safe to re-run, and re-running it is the intended way to fill in a
// site or an app added to the configuration later.
package secretsgen

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"regexp"
	"sort"

	"golang.org/x/crypto/curve25519"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// garageKeyIDPattern and garageSecretPattern are the only shapes Garage
// accepts for an S3 access key ID and secret key, established by running
// dxflrs/garage:v1.0.1.
var (
	garageKeyIDPattern  = regexp.MustCompile(`^GK[0-9a-f]{24}$`)
	garageSecretPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Result records what a generation pass did, by name and never by value. A
// secret that reaches a terminal has been written to a scrollback buffer,
// a screen recording, or a terminal multiplexer's log.
type Result struct {
	// Generated names the secrets this pass created, in sorted order.
	Generated []string
	// Kept names the secrets that already had a value and were left alone.
	Kept []string
	// Owed names secrets the toolkit cannot generate, because somebody else
	// issues them or a running service mints them.
	Owed []Owed
}

// Owed is a secret that has to come from outside, with the reason it cannot be
// generated here.
type Owed struct {
	Name string
	Why  string
}

// Changed reports whether anything was generated, so a caller can say "nothing
// to do" rather than claiming to have written a file it did not change.
func (r Result) Changed() bool { return len(r.Generated) > 0 }

// Fill generates every missing generated secret for a configuration, in place.
//
// It is deliberately driven by the configuration rather than by what the file
// already contains: a site or an app added to paisans.yaml is a secret owed,
// and the whole point of re-running init is to notice that.
func Fill(cfg *config.Config, secrets *config.Secrets) (Result, error) {
	var result Result
	note := func(created bool, name string) {
		if created {
			result.Generated = append(result.Generated, name)
			return
		}
		result.Kept = append(result.Kept, name)
	}

	if secrets.Version == 0 {
		secrets.Version = 1
	}

	// The cluster's three roles. Spilo ships published defaults for all three
	// (zalando, cola, standby), so leaving any unset is worse than it looks.
	created, err := fillString(&secrets.Cluster.SuperuserPassword)
	if err != nil {
		return result, err
	}
	note(created, "cluster.superuser_password")

	created, err = fillString(&secrets.Cluster.AdminPassword)
	if err != nil {
		return result, err
	}
	note(created, "cluster.admin_password")

	created, err = fillString(&secrets.Cluster.StandbyPassword)
	if err != nil {
		return result, err
	}
	note(created, "cluster.standby_password")

	// Garage's own two credentials, and only one of them is an opaque string.
	// admin_token is unconstrained: Garage takes it as given, and a base64
	// password starts fine. rpc_secret is not. Garage parses it as a hex
	// encoded 32 byte key and refuses anything else at startup with "Invalid
	// RPC secret key: expected 32 bits of entropy", which is a node that never
	// comes up rather than a node that comes up wrong. Established by booting
	// dxflrs/garage:v1.0.1 against this toolkit's own rendered garage.toml;
	// internal/garage's integration suite boots the rendered file so that a
	// future change here cannot quietly go back to base64.
	created, err = fillString(&secrets.Storage.Garage.AdminToken)
	if err != nil {
		return result, err
	}
	note(created, "storage.garage.admin_token")

	created, err = fillHex(&secrets.Storage.Garage.RPCSecret)
	if err != nil {
		return result, err
	}
	note(created, "storage.garage.rpc_secret")

	// One WireGuard identity per site, kept in the file rather than on the
	// host, so rebuilding a dead machine restores the same identity and no
	// peer is reconfigured.
	if secrets.Sites == nil {
		secrets.Sites = map[string]config.SiteSecrets{}
	}
	for _, name := range cfg.SiteNames() {
		site := secrets.Sites[name]
		if site.WireGuardPrivateKey != "" {
			note(false, "sites."+name+".wireguard_private_key")
			continue
		}
		key, err := wireGuardPrivateKey()
		if err != nil {
			return result, err
		}
		site.WireGuardPrivateKey = key
		secrets.Sites[name] = site
		note(true, "sites."+name+".wireguard_private_key")
	}

	// Every app connects as its own role with its own password. There is no
	// shared fallback, so a missing one is a hard failure at render rather
	// than a quiet reuse of somebody else's credential.
	if secrets.Apps == nil {
		secrets.Apps = map[string]map[string]any{}
	}
	if err := CheckGarageKeys(cfg, secrets); err != nil {
		return result, err
	}
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		entries := secrets.Apps[name]
		if entries == nil {
			entries = map[string]any{}
		}
		for _, key := range appSecretKeys(app) {
			if key == oauthPrivateKey || key == oauthPublicKey {
				continue // a pair, derived from the passphrase; see below
			}
			if existing, ok := entries[key].(string); ok && existing != "" {
				note(false, "apps."+name+"."+key)
				continue
			}
			value, err := appSecret(key)
			if err != nil {
				return result, err
			}
			entries[key] = value
			note(true, "apps."+name+"."+key)
		}
		if app.Kind == config.KindMbin {
			if err := fillOAuthKeypair(name, entries, note); err != nil {
				return result, err
			}
		}
		secrets.Apps[name] = entries
	}

	result.Owed = owed(cfg, secrets)
	sort.Strings(result.Generated)
	sort.Strings(result.Kept)
	return result, nil
}

// appSecretKeys is what one app's stanza needs, which depends on the kind:
// the writefreely-wisp fork has a database role like any other Postgres
// backed kind, and only Mbin runs a message broker, a cache and an OAuth2
// server of its own.
func appSecretKeys(app config.App) []string {
	var keys []string
	if kinds.UsesPostgres(app.Kind) {
		keys = append(keys, "database_password")
	}
	if app.Kind == config.KindMbin {
		// The broker, the cache and the Mercure hub each need one, and so do
		// Symfony (APP_SECRET) and Mbin's own OAuth2 server for API clients
		// (OAUTH_PASSPHRASE for its private key, OAUTH_ENCRYPTION_KEY for the
		// tokens it issues). Upstream's .env.example_docker ships a
		// placeholder for each of the last three, and the image bakes those
		// placeholders in, so leaving one unset runs on a value anyone can
		// read in upstream's repository.
		keys = append(keys, "mercure_jwt_secret", "rabbitmq_password", "valkey_password",
			"app_secret", "oauth_passphrase", "oauth_encryption_key",
			oauthPrivateKey, oauthPublicKey)
	}
	if app.Kind == config.KindOAuth2Proxy {
		keys = append(keys, "cookie_secret")
	}
	if app.Kind == config.KindPocketID {
		// Pocket ID will not start without an ENCRYPTION_KEY of at least 16
		// bytes (backend/internal/common/env_config.go:167-169 at tag
		// v2.14.0). A generated password is 32 random bytes, base64 encoded.
		// It encrypts stored secrets, so like every generated secret it is
		// never replaced once set.
		//
		// static_api_key is how this toolkit administers Pocket ID without a
		// browser: STATIC_API_KEY authenticates the X-API-Key header as a
		// synthetic administrator (apikey/service.go:156-163 and :221-259,
		// middleware/api_key_auth.go:38) and must be at least 16 characters
		// (env_config.go:182-184). README, "The toolkit administers Pocket ID
		// through its static API key", says why it is generated here.
		keys = append(keys, "encryption_key", "static_api_key")
	}
	if app.Kind == config.KindUptime {
		// ADMIN_PASS is the break glass sign in for when the identity
		// provider is down, which is one of the things this app watches; the
		// fork falls back to the literal "admin" when it is unset, so it is
		// generated rather than optional. SESSION_SECRET signs the session
		// cookie; unset, the fork invents one per boot and every restart
		// signs everyone out.
		keys = append(keys, "admin_password", "session_secret")
	}
	if app.Kind == config.KindSynapse {
		// Three, because the homeserver no longer authenticates anyone and
		// Matrix Authentication Service in front of it needs its own.
		keys = append(keys, masEncryptionSecret, masMatrixSecret, masSigningKey)
	}
	if kinds.UsesObjectStorage(app.Kind) {
		// Per app rather than shared: one key for every app would mean each
		// bucket granted to it with --owner, so any one app's .env would be
		// enough to read, rewrite and delete every other app's objects.
		keys = append(keys, "s3_access_key_id", "s3_secret_access_key")
	}
	sort.Strings(keys)
	return keys
}

// The three secrets Matrix Authentication Service needs, named here because
// two of them are not a generated password and the difference has to be
// visible in one place.
const (
	// masEncryptionSecret encrypts database fields and cookies. MAS states the
	// form: "This must be a 32-byte long hex-encoded key", from its
	// configuration reference at tag v1.24.0, checked 2026-09-15. Losing or
	// changing it after members exist makes every encrypted field and cookie
	// unrecoverable, which is why nothing here ever replaces one that is set.
	masEncryptionSecret = "mas_encryption_secret"
	// masMatrixSecret is shared with the homeserver, which carries the same
	// value in matrix_authentication_service.secret. It authenticates the
	// service to the homeserver, so leaking it is an admin compromise.
	masMatrixSecret = "mas_matrix_secret"
	// masSigningKey signs the tokens MAS issues. Its configuration reference
	// says "At least one RSA key must be configured", so this is an RSA key
	// and not one of the elliptic curve types it also accepts.
	masSigningKey = "mas_signing_key"
)

// appSecret produces the value for one app secret key.
//
// Most are a generated password. MAS parses its encryption secret as hex and
// its signing key as PEM, so a base64 password in either position is a
// service that will not start. Garage is stricter still: it refuses a base64
// key ID or secret outright rather than merely failing to start with one, so
// the two S3 credential keys get their own generators too.
func appSecret(key string) (string, error) {
	switch key {
	case masEncryptionSecret:
		return hexSecret(32)
	case masSigningKey:
		return rsaPrivateKeyPEM()
	case "s3_access_key_id":
		return garageKeyID()
	case "s3_secret_access_key":
		return garageSecretKey()
	default:
		return password()
	}
}

// hexSecret is n bytes from the system source, hex encoded, for a consumer
// that parses its secret as hex rather than taking it as an opaque string.
func hexSecret(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// rsaPrivateKeyPEM is a 2048 bit RSA key in PKCS#8 PEM, which is one of the
// formats MAS documents accepting.
//
// 2048 rather than 4096 because it is what an OpenID Connect signing key is
// expected to be, and the key signs short lived tokens rather than protecting
// anything at rest.
func rsaPrivateKeyPEM() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generating an RSA signing key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("encoding the RSA signing key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// Mbin's OAuth2 server keypair, which signs the tokens it issues to API
// clients and mobile apps. The image does not generate one and upstream's
// install documents an operator running openssl by hand
// (docs/02-admin/01-installation/02-docker.md, "Configure OAuth2 keys", at
// tag v1.13.3+paisans). Generating it here means no host step, and keeping it
// in the secrets file means every apps site under cluster placement renders
// the same pair, so a token one site issues verifies on another.
const (
	oauthPrivateKey = "oauth_private_key"
	oauthPublicKey  = "oauth_public_key"
)

// fillOAuthKeypair generates Mbin's keypair when the private key is unset.
//
// The private key is never replaced: every token Mbin has issued is signed by
// it, so a new one logs out every API client and app at once. The public key
// is derived, so a missing one is filled from the private key rather than
// counted as a reason to make a new pair. Deriving it means opening the
// private key, and one the passphrase beside it cannot open is refused rather
// than regenerated, because Mbin would refuse it too, on the host.
func fillOAuthKeypair(app string, entries map[string]any, note func(bool, string)) error {
	passphrase, _ := entries["oauth_passphrase"].(string)
	if passphrase == "" {
		return fmt.Errorf("apps.%s.oauth_passphrase is empty, and the OAuth2 private key is encrypted with it", app)
	}
	privateName := "apps." + app + "." + oauthPrivateKey
	publicName := "apps." + app + "." + oauthPublicKey

	existing, _ := entries[oauthPrivateKey].(string)
	if existing == "" {
		private, public, err := oauthKeypairPEM(passphrase)
		if err != nil {
			return err
		}
		entries[oauthPrivateKey] = private
		entries[oauthPublicKey] = public
		note(true, privateName)
		note(true, publicName)
		return nil
	}
	note(false, privateName)
	if current, _ := entries[oauthPublicKey].(string); current != "" {
		note(false, publicName)
		return nil
	}

	key, err := OpenOAuthPrivateKey(existing, passphrase)
	if err != nil {
		return fmt.Errorf("oauth-key-does-not-open: %s cannot be opened with apps.%s.oauth_passphrase, so its public half cannot be derived and Mbin could not sign a token with it either: %w", privateName, app, err)
	}
	public, err := publicKeyPEM(&key.PublicKey)
	if err != nil {
		return err
	}
	entries[oauthPublicKey] = public
	note(true, publicName)
	return nil
}

// oauthKeypairPEM is a 4096 bit RSA key, encrypted with the passphrase, and
// its public half. Both choices are upstream's: its documented command is
// `openssl genrsa -des3 -out ./storage/oauth/private.pem 4096`, and its
// docker/setup.sh runs the same with -passout set to OAUTH_PASSPHRASE.
//
// The encryption is the traditional PEM form (Proc-Type and DEK-Info headers)
// with AES-256-CBC, which OpenSSL, and so PHP's openssl_pkey_get_private that
// league/oauth2-server calls with OAUTH_PASSPHRASE, reads. Go deprecates
// writing it because decrypting it can be a padding oracle when an attacker
// can submit ciphertexts to it; nothing here does. Encrypted PKCS#8 would need
// a dependency outside the standard library for no difference to Mbin.
//
// It is encrypted, rather than left plain with an unused passphrase, because
// it is rendered 0644: README, "Three kinds of secret", says why.
func oauthKeypairPEM(passphrase string) (string, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", fmt.Errorf("generating Mbin's OAuth2 key: %w", err)
	}
	//nolint:staticcheck // see above for why the deprecated form is the right one
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY",
		x509.MarshalPKCS1PrivateKey(key), []byte(passphrase), x509.PEMCipherAES256)
	if err != nil {
		return "", "", fmt.Errorf("encrypting Mbin's OAuth2 key: %w", err)
	}
	public, err := publicKeyPEM(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), public, nil
}

// OpenOAuthPrivateKey decrypts and parses an OAuth2 private key the way Mbin
// will, so a key that would fail there fails here first. An unencrypted key
// is accepted, as OpenSSL accepts one with a passphrase supplied.
func OpenOAuthPrivateKey(privatePEM, passphrase string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, fmt.Errorf("not PEM")
	}
	der := block.Bytes
	//nolint:staticcheck // reading the form oauthKeypairPEM writes
	if x509.IsEncryptedPEMBlock(block) {
		var err error
		//nolint:staticcheck // reading the form oauthKeypairPEM writes
		der, err = x509.DecryptPEMBlock(block, []byte(passphrase))
		if err != nil {
			return nil, err
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}
	return key, nil
}

// publicKeyPEM is the SubjectPublicKeyInfo form `openssl rsa -pubout` writes.
func publicKeyPEM(key *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", fmt.Errorf("encoding Mbin's OAuth2 public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// owed lists what this package will not invent, with the reason, so that an
// operator finishing an install knows exactly what is left and who issues it.
func owed(cfg *config.Config, secrets *config.Secrets) []Owed {
	var out []Owed
	if secrets.External["acme_dns_token"] == "" && len(cfg.CaddySites()) > 0 {
		out = append(out, Owed{
			Name: "external.acme_dns_token",
			Why: fmt.Sprintf(
				"issued by the DNS provider, %s in this deployment, scoped to this zone only. Certificates use DNS-01, so the gateway, and a monitor serving its own hostname, cannot obtain one without it",
				cfg.ACME.Provider),
		})
	}
	if o, ok := owedSMTPPassword(cfg, secrets); ok {
		out = append(out, o)
	}
	for _, name := range cfg.AppNames() {
		if _, ok := secrets.OIDCClients[name]; ok {
			continue
		}
		if cfg.Apps[name].Kind == config.KindPocketID {
			continue // the identity provider has no client at itself
		}
		if cfg.Apps[name].Kind == config.KindElement {
			// A Matrix client does not authenticate at the identity provider.
			// It authenticates at the homeserver, which delegates to its own
			// authentication service, which is the relying party. Asking an
			// operator to mint a client for it would send them to approve a
			// Pocket ID mutation that nothing would ever use, and its template
			// set reads no OIDC value at all.
			continue
		}
		why := "minted by the identity provider, and creating a client there is a mutation a human approves. The toolkit records the value afterwards rather than automating the approval away"
		if cfg.Apps[name].Kind == config.KindSynapse {
			// Said here rather than only in the rendered configuration,
			// because the rendered configuration is 0600 and does not exist
			// until apply, which is after the operator needed this. A client
			// registered with the wrong redirect URI fails at the end of the
			// first sign in, once the member has already authenticated.
			why += ". Register its redirect URI as " +
				kinds.MASRedirectURI(cfg.Apps[name].Hostname, name) +
				", which is the authentication service's callback for this upstream provider and not the /oauth/callback every other kind uses"
		}
		// A kind with a known client shape has its client created by apply
		// itself, since declaring the app is the approval for it (founder
		// decision, 2026-10-08). The redirect URI and PKCE are still named,
		// because they are what apply creates and what an operator checking
		// an existing client needs to see.
		created := "created at the deployment's Pocket ID by `paisans apply --execute` on a site that runs " + name +
			", which records the client ID and secret here itself before " + name + " starts; declaring the app is the approval. `paisans oidc client create --app " + name +
			"` runs the same step alone. "
		if cfg.Apps[name].Kind == config.KindMbin {
			// The fork's OidcClient extends KnpU's OAuth2PKCEClient and always
			// sends a code challenge (src/Security/Oidc/OidcClient.php:18 at
			// tag v1.13.3+paisans).
			why = created + "The client's redirect URI is " +
				kinds.MbinRedirectURI(cfg.Apps[name].Hostname) + ", with PKCE enabled: Mbin always sends a code challenge"
		}
		if cfg.Apps[name].Kind == config.KindUptime {
			why = created + "The client's redirect URI is https://" + cfg.Apps[name].Hostname +
				"/login/oidc/callback (src/lib/oidc.js:26 in the uptime fork), restricted to the group named in apps." +
				name + ".settings.admin_group, so the identity provider refuses everyone else before the monitor does"
		}
		out = append(out, Owed{Name: "oidc_clients." + name, Why: why})
	}
	return out
}

// owedSMTPPassword is external.smtp_password, owed once any app that sends
// mail resolves an SMTP host and neither that secret nor the app's own
// apps.<app>.smtp_password covers it. Said at init rather than discovered when
// the first alert fails to send.
func owedSMTPPassword(cfg *config.Config, secrets *config.Secrets) (Owed, bool) {
	if secrets.External["smtp_password"] != "" {
		return Owed{}, false
	}
	for _, name := range cfg.AppNames() {
		if !kinds.SendsMail(cfg.Apps[name].Kind) {
			continue
		}
		host := cfg.SMTPFor(name).Host
		if host == "" {
			continue
		}
		if own, _ := secrets.Apps[name]["smtp_password"].(string); own != "" {
			continue
		}
		return Owed{
			Name: "external.smtp_password",
			Why: fmt.Sprintf("issued by the mail provider for %s. %s sends mail through it and has no password of its own under apps.%s.smtp_password",
				host, name, name),
		}, true
	}
	return Owed{}, false
}

// fillString generates into a pointer when it is empty, reporting whether it
// did.
func fillString(into *string) (bool, error) {
	return fillWith(into, password)
}

// fillHex is fillString for a consumer that parses its secret as hex rather
// than taking it as an opaque string. It reuses garageSecretKey rather than
// growing a third generator of the same shape.
func fillHex(into *string) (bool, error) {
	return fillWith(into, garageSecretKey)
}

// fillWith is the rule that matters in this package, in one place: nothing
// already set is ever replaced.
func fillWith(into *string, gen func() (string, error)) (bool, error) {
	if *into != "" {
		return false, nil
	}
	value, err := gen()
	if err != nil {
		return false, err
	}
	*into = value
	return true, nil
}

// garageKeyID returns an S3 access key ID in the only shape Garage accepts:
// the literal "GK" followed by 12 hex encoded bytes. `password()` is not
// reused here, and base64 is exactly why: Garage rejects it outright with
// "The specified key ID is not a valid Garage key ID".
func garageKeyID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return "GK" + hex.EncodeToString(raw), nil
}

// garageSecretKey returns 32 hex encoded bytes, which is the only shape Garage
// accepts for an S3 secret key and also the only shape it accepts for
// rpc_secret in garage.toml. One generator serves both because it is one
// requirement, stated by the same piece of software, and a second copy is a
// second thing to get wrong.
func garageSecretKey() (string, error) {
	return hexSecret(32)
}

// PreviousKeyID and PreviousSecretKey hold an app's S3 key while `storage
// rotate-key` replaces it: the key being retired, kept beside the new one in
// s3_access_key_id and s3_secret_access_key until the new one is proven and
// the old one deleted from Garage. Their presence is what says a rotation is
// in progress, so they live in the encrypted file a resumed run reads rather
// than anywhere on a host.
const (
	PreviousKeyID     = "s3_previous_access_key_id"
	PreviousSecretKey = "s3_previous_secret_access_key"
)

// GarageKeyPair generates one S3 key ID and secret in the shapes Garage
// accepts, the generators `init` uses. Exported for storage rotate-key.
func GarageKeyPair() (keyID, secret string, err error) {
	if keyID, err = garageKeyID(); err != nil {
		return "", "", err
	}
	if secret, err = garageSecretKey(); err != nil {
		return "", "", err
	}
	return keyID, secret, nil
}

// garageKeyIsMalformed refuses an S3 credential Garage will not accept.
//
// Generated credentials cannot trip this: garageKeyID and garageSecretKey
// only ever produce the accepted shape. A hand edited secrets file can, and
// the failure it prevents is a provisioning run that dies halfway through
// with a message about hex encoding, after it has already imported some
// keys.
//
// This lives here rather than as a validate.go rule because validate.Check
// takes only a *config.Config and never sees secrets. Fill is the one place
// that reads a hand written key rather than generating one, so it is the one
// place that can see it to check it.
func garageKeyIsMalformed(appName, key, value string) error {
	switch key {
	case "s3_access_key_id", PreviousKeyID:
		if !garageKeyIDPattern.MatchString(value) {
			return fmt.Errorf("garage-key-is-malformed: apps.%s.%s is %q, which Garage will not accept. A Garage access key ID is the literal \"GK\" followed by exactly 24 lowercase hex characters.", appName, key, value)
		}
	case "s3_secret_access_key", PreviousSecretKey:
		if !garageSecretPattern.MatchString(value) {
			return fmt.Errorf("garage-key-is-malformed: apps.%s.%s is not in the shape Garage accepts. A Garage secret key is exactly 64 lowercase hex characters.", appName, key)
		}
	}
	return nil
}

// CheckGarageKeys refuses a hand edited S3 credential Garage would reject, for
// every app that stores objects and already has one set.
//
// Fill calls this itself, so `init` is covered. It is exported because `init`
// is not the only path that reads a secrets file: `render` and `apply` both
// call config.LoadSecrets directly and never call Fill, so a key hand edited
// into the file after the last `init` would otherwise reach a rendered
// artifact, and from there a host, with nothing ever having looked at it.
// Call this once after loading secrets and before using them for anything
// that reaches a host.
func CheckGarageKeys(cfg *config.Config, secrets *config.Secrets) error {
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if !kinds.UsesObjectStorage(app.Kind) {
			continue
		}
		entries := secrets.Apps[name]
		for _, key := range []string{"s3_access_key_id", "s3_secret_access_key", PreviousKeyID, PreviousSecretKey} {
			value, _ := entries[key].(string)
			if value == "" {
				continue
			}
			if err := garageKeyIsMalformed(name, key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// password is 32 bytes from the system source, base64 encoded without padding.
//
// Encoding matters more than it looks: these land in an .env, an INI file, a
// YAML file and a Postgres connection URL, and a raw byte string would need
// different escaping in each. The alphabet here survives all four unescaped
// except for the two characters removed below.
func password() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	return encoded, nil
}

// wireGuardPrivateKey returns a curve25519 private key, base64 encoded, in the
// form WireGuard expects. Clamping is what `wg genkey` does, and an unclamped
// key is not wrong so much as not canonical: the same key would be derived
// either way, but tooling that compares keys would disagree with itself.
func wireGuardPrivateKey() (string, error) {
	raw := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	raw[0] &= 248
	raw[31] &= 127
	raw[31] |= 64
	return base64.StdEncoding.EncodeToString(raw), nil
}

// ClientSecret is a new OIDC client secret, in the same shape as every other
// generated password: 43 characters of unpadded base64url, which is printable
// ASCII and well over the 16 characters Pocket ID requires of a supplied
// secret (dto/oidc_dto.go:80 at tag v2.14.0). It is generated here rather
// than by the identity provider so that it can be recorded before the
// provider is sent it.
func ClientSecret() (string, error) { return password() }

// OwedNames is the names Fill would report as owed, without generating
// anything. `secrets set` uses it to accept exactly the keys an operator has
// been told to supply.
func OwedNames(cfg *config.Config, secrets *config.Secrets) []string {
	var out []string
	for _, o := range owed(cfg, secrets) {
		out = append(out, o.Name)
	}
	return out
}
