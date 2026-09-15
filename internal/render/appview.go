package render

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// appValues is everything a kind's template set may need about one app.
//
// It exists because the template set, not this package, decides what an
// application's configuration looks like. Go works out the values that require
// the whole deployment to know: which host holds the database, what the
// object storage endpoint is, which secret belongs to this app. The templates
// decide which of those go into which file, under what names, in what format.
//
// The alternative was a switch on kind that assembled environment variables
// here, which is what this replaces. It could only ever produce one file per
// app, and two of the five applications this toolkit renders are not
// configured by environment variables at all.
type appValues struct {
	App plannedApp

	// Hostname is the public name, and PublicURL the same with a scheme. Both
	// are here because roughly half the settings that want one want the other.
	Hostname  string
	PublicURL string

	// TrustedProxies is the mesh subnet, never a host address, so the gateway
	// role can move without rewriting every application's configuration.
	TrustedProxies string

	// The database this app connects to. A clustered app is given the local
	// HAProxy; a pinned app is given its own container. DSN is the same
	// connection as a URL, for the applications that take one.
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DSN        string

	// S3 is object storage, which is Garage over the mesh.
	S3 s3Values

	// OIDC is this app's client at the identity provider, empty when the app
	// has no client declared in the secrets.
	OIDC oidcValues

	// DataPath is the app's bind mounted directory on the host. Templates that
	// mount a rendered file need it to say where the file lives.
	DataPath string

	// Upstreams is where the gateway sends this app's traffic: one address for
	// a pinned app, every apps site for a clustered one. A snippet never
	// hardcodes a location, which is what keeps a pinned app and a clustered
	// app the same shape.
	Upstreams []string

	secrets map[string]any
	set     map[string]any
}

type s3Values struct {
	Endpoint       string
	AccessKeyID    string
	SecretKey      string
	Bucket         string
	Region         string
	ForcePathStyle bool
}

// oidcValues is the app's own client, plus where the identity provider is.
//
// Endpoints are spelled out rather than left to discovery because two of the
// applications here want them individually: WriteFreely's `[oauth.generic]`
// takes an auth, token and inspect endpoint, and neither it nor Synapse can be
// given only an issuer in every version. The paths are Pocket ID's, read from
// a running instance's discovery document rather than recalled.
type oidcValues struct {
	Present      bool
	ClientID     string
	ClientSecret string
	RedirectURL  string

	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	UserinfoEndpoint      string

	// The same three as paths. WriteFreely stores a host and appends a path to
	// it (oauth.go builds AuthLocation as Host + AuthEndpoint), so giving it an
	// absolute URL there produces a doubled host and a sign in that fails at
	// the redirect.
	AuthorizationPath string
	TokenPath         string
	UserinfoPath      string
}

// Secret returns one of this app's secrets by key, empty when unset. A
// template asking for a secret that does not exist renders an empty value
// rather than failing, because the applications differ in which they need and
// an absent optional secret is not an error.
func (v appValues) Secret(key string) string {
	value, _ := v.secrets[key].(string)
	return value
}

// Setting returns a declared setting, or the fallback.
func (v appValues) Setting(key, fallback string) string {
	if s, ok := v.set[key].(string); ok && s != "" {
		return s
	}
	return fallback
}

// SettingBool returns a declared boolean setting, or the fallback.
func (v appValues) SettingBool(key string, fallback bool) bool {
	if b, ok := v.set[key].(bool); ok {
		return b
	}
	return fallback
}

// Image returns the reference a service runs, so a compose template never
// decides for itself.
func (v appValues) Image(service string) string { return v.App.Images[service] }

// values assembles what the template set is given.
func (p *planner) values(planned plannedApp, app config.App) (appValues, error) {
	var password string
	if kinds.UsesPostgres(planned.Kind) {
		var err error
		password, err = p.appDatabasePassword(planned)
		if err != nil {
			return appValues{}, err
		}
	}

	dbHost, dbPort := databaseHost, p.clusterPort()
	if planned.Pinned {
		dbHost, dbPort = "postgres", postgresPort
	}

	garage := p.secrets.Storage.Garage
	v := appValues{
		App:            planned,
		Hostname:       planned.Hostname,
		PublicURL:      "https://" + planned.Hostname,
		TrustedProxies: p.mesh,
		DBHost:         dbHost,
		DBPort:         dbPort,
		DBName:         planned.DBName,
		DBUser:         planned.DBUser,
		DBPassword:     password,
		DSN:            dsn(planned, password, dbHost, dbPort),
		DataPath:       "/srv/" + planned.Name,
		secrets:        p.secrets.Apps[planned.Name],
		set:            app.Settings,
	}
	v.S3 = s3Values{
		Endpoint:       fmt.Sprintf("http://%s:3900", garageEndpointHost(p)),
		AccessKeyID:    garage.AccessKeyID,
		SecretKey:      garage.SecretAccessKey,
		Bucket:         v.Setting("s3_bucket", planned.Name+"-uploads"),
		Region:         v.Setting("s3_region", "garage"),
		ForcePathStyle: v.SettingBool("s3_force_path_style", true),
	}
	v.OIDC = p.oidcFor(planned)
	v.Upstreams = p.upstreams(planned.Name)
	return v, nil
}

// dsn is the app's connection URL, empty for a kind that uses no Postgres so
// that a template cannot render a connection string to a database nobody
// created.
func dsn(planned plannedApp, password, host string, port int) string {
	if !kinds.UsesPostgres(planned.Kind) {
		return ""
	}
	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s", planned.DBUser, password, host, port, planned.DBName)
}

// oidcFor builds the app's identity provider client.
//
// The issuer is the Pocket ID app declared in this same configuration, because
// there is exactly one identity provider in this design and hardcoding its URL
// anywhere else would be a second place to change it.
func (p *planner) oidcFor(planned plannedApp) oidcValues {
	client, ok := p.secrets.OIDCClients[planned.Name]
	if !ok {
		return oidcValues{}
	}
	issuer := p.identityProviderURL()
	return oidcValues{
		Present:      true,
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		RedirectURL:  "https://" + planned.Hostname + "/oauth/callback",

		Issuer:                issuer,
		AuthorizationEndpoint: issuer + authorizationPath,
		TokenEndpoint:         issuer + tokenPath,
		UserinfoEndpoint:      issuer + userinfoPath,

		AuthorizationPath: authorizationPath,
		TokenPath:         tokenPath,
		UserinfoPath:      userinfoPath,
	}
}

// The identity provider's OIDC endpoints, as paths. Read from a running Pocket
// ID v2.14.0's discovery document on 2026-09-08 rather than recalled.
const (
	authorizationPath = "/authorize"
	tokenPath         = "/api/oidc/token"
	userinfoPath      = "/api/oidc/userinfo"
)

// identityProviderURL is the Pocket ID app's public URL, empty when the
// deployment declares none.
func (p *planner) identityProviderURL() string {
	for _, name := range p.cfg.AppNames() {
		if p.cfg.Apps[name].Kind == config.KindPocketID {
			return "https://" + p.cfg.Apps[name].Hostname
		}
	}
	return ""
}

// quote renders a value into a double quoted YAML or INI string. Templates use
// it wherever a secret lands in a structured file, since a generated password
// can legitimately contain a character the format would otherwise read.
func quote(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(s) + `"`
}
