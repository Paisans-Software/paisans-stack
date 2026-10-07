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

	// ServerName is the name that appears in a user identifier. It is the
	// hostname holding the wellknown role when the app declares one, and the
	// primary hostname otherwise.
	//
	// Two kinds use it, and they must agree: a homeserver writes it into
	// homeserver.yaml, and an Element client offers it to members as the name
	// they have an account on. Element's is therefore resolved from the
	// homeserver its `homeserver` setting names, not from its own hostname.
	//
	// The reason it is not simply the
	// primary hostname is that delegation exists: a deployment serves
	// /.well-known/matrix/server on the apex so that identifiers read
	// @someone:example.org while the API lives on a subdomain. If the two
	// disagree the delegation documents point at a server that does not answer
	// to the name they claim. Changing it after an account or a room exists is
	// a migration rather than a setting, which is why it is derived from the
	// declared hostnames rather than from anything that can drift.
	ServerName string

	// Domain is the community's own domain, community.domain in the
	// configuration, never an app's own hostname. The oauth2-proxy kind scopes
	// its cookie to it with a leading dot so one login covers every current
	// and future subdomain, the same shape as the live authgate stack's
	// COOKIE_DOMAINS.
	Domain string

	// TrustedProxies is the mesh subnet, never a host address, so the gateway
	// role can move without rewriting every application's configuration.
	TrustedProxies string

	// MeshAddress is the mesh address of the site this instance runs on. A
	// published port binds it and nothing else, and a clustered app reaches
	// its local HAProxy at it. Empty when the gateway rebuilds the app for
	// routing, which never publishes a port or opens a connection.
	MeshAddress string

	// The database this app connects to. A clustered app is given the
	// HAProxy on its own site, at that site's mesh address; a pinned app is
	// given its own container. DSN is the same connection as a URL, for the
	// applications that take one.
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DSN        string

	// PostgresVersion is the cluster's major version, which every database in
	// the deployment runs. Mbin's Doctrine wants it in its DSN.
	PostgresVersion string

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

	// GateMembersPort and GateMembersUpstreams describe the oauth2-proxy
	// kind's second instance, the one enforcing "members". They are populated
	// only for that kind: every other kind's template set has one service on
	// one port, already covered by Upstreams and App.Port, and needs neither.
	GateMembersPort      int
	GateMembersUpstreams []string

	// MASPort, MASUpstreams, MASDBName and MASUpstreamProviderID describe the
	// synapse kind's second container, Matrix Authentication Service. They are
	// populated only for that kind, for the same reason the two fields above
	// are populated only for the gate.
	//
	// MAS is a second service on a second port with a database of its own, and
	// the gateway has to reach it directly: the three compatibility login paths
	// are split off to it in front of the homeserver, so a snippet that only
	// knew the homeserver's address could not route them.
	MASPort               int
	MASUpstreams          []string
	MASDBName             string
	MASUpstreamProviderID string

	secrets map[string]any
	set     map[string]any
}

type s3Values struct {
	Endpoint string

	// MediaHostname is where this app's objects are served from: its own
	// hostname, a sibling of the app's, from kinds.MediaHostname. Empty for a
	// kind that stores nothing.
	MediaHostname string

	// PublicBase is what an app writes into a page, a feed or a federated
	// post: the media hostname, over https, never the mesh endpoint. It has
	// to resolve for a browser and for a server that will never join this
	// mesh, which the endpoint above does not. It carries no bucket segment,
	// because the media hostname is this app's alone. Empty when there is no
	// media hostname, so a kind that stores nothing does not carry a
	// half-built URL.
	PublicBase string

	// GarageAddress is the Garage node the gateway proxies a media hostname
	// to, over the mesh. It is the same node Endpoint names.
	GarageAddress string

	// WebHost is the Host Garage's web endpoint resolves this app's bucket
	// from: the bucket name under an internal suffix that resolves nowhere.
	// Only a media snippet for a kind that serves objects publicly sends it.
	WebHost string

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
	if err := p.requireAppSecrets(planned); err != nil {
		return appValues{}, err
	}

	dbHost, dbPort := p.clusteredDatabaseHost(planned.Site), p.clusterPort()
	if planned.Pinned {
		dbHost, dbPort = "postgres", postgresPort
	}

	v := appValues{
		App:             planned,
		Hostname:        planned.Hostname,
		PublicURL:       "https://" + planned.Hostname,
		Domain:          p.cfg.Community.Domain,
		TrustedProxies:  p.mesh,
		DBHost:          dbHost,
		DBPort:          dbPort,
		DBName:          planned.DBName,
		DBUser:          planned.DBUser,
		DBPassword:      password,
		DSN:             dsn(planned, password, dbHost, dbPort),
		PostgresVersion: p.postgresVersion(),
		DataPath:        "/srv/" + planned.Name,
		MeshAddress:     p.meshAddress(planned.Site),
		secrets:         p.secrets.Apps[planned.Name],
		set:             app.Settings,
	}
	v.ServerName = planned.Hostname
	if delegated := app.Hostnames[kinds.WellknownRole]; delegated != "" {
		v.ServerName = delegated
	}
	if planned.Kind == config.KindElement {
		v.ServerName = p.homeserverServerName(v.Setting("homeserver", planned.Hostname))
	}
	garage := garageEndpointHost(p)
	var s3Endpoint string
	if garage != "" {
		s3Endpoint = fmt.Sprintf("http://%s:3900", garage)
	}
	v.S3 = s3Values{
		// Mbin and the blog write to this address themselves, server side,
		// over the mesh. Outline never uses it: its uploads are a presigned
		// POST the browser submits directly, so for Outline every upload and
		// every read goes through PublicBase, not here.
		Endpoint:       s3Endpoint,
		GarageAddress:  garage,
		AccessKeyID:    v.Secret("s3_access_key_id"),
		SecretKey:      v.Secret("s3_secret_access_key"),
		Bucket:         kinds.BucketName(planned.Name, app),
		Region:         v.Setting("s3_region", "garage"),
		ForcePathStyle: v.SettingBool("s3_force_path_style", true),
	}
	v.S3.WebHost = v.S3.Bucket + webEndpointSuffix
	// Set only for a kind that stores objects, so a kind that stores nothing
	// does not carry the literal string "https://" into a template that has
	// never been written to check.
	if media := kinds.MediaHostname(app, p.cfg.Community.Domain); media != "" {
		v.S3.MediaHostname = media
		v.S3.PublicBase = "https://" + media
	}
	v.OIDC = p.oidcFor(planned)
	v.Upstreams = p.upstreams(planned.Name)
	if planned.Kind == config.KindOAuth2Proxy {
		v.GateMembersPort = gateMembersPort
		v.GateMembersUpstreams = p.upstreamsOnPort(planned.Name, gateMembersPort)
	}
	if planned.Kind == config.KindSynapse {
		v.MASPort = masPort
		v.MASUpstreams = p.upstreamsOnPort(planned.Name, masPort)
		v.MASDBName = planned.DBName + "_mas"
		v.MASUpstreamProviderID = kinds.MASUpstreamProviderID(planned.Name)
	}
	return v, nil
}

// homeserverServerName is the server_name of the homeserver reached at a given
// hostname, or that hostname when no declared homeserver answers on it.
//
// A client has to be told two different names. base_url is where the API is,
// which is the homeserver's primary hostname; server_name is the part after the
// colon in a user identifier, which is the hostname holding the wellknown role
// when the homeserver declares one. Defaulting the second to the first is wrong
// the moment delegation is in use: the client would offer members accounts on a
// name that is not in any identifier the homeserver mints, and it disagreed
// with the homeserver in this repository's own fixture.
//
// It is resolved from the declared deployment rather than made a setting an
// operator types twice, because the two values are not independent: a
// server_name that disagrees with the homeserver's is not a preference, it is a
// broken client.
func (p *planner) homeserverServerName(hostname string) string {
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if app.Kind != config.KindSynapse || app.Hostname != hostname {
			continue
		}
		if delegated := app.Hostnames[kinds.WellknownRole]; delegated != "" {
			return delegated
		}
		return app.Hostname
	}
	return hostname
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
	// The homeserver's client belongs to the authentication service in front
	// of it, which serves one callback path per upstream provider rather than
	// the /oauth/callback every other kind uses.
	redirect := "https://" + planned.Hostname + "/oauth/callback"
	switch planned.Kind {
	case config.KindSynapse:
		redirect = kinds.MASRedirectURI(planned.Hostname, planned.Name)
	case config.KindMbin:
		redirect = kinds.MbinRedirectURI(planned.Hostname)
	}
	return oidcValues{
		Present:      true,
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		RedirectURL:  redirect,

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

// webEndpointSuffix is the internal suffix Garage's s3_web endpoint resolves
// a bucket from, and that nothing on the mesh or off it otherwise resolves.
// It must match the root_domain rendered into garage.toml.
const webEndpointSuffix = ".web.garage.internal"

// quote renders a value into a double quoted YAML or INI string. Templates use
// it wherever a secret lands in a structured file, since a generated password
// can legitimately contain a character the format would otherwise read.
func quote(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(s) + `"`
}
