package render

import (
	"fmt"
	"sort"

	"github.com/josephquigley/paisans-stack/internal/config"
)

// databaseHost is what every clustered application connects to. It is the
// local HAProxy, from the first install, with a backend list that initially
// has one entry. Adding a site grows that list and no application
// configuration changes, ever.
const databaseHost = "127.0.0.1"

// appEnv assembles the environment for one app.
//
// Two rules run through all of it. Trusted proxies are the mesh subnet and
// never a specific host, so the gateway role can move later without breaking
// client address handling. And a clustered app is given the local proxy
// address, while a pinned app is given its own container: an app that is
// pinned must be pinned all the way down, because a half pinned app needs two
// sites to be up and is less available than either.
func (p *planner) appEnv(planned plannedApp, app config.App) ([]envVar, error) {
	vars := map[string]string{}
	set := func(k, v string) { vars[k] = v }

	password, err := p.appDatabasePassword(planned)
	if err != nil {
		return nil, err
	}

	dbHost, dbPort := databaseHost, p.clusterPort()
	if planned.Pinned {
		dbHost, dbPort = "postgres", postgresPort
	}
	dsn := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s", planned.DBUser, password, dbHost, dbPort, planned.DBName)
	publicURL := "https://" + planned.Hostname

	set("PAISANS_APP", planned.Name)
	set("PAISANS_SITE", planned.Site)
	set("TRUSTED_PROXIES", p.mesh)

	garage := p.secrets.Storage.Garage
	s3Endpoint := fmt.Sprintf("http://%s:3900", garageEndpointHost(p))

	switch app.Kind {
	case config.KindMbin:
		set("SERVER_NAME", planned.Hostname)
		set("KBIN_DOMAIN", planned.Hostname)
		set("DATABASE_URL", dsn)
		set("MERCURE_JWT_SECRET", p.appSecret(planned.Name, "mercure_jwt_secret"))
		set("RABBITMQ_PASSWORD", p.appSecret(planned.Name, "rabbitmq_password"))
		set("VALKEY_PASSWORD", p.appSecret(planned.Name, "valkey_password"))
		set("S3_KEY", garage.AccessKeyID)
		set("S3_SECRET", garage.SecretAccessKey)
		set("S3_BUCKET", settingString(app, "s3_bucket", planned.Name+"-uploads"))
		set("S3_REGION", settingString(app, "s3_region", "garage"))
		set("S3_ENDPOINT", s3Endpoint)
		p.setOIDC(set, planned.Name, publicURL)
	case config.KindOutline:
		set("URL", publicURL)
		set("DATABASE_URL", dsn)
		set("FILE_STORAGE", "s3")
		set("AWS_ACCESS_KEY_ID", garage.AccessKeyID)
		set("AWS_SECRET_ACCESS_KEY", garage.SecretAccessKey)
		set("AWS_REGION", settingString(app, "s3_region", "garage"))
		set("AWS_S3_UPLOAD_BUCKET_NAME", settingString(app, "s3_bucket", planned.Name+"-uploads"))
		set("AWS_S3_UPLOAD_BUCKET_URL", s3Endpoint)
		// Non AWS endpoints need path style addressing.
		set("AWS_S3_FORCE_PATH_STYLE", boolString(settingBool(app, "s3_force_path_style", true)))
		p.setOIDC(set, planned.Name, publicURL)
	case config.KindPocketID:
		set("APP_URL", publicURL)
		set("DB_CONNECTION_STRING", dsn)
		set("DB_PROVIDER", "postgres")
		// database keeps roughly a megabyte of avatars and branding on
		// streaming replication, so a promoted site already has them.
		set("FILE_BACKEND", settingString(app, "file_backend", "database"))
		set("TRUST_PROXY", "true")
	case config.KindSynapse:
		set("SYNAPSE_SERVER_NAME", planned.Hostname)
		set("SYNAPSE_REPORT_STATS", "no")
		set("POSTGRES_HOST", dbHost)
		set("POSTGRES_PORT", fmt.Sprint(dbPort))
		set("POSTGRES_DB", planned.DBName)
		set("POSTGRES_USER", planned.DBUser)
		set("POSTGRES_PASSWORD", password)
		// The S3 storage provider supplements the media store rather than
		// replacing it, so a local media directory is always required.
		set("SYNAPSE_MEDIA_STORE_PATH", "/data/media_store")
	case config.KindWriteFreely:
		set("WF_HOST", publicURL)
		set("WF_DATABASE_URL", dsn)
		// WriteFreely has no object storage support. Its uploads are node
		// local and are not covered by Garage.
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]envVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, envVar{Key: k, Value: vars[k]})
	}
	return out, nil
}

func (p *planner) setOIDC(set func(string, string), app, publicURL string) {
	client, ok := p.secrets.OIDCClients[app]
	if !ok {
		return
	}
	set("OIDC_CLIENT_ID", client.ClientID)
	set("OIDC_CLIENT_SECRET", client.ClientSecret)
	set("OIDC_REDIRECT_URL", publicURL+"/oauth/callback")
}

// appDatabasePassword prefers a per app credential and falls back to the
// cluster admin password. A pinned app runs its own Postgres and should have
// its own credential, so the fallback is reported rather than silent.
func (p *planner) appDatabasePassword(planned plannedApp) (string, error) {
	if secret := p.appSecret(planned.Name, "database_password"); secret != "" {
		return secret, nil
	}
	if p.secrets.Cluster.AdminPassword == "" {
		return "", fmt.Errorf("secrets: apps.%s.database_password is unset and cluster.admin_password is empty. Set one of them", planned.Name)
	}
	return p.secrets.Cluster.AdminPassword, nil
}

func (p *planner) appSecret(app, key string) string {
	entries, ok := p.secrets.Apps[app]
	if !ok {
		return ""
	}
	value, _ := entries[key].(string)
	return value
}

func (p *planner) clusterPort() int {
	if p.cfg.Cluster.Port == 0 {
		return 5000
	}
	return p.cfg.Cluster.Port
}

// garageEndpointHost is the address applications use to reach object storage.
// It is the first Garage site in sorted order, over the mesh.
func garageEndpointHost(p *planner) string {
	sites := append([]string(nil), p.cfg.Storage.Garage.Sites...)
	sort.Strings(sites)
	for _, name := range sites {
		if site, ok := p.sites[name]; ok {
			return site.Address
		}
	}
	return databaseHost
}

func settingString(app config.App, key, fallback string) string {
	if v, ok := app.Settings[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func settingBool(app config.App, key string, fallback bool) bool {
	if v, ok := app.Settings[key].(bool); ok {
		return v
	}
	return fallback
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
