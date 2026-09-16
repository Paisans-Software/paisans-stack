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

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
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
	"quote":  quote,
	"indent": indent,
	"yesno": func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	},
}

// indent prefixes every line of a value with n spaces, so that a multi line
// secret can be placed into a YAML block scalar. A PEM encoded key is the case
// that needs it: unindented, its second line ends the block and the rest of the
// file becomes a parse error.
//
// A trailing newline is dropped, because the template supplies the line break
// after the value and two would render a blank line inside the block.
func indent(n int, value string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n")
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

// caddyImage is the image the gateway runs.
//
// A DNS provider in Caddy is a module compiled into the binary, not a setting,
// so the provider decides the image. A declared image wins, because it is the
// only way a provider the toolkit publishes nothing for can reach a gateway now
// that nothing is built on a host.
func (p *planner) caddyImage() (string, error) {
	if declared := p.cfg.ACME.Image; declared != "" {
		return declared, nil
	}
	image, ok := acme.Image(p.cfg.ACME.Provider)
	if !ok {
		// Kept rather than deleted, and unreachable through the CLI: `paisans`
		// runs validate first, and acme-provider-needs-an-image refuses this
		// configuration there with a message written for an operator. This is
		// the library level guard for a caller that reaches render.Build
		// without validating, which is a supported way to use this package.
		// Without it the template would write `image: ` and the operator would
		// meet the problem as a compose file the host rejects.
		return "", fmt.Errorf(
			"acme.provider %q has no image in this toolkit and acme.image declares none. A DNS provider is a module compiled into Caddy, so an image carrying it is the only way it reaches a gateway. Published providers: %s",
			p.cfg.ACME.Provider, strings.Join(acme.Providers(), ", "))
	}
	return image, nil
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
	App string
	// Role is here so a later task can gate the host block on it: whether an
	// import belongs in this block is a fact about which hostname it is, not
	// about the app in general.
	Role      string
	Hostname  string
	Upstreams []string
	// Snippet is where this hostname's own routing lives on the gateway, as
	// the container sees it. The host block imports it rather than
	// containing it.
	Snippet string
	// Gate is the app's declared gate, carried onto every one of its
	// hostnames. A gate is access policy for the app, not for one hostname of
	// it, so every route an app has gets the same value.
	Gate string
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
	// Resolved only for a gateway site: a site holding no gateway role needs no
	// Caddy image, and acme.provider is not even required in a deployment with
	// no gateway anywhere, so demanding one here would refuse a legitimate
	// configuration over an image nothing will use. The template gates the
	// caddy service on Site.IsGateway, so an empty string here never reaches
	// a compose file.
	var caddy string
	if site.IsGateway {
		caddy, err = p.caddyImage()
		if err != nil {
			return nil, err
		}
	}
	infra, err := p.renderTemplate("infra-compose.yaml.tmpl", map[string]any{
		"Site":               site,
		"Scope":              p.scope(),
		"EtcdInitialCluster": p.etcdInitialCluster(),
		"HeartbeatMS":        p.heartbeatMS(),
		"ElectionTimeoutMS":  p.electionTimeoutMS(),
		"PostgresVersion":    p.postgresVersion(),
		"SpiloTag":           spilo,
		"CaddyImage":         caddy,
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
			"GateSnippets":   p.gateSnippetMounts(),
			"TrustedProxies": p.mesh,
			"ACMEDirective":  acme.Directive(p.cfg.ACME.Provider),
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

		gateSnippets, err := p.renderGateSnippets(base)
		if err != nil {
			return nil, err
		}
		files = append(files, gateSnippets...)

		env, err := p.renderTemplate("caddy.env.tmpl", map[string]any{
			"ACMEDNSToken": p.secrets.External["acme_dns_token"],
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
// routes to it on its primary hostname. It is rendered onto the gateway
// rather than beside the app, because that is where it is read.
//
// snippetPrefix matches it and every extra hostname's own snippet, such as
// caddy.snippet.wellknown.tmpl, all of which belong to the gateway for the
// same reason.
const (
	snippetTemplate = "caddy.snippet.tmpl"
	snippetPrefix   = "caddy.snippet."
)

// snippetDir is where snippets land on the gateway, and snippetMount is the
// same directory as the Caddy container sees it. The Caddyfile imports by the
// second, because Caddy reads it from inside the container.
const (
	snippetDir   = "srv/infra/caddy/snippets/"
	snippetMount = "/etc/caddy/snippets/"
)

// renderSnippets renders every hostname's routing onto a gateway.
//
// It walks the whole configuration rather than the gateway's own apps: a
// gateway routes to applications that run elsewhere, which is the usual case.
// One route renders one snippet, from the template its role selects, because
// two hostnames on the same app can serve entirely different things.
func (p *planner) renderSnippets(base string) ([]File, error) {
	var files []File
	for _, r := range p.routes() {
		app := p.cfg.Apps[r.App]
		planned, err := p.plannedFor(r.App, app)
		if err != nil {
			return nil, err
		}
		values, err := p.values(planned, app)
		if err != nil {
			return nil, err
		}
		// The snippet names the hostname it serves, not the app's primary: the
		// apex snippet for a homeserver has to say the apex, never the API
		// name, or its own comments would lie about which name reaches it.
		values.Hostname = r.Hostname
		values.PublicURL = "https://" + r.Hostname

		path := "templates/" + string(app.Kind) + "/" + kinds.SnippetFor(r.Role)
		content, err := p.renderFile(path, values)
		if err != nil {
			return nil, err
		}
		name := r.App
		if r.Role != kinds.PrimaryRole {
			name = r.App + "-" + r.Role
		}
		files = append(files, File{Path: base + snippetDir + name + ".caddy", Content: content, Mode: 0o644})
	}
	return files, nil
}

// gateSnippetTemplate defines the oauth2-proxy kind's named Caddy snippets,
// gate_provisional and gate_members. It is not a hostname's routing: nothing
// imports it by path, and no host block is rendered for it. A later kind's
// own snippet imports the names it defines, the same way the deployment this
// toolkit generalises imports its single `(pocketid_gate)` snippet by name.
const gateSnippetTemplate = "caddy.snippet.gates.tmpl"

// renderGateSnippets renders every oauth2-proxy app's named gate snippets onto
// the gateway. It walks every app in the configuration, not just this site's
// own, for the same reason renderSnippets does: the gateway routes to
// applications wherever they run.
func (p *planner) renderGateSnippets(base string) ([]File, error) {
	var files []File
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if app.Kind != config.KindOAuth2Proxy {
			continue
		}
		planned, err := p.plannedFor(name, app)
		if err != nil {
			return nil, err
		}
		values, err := p.values(planned, app)
		if err != nil {
			return nil, err
		}
		path := "templates/" + string(app.Kind) + "/" + gateSnippetTemplate
		content, err := p.renderFile(path, values)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: base + snippetDir + name + "-gates.caddy", Content: content, Mode: 0o644})
	}
	return files, nil
}

// gateSnippetMounts is where each oauth2-proxy app's gate snippet file lands,
// as the Caddy container sees it. The Caddyfile imports each at the top
// level, before any site block, because a named Caddy snippet has to be
// defined before something else can import it by name.
func (p *planner) gateSnippetMounts() []string {
	var out []string
	for _, name := range p.cfg.AppNames() {
		if p.cfg.Apps[name].Kind == config.KindOAuth2Proxy {
			out = append(out, snippetMount+name+"-gates.caddy")
		}
	}
	return out
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
		if strings.HasPrefix(strings.TrimPrefix(path, dir+"/"), snippetPrefix) {
			// Routing belongs to the gateway, not to the app's own directory,
			// whichever hostname it is for.
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

// routes is the gateway's inventory: one host block per hostname, each
// importing the snippet for that hostname's role.
func (p *planner) routes() []route {
	var out []route
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		gate := app.Gate
		if gate == "none" {
			// Carried to the template as empty rather than as the literal
			// "none", because the template's gate import is guarded on
			// truthiness: `none` is a value an operator writes, but it means
			// the same as never having written the key at all.
			gate = ""
		}
		out = append(out, route{
			App:       name,
			Role:      kinds.PrimaryRole,
			Hostname:  app.Hostname,
			Upstreams: p.upstreams(name),
			Snippet:   snippetMount + name + ".caddy",
			Gate:      gate,
		})
		for _, role := range sortedKeys(app.Hostnames) {
			out = append(out, route{
				App:       name,
				Role:      role,
				Hostname:  app.Hostnames[role],
				Upstreams: p.upstreams(name),
				Snippet:   snippetMount + name + "-" + role + ".caddy",
				Gate:      gate,
			})
		}
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
	return p.upstreamsOnPort(name, appPort[app.Kind])
}

// upstreamsOnPort is upstreams with the port supplied rather than looked up,
// for the one case where an app answers on a second, explicitly declared
// port: the oauth2-proxy kind's members instance. It exists so that finding a
// members upstream never computes a port from another kind's port, only from
// the literal gateMembersPort.
func (p *planner) upstreamsOnPort(name string, port int) []string {
	app, ok := p.cfg.Apps[name]
	if !ok {
		return nil
	}
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
