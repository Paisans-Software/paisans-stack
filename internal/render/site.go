package render

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/crypto/curve25519"

	"github.com/josephquigley/paisans-stack/internal/config"
)

// The `all:` prefix matters: without it embed skips files beginning with a
// dot, and every kind configured by environment ships a `.env` template.
//
//go:embed all:templates
var templateFS embed.FS

// templates holds the infrastructure templates, which are shared by every
// deployment. An application's templates are not here: each kind owns a
// directory under templates/, rendered as a set, so that adding an application
// adds a directory rather than a branch in a file every other application
// shares.
var templates = template.Must(template.New("infra").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.tmpl"))

// templateFuncs is the whole function surface a template gets. It is small on
// purpose: logic that needs more than this belongs in Go, where it can be
// tested.
var templateFuncs = template.FuncMap{
	"quote": quote,
	"yesno": func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	},
}

// secretSuffix marks a template whose rendered file is written 0600.
//
// The mode travels with the template rather than in a table somewhere else,
// for the same reason the destination path does: a file and the facts about it
// should not be able to drift apart. Everything else is 0644, because a
// rendered file a container's own user cannot read is a stack that does not
// start.
const secretSuffix = ".secret"

// spiloTag is the Spilo image tag for a Postgres major version.
//
// Spilo publishes one repository per major version and the tags do not run in
// step across them, so a single tag cannot serve every version and `latest`
// cannot serve any: the toolkit refuses a floating tag in an operator's
// configuration and must not render one itself. Each was checked against ghcr
// on 2026-09-08.
//
// A major version with no entry is an error rather than a guess, because
// guessing produces a compose file that pulls nothing and fails on the host.
var spiloTag = map[string]string{
	"16": "3.3-p3",
	"17": "4.0-p3",
	"18": "4.1-p2",
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
	App       string
	Hostname  string
	Upstreams []string
	// Snippet is where the app's own routing lives on the gateway, as the
	// container sees it. The host block imports it rather than containing it.
	Snippet string
}

func (p *planner) renderSite(site *siteView) ([]File, error) {
	var files []File
	base := site.Name + "/"

	wg, err := p.renderWireGuard(site)
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: base + "etc/wireguard/wg0.conf", Content: wg, Mode: 0o600})

	spilo, ok := spiloTag[p.postgresVersion()]
	if !ok {
		return nil, fmt.Errorf(
			"cluster.postgres_version: %q has no Spilo image known to this toolkit. Spilo publishes one repository per major version with its own tags, so there is nothing to fall back to. Use one of %s, or add the tag",
			p.postgresVersion(), strings.Join(sortedKeys(spiloTag), ", "))
	}
	infra, err := p.renderTemplate("infra-compose.yaml.tmpl", map[string]any{
		"Site":               site,
		"Scope":              p.scope(),
		"EtcdInitialCluster": p.etcdInitialCluster(),
		"HeartbeatMS":        p.heartbeatMS(),
		"ElectionTimeoutMS":  p.electionTimeoutMS(),
		"PostgresVersion":    p.postgresVersion(),
		"SpiloTag":           spilo,
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
			"Domain":         p.cfg.Community.Domain,
			"Routes":         p.routes(),
			"TrustedProxies": p.mesh,
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/caddy/Caddyfile", Content: caddyfile, Mode: 0o644})

		snippets, err := p.renderSnippets(base)
		if err != nil {
			return nil, err
		}
		files = append(files, snippets...)

		env, err := p.renderTemplate("caddy.env.tmpl", map[string]any{
			"CloudflareAPIToken": p.secrets.External["cloudflare_api_token"],
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + "srv/infra/caddy/caddy.env", Content: env, Mode: 0o600})
	}

	for _, app := range site.Apps {
		appFiles, err := p.renderTemplateSet(base, app)
		if err != nil {
			return nil, err
		}
		files = append(files, appFiles...)
	}

	return files, nil
}

// snippetTemplate is the file in a kind's set that describes how the gateway
// routes to it. It is rendered onto the gateway rather than beside the app,
// because that is where it is read.
const snippetTemplate = "caddy.snippet.tmpl"

// snippetDir is where snippets land on the gateway, and snippetMount is the
// same directory as the Caddy container sees it. The Caddyfile imports by the
// second, because Caddy reads it from inside the container.
const (
	snippetDir   = "srv/infra/caddy/snippets/"
	snippetMount = "/etc/caddy/snippets/"
)

// renderSnippets renders every app's routing onto a gateway.
//
// It walks the whole configuration rather than the gateway's own apps: a
// gateway routes to applications that run elsewhere, which is the usual case.
func (p *planner) renderSnippets(base string) ([]File, error) {
	var files []File
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		planned, err := p.plannedFor(name, app)
		if err != nil {
			return nil, err
		}
		values, err := p.values(planned, app)
		if err != nil {
			return nil, err
		}
		path := "templates/" + string(app.Kind) + "/" + snippetTemplate
		content, err := p.renderFile(path, values)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + snippetDir + name + ".caddy", Content: content, Mode: 0o644})
	}
	return files, nil
}

// plannedFor rebuilds an app's planned form for the gateway, which needs its
// hostname and port without caring where it runs.
func (p *planner) plannedFor(name string, app config.App) (plannedApp, error) {
	return plannedApp{
		Name:     name,
		Kind:     app.Kind,
		Hostname: app.Hostname,
		Pinned:   app.Placement.Mode == config.PlacementPinned,
		Site:     app.Placement.Site,
		Port:     appPort[app.Kind],
		DBName:   dbIdentifier(name),
		DBUser:   dbIdentifier(name),
		Images:   p.appImages(app),
	}, nil
}

// TemplateSet lists the templates a kind ships, as paths inside the embedded
// filesystem. It exists so a test can assert that a set is complete: embed sees
// what git checked out, so a template left uncommitted is absent here even
// though it is present on the machine that wrote it.
func TemplateSet(kind config.Kind) ([]string, error) {
	dir := "templates/" + string(kind)
	var out []string
	err := fs.WalkDir(templateFS, dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("kind %s ships no template set: %w", kind, err)
	}
	sort.Strings(out)
	return out, nil
}

// renderTemplateSet renders every file in a kind's template directory.
//
// A template's path is its destination: templates/<kind>/config/packages/x.yaml
// lands at /srv/<stack>/config/packages/x.yaml, so nothing holds a separate
// mapping of template to location and a new file in a set needs no code.
func (p *planner) renderTemplateSet(base string, app plannedApp) ([]File, error) {
	dir := "templates/" + string(app.Kind)
	entries, err := fs.ReadDir(templateFS, dir)
	if err != nil {
		return nil, fmt.Errorf("kind %s ships no template set: %w", app.Kind, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("kind %s ships an empty template set, so the stack would be rendered with no configuration at all", app.Kind)
	}

	values, err := p.values(app, p.cfg.Apps[app.Name])
	if err != nil {
		return nil, err
	}

	var files []File
	err = fs.WalkDir(templateFS, dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".tmpl") {
			return fmt.Errorf("%s is in a template set but is not a template. Every file in a set is rendered, so there is nowhere for a plain file to go", path)
		}
		if strings.TrimPrefix(path, dir+"/") == snippetTemplate {
			// Routing belongs to the gateway, not to the app's own directory.
			return nil
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(path, dir+"/"), ".tmpl")
		mode := uint32(0o644)
		if strings.HasSuffix(rel, secretSuffix) {
			rel = strings.TrimSuffix(rel, secretSuffix)
			mode = 0o600
		}

		content, err := p.renderFile(path, values)
		if err != nil {
			return err
		}
		files = append(files, File{Path: base + "srv/" + app.Name + "/" + rel, Content: content, Mode: mode})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// renderFile renders one template from a kind's set. Each is parsed on its own
// rather than into the shared set, so that two kinds may name a file the same
// thing, which they routinely do.
func (p *planner) renderFile(path string, values appValues) (string, error) {
	tmpl, err := template.New(filepath.Base(path)).Funcs(templateFuncs).ParseFS(templateFS, path)
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, values); err != nil {
		return "", fmt.Errorf("rendering %s: %w", path, err)
	}
	return buf.String(), nil
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

	return p.renderTemplate("wg0.conf.tmpl", map[string]any{
		"Site":       site,
		"PrivateKey": private,
		"MeshPrefix": p.cfg.Mesh.Prefix(),
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

// sortedKeys returns a map's keys in sorted order, for a message that lists
// them.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// routes is the gateway's inventory: one host block per app, each importing
// that app's own snippet.
func (p *planner) routes() []route {
	var out []route
	for _, name := range p.cfg.AppNames() {
		out = append(out, route{
			App:       name,
			Hostname:  p.cfg.Apps[name].Hostname,
			Upstreams: p.upstreams(name),
			Snippet:   snippetMount + name + ".caddy",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}

// upstreams is where an app's traffic goes: its own location when pinned, and
// every site holding the apps role when clustered.
func (p *planner) upstreams(name string) []string {
	app, ok := p.cfg.Apps[name]
	if !ok {
		return nil
	}
	port := appPort[app.Kind]
	if app.Placement.Mode == config.PlacementPinned {
		site, ok := p.sites[app.Placement.Site]
		if !ok {
			return nil
		}
		return []string{fmt.Sprintf("%s:%d", site.Address, port)}
	}
	var out []string
	for _, hostName := range p.cfg.AppsSites() {
		out = append(out, fmt.Sprintf("%s:%d", p.sites[hostName].Address, port))
	}
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
