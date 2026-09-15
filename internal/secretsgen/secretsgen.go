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
	"encoding/base64"
	"fmt"
	"sort"

	"golang.org/x/crypto/curve25519"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
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

	for _, field := range []struct {
		name string
		into *string
	}{
		{"storage.garage.admin_token", &secrets.Storage.Garage.AdminToken},
		{"storage.garage.rpc_secret", &secrets.Storage.Garage.RPCSecret},
		{"storage.garage.access_key_id", &secrets.Storage.Garage.AccessKeyID},
		{"storage.garage.secret_access_key", &secrets.Storage.Garage.SecretAccessKey},
	} {
		created, err := fillString(field.into)
		if err != nil {
			return result, err
		}
		note(created, field.name)
	}

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
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		entries := secrets.Apps[name]
		if entries == nil {
			entries = map[string]any{}
		}
		for _, key := range appSecretKeys(app) {
			if existing, ok := entries[key].(string); ok && existing != "" {
				note(false, "apps."+name+"."+key)
				continue
			}
			value, err := password()
			if err != nil {
				return result, err
			}
			entries[key] = value
			note(true, "apps."+name+"."+key)
		}
		secrets.Apps[name] = entries
	}

	result.Owed = owed(cfg, secrets)
	sort.Strings(result.Generated)
	sort.Strings(result.Kept)
	return result, nil
}

// appSecretKeys is what one app's stanza needs, which depends on the kind:
// WriteFreely has no database role, and only Mbin runs a message broker and a
// cache of its own.
func appSecretKeys(app config.App) []string {
	var keys []string
	if kinds.UsesPostgres(app.Kind) {
		keys = append(keys, "database_password")
	}
	if app.Kind == config.KindMbin {
		keys = append(keys, "mercure_jwt_secret", "rabbitmq_password", "valkey_password")
	}
	sort.Strings(keys)
	return keys
}

// owed lists what this package will not invent, with the reason, so that an
// operator finishing an install knows exactly what is left and who issues it.
func owed(cfg *config.Config, secrets *config.Secrets) []Owed {
	var out []Owed
	if secrets.External["cloudflare_api_token"] == "" && len(cfg.GatewaySites()) > 0 {
		out = append(out, Owed{
			Name: "external.cloudflare_api_token",
			Why:  "issued by the DNS provider, scoped to this zone only. Certificates use DNS-01, so the gateway cannot obtain one without it",
		})
	}
	for _, name := range cfg.AppNames() {
		if _, ok := secrets.OIDCClients[name]; ok {
			continue
		}
		if cfg.Apps[name].Kind == config.KindPocketID {
			continue // the identity provider has no client at itself
		}
		out = append(out, Owed{
			Name: "oidc_clients." + name,
			Why:  "minted by the identity provider, and creating a client there is a mutation a human approves. The toolkit records the value afterwards rather than automating the approval away",
		})
	}
	return out
}

// fillString generates into a pointer when it is empty, reporting whether it
// did.
func fillString(into *string) (bool, error) {
	if *into != "" {
		return false, nil
	}
	value, err := password()
	if err != nil {
		return false, err
	}
	*into = value
	return true, nil
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
