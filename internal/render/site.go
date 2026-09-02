package render

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/crypto/curve25519"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.tmpl"))

// image is the container image for an application kind. Versions are pinned
// here rather than floating, because a deployment that changes underneath an
// operator is a deployment nobody can reason about.
var image = map[string]string{
	"mbin":        "ghcr.io/mbinorg/mbin:latest",
	"outline":     "docker.getoutline.com/outlinewiki/outline:latest",
	"pocket-id":   "ghcr.io/pocket-id/pocket-id:latest",
	"synapse":     "ghcr.io/element-hq/synapse:latest",
	"writefreely": "ghcr.io/writefreely/writefreely:latest",
}

type clusterMember struct {
	Name    string
	Address string
}

type peerView struct {
	Name       string
	PublicKey  string
	AllowedIPs string
	Endpoint   string
}

type route struct {
	Hostname  string
	Upstreams []string
}

func (p *planner) renderSite(site *siteView) ([]File, error) {
	var files []File
	base := site.Name + "/"

	wg, err := p.renderWireGuard(site)
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: base + "etc/wireguard/wg0.conf", Content: wg, Mode: 0o600})

	infra, err := p.renderTemplate("infra-compose.yaml.tmpl", map[string]any{
		"Site":               site,
		"Scope":              p.scope(),
		"EtcdInitialCluster": p.etcdInitialCluster(),
		"HeartbeatMS":        p.heartbeatMS(),
		"ElectionTimeoutMS":  p.electionTimeoutMS(),
		"PostgresVersion":    p.postgresVersion(),
	})
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: base + "srv/infra/compose.yaml", Content: infra, Mode: 0o644})

	if site.IsData {
		env, err := p.renderTemplate("patroni.env.tmpl", map[string]any{
			"Site":              site,
			"Scope":             p.scope(),
			"EtcdClientHosts":   p.etcdClientHosts(),
			"PatroniAPIPort":    patroniAPIPort,
			"PostgresPort":      postgresPort,
			"SuperuserPassword": p.secrets.Cluster.SuperuserPassword,
			"StandbyPassword":   p.secrets.Cluster.StandbyPassword,
			"AdminPassword":     p.secrets.Cluster.AdminPassword,
			"Synchronous":       boolString(p.cfg.Cluster.Synchronous),
			"SynchronousStrict": boolString(p.cfg.Cluster.SynchronousStrict),
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/patroni.env", Content: env, Mode: 0o600})
	}

	if site.NeedsProxy {
		cfg, err := p.renderTemplate("haproxy.cfg.tmpl", map[string]any{
			"ClusterPort":    p.clusterPort(),
			"ClusterMembers": p.clusterMembers(),
			"PostgresPort":   postgresPort,
			"PatroniAPIPort": patroniAPIPort,
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/haproxy/haproxy.cfg", Content: cfg, Mode: 0o644})
	}

	if site.IsGarage {
		rpcSecret := p.secrets.Storage.Garage.RPCSecret
		if rpcSecret == "" {
			rpcSecret = p.secrets.Storage.Garage.AdminToken
		}
		toml, err := p.renderTemplate("garage.toml.tmpl", map[string]any{
			"Site":        site,
			"Domain":      p.cfg.Community.Domain,
			"Replication": p.replication(),
			"RPCSecret":   rpcSecret,
			"AdminToken":  p.secrets.Storage.Garage.AdminToken,
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/garage/garage.toml", Content: toml, Mode: 0o600})
	}

	if site.IsGateway {
		caddyfile, err := p.renderTemplate("Caddyfile.tmpl", map[string]any{
			"Domain": p.cfg.Community.Domain,
			"Routes": p.routes(),
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/caddy/Caddyfile", Content: caddyfile, Mode: 0o644})

		env, err := p.renderTemplate("caddy.env.tmpl", map[string]any{
			"CloudflareAPIToken": p.secrets.External["cloudflare_api_token"],
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/caddy/caddy.env", Content: env, Mode: 0o600})
	}

	for _, app := range site.Apps {
		compose, err := p.renderTemplate("app-compose.yaml.tmpl", map[string]any{
			"App":             app,
			"Image":           image[string(app.Kind)],
			"ClusterPort":     p.clusterPort(),
			"PostgresVersion": p.postgresVersion(),
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/" + app.Name + "/compose.yaml", Content: compose, Mode: 0o644})

		env, err := p.renderTemplate("app.env.tmpl", map[string]any{"Env": app.Env})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/" + app.Name + "/.env", Content: env, Mode: 0o600})
	}

	return files, nil
}

func (p *planner) renderTemplate(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return buf.String(), nil
}

// renderWireGuard builds one site's interface and peer list.
//
// A site with a stable endpoint is dialled by everyone else. Two sites that
// both lack one cannot dial each other, so their traffic relays through a site
// that has one. Relaying keeps this to static configuration with no daemon and
// no coordination service, which is why plain WireGuard was chosen.
func (p *planner) renderWireGuard(site *siteView) (string, error) {
	private := p.secrets.Sites[site.Name].WireGuardPrivateKey
	if _, err := publicKey(private); err != nil {
		return "", fmt.Errorf("secrets sites.%s.wireguard_private_key: %w", site.Name, err)
	}

	relays := p.relays()
	isRelay := site.Endpoint != ""

	var peers []peerView
	assignedMesh := false
	for _, name := range p.order {
		if name == site.Name {
			continue
		}
		other := p.sites[name]
		otherPublic, err := publicKey(p.secrets.Sites[name].WireGuardPrivateKey)
		if err != nil {
			return "", fmt.Errorf("secrets sites.%s.wireguard_private_key: %w", name, err)
		}
		switch {
		case other.Endpoint != "":
			// The other side has a stable address, so this side dials it.
			allowed := other.Address + "/32"
			if !isRelay && !assignedMesh && contains(relays, name) {
				// Route everything not directly reachable through the first
				// relay, in sorted order so the choice is deterministic.
				allowed = p.mesh
				assignedMesh = true
			}
			peers = append(peers, peerView{
				Name:       name,
				PublicKey:  otherPublic,
				AllowedIPs: allowed,
				Endpoint:   other.Endpoint,
			})
		case isRelay:
			// This side is dialled by the other, so no endpoint is known here.
			peers = append(peers, peerView{
				Name:       name,
				PublicKey:  otherPublic,
				AllowedIPs: other.Address + "/32",
			})
		default:
			// Neither side has an endpoint. Nothing to configure: the traffic
			// goes through a relay.
		}
	}

	prefix := strings.SplitN(p.mesh, "/", 2)
	meshPrefix := "24"
	if len(prefix) == 2 {
		meshPrefix = prefix[1]
	}
	return p.renderTemplate("wg0.conf.tmpl", map[string]any{
		"Site":       site,
		"PrivateKey": private,
		"MeshPrefix": meshPrefix,
		"IsRelay":    isRelay,
		"Peers":      peers,
	})
}

// publicKey derives a WireGuard public key from a private key. Keys are
// generated on the workstation and kept in the encrypted file, so rebuilding a
// dead node restores the same identity and no peer is reconfigured.
func publicKey(private string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(private))
	if err != nil {
		return "", fmt.Errorf("is not base64. WireGuard keys are 32 bytes, base64 encoded: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("decodes to %d bytes, but a WireGuard key is 32", len(raw))
	}
	pub, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("is not a usable curve25519 key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

func (p *planner) relays() []string {
	var out []string
	for _, name := range p.order {
		if p.sites[name].Endpoint != "" {
			out = append(out, name)
		}
	}
	return out
}

// routes is the gateway's inventory: a pinned app points at its location, and
// a clustered app points at every site holding the apps role.
func (p *planner) routes() []route {
	var out []route
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		var upstreams []string
		port := appPort[app.Kind]
		if app.Placement.Mode == "pinned" {
			site, ok := p.sites[app.Placement.Site]
			if !ok {
				continue
			}
			upstreams = []string{fmt.Sprintf("%s:%d", site.Address, port)}
		} else {
			for _, hostName := range p.cfg.AppsSites() {
				upstreams = append(upstreams, fmt.Sprintf("%s:%d", p.sites[hostName].Address, port))
			}
		}
		out = append(out, route{Hostname: app.Hostname, Upstreams: upstreams})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}

func (p *planner) clusterMembers() []clusterMember {
	names := append([]string(nil), p.cfg.Cluster.Sites...)
	sort.Strings(names)
	var out []clusterMember
	for _, name := range names {
		site, ok := p.sites[name]
		if !ok {
			continue
		}
		out = append(out, clusterMember{Name: name, Address: site.Address})
	}
	return out
}

func (p *planner) etcdInitialCluster() string {
	names := append([]string(nil), p.cfg.Etcd.Members...)
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		site, ok := p.sites[name]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=http://%s:2380", name, site.Address))
	}
	return strings.Join(parts, ",")
}

func (p *planner) etcdClientHosts() string {
	names := append([]string(nil), p.cfg.Etcd.Members...)
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		site, ok := p.sites[name]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:2379", site.Address))
	}
	return strings.Join(parts, ",")
}

func (p *planner) scope() string {
	return "paisans"
}

func (p *planner) postgresVersion() string {
	if p.cfg.Cluster.PostgresVersion == "" {
		return "18"
	}
	return p.cfg.Cluster.PostgresVersion
}

func (p *planner) replication() int {
	if p.cfg.Storage.Garage.Replication == 0 {
		return 1
	}
	return p.cfg.Storage.Garage.Replication
}

func (p *planner) heartbeatMS() int {
	if p.cfg.Etcd.HeartbeatMS == 0 {
		return 100
	}
	return p.cfg.Etcd.HeartbeatMS
}

func (p *planner) electionTimeoutMS() int {
	if p.cfg.Etcd.ElectionTimeoutMS == 0 {
		return 1000
	}
	return p.cfg.Etcd.ElectionTimeoutMS
}
