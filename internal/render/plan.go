// Package render turns a validated configuration and its secrets into a tree
// of per site artifacts on local disk. It writes files and nothing else: no
// SSH, no Docker, no network. Pushing is a separate concern and a later slice.
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// File is one rendered artifact. Content is complete: rendering never appends
// to a file that already exists.
type File struct {
	// Path is relative to the output directory, always slash separated.
	Path    string
	Content string
	Mode    uint32
	// App is the app a file belongs to when it lies outside that app's own
	// stack directory, as a route snippet on a site running Caddy does, and
	// empty otherwise. apply holds such a file back with its app (see
	// apply.Except), which its path alone does not say: it sits in the
	// infrastructure stack's directory.
	App string
}

// Plan is every file a configuration renders to, sorted by path so that two
// runs over the same input produce the same tree in the same order.
type Plan struct {
	Files []File
	// Deployment is the identity every path and name in Files derives from.
	Deployment deployment.Deployment
}

// appPort is the port an application listens on inside its container. The
// gateway needs it to route, and it is a property of the software rather than
// of a deployment, so it is not configurable.
var appPort = map[config.Kind]int{
	config.KindElement:     80,
	config.KindMbin:        8080,
	config.KindOAuth2Proxy: 4180,
	config.KindOutline:     3000,
	config.KindPocketID:    1411,
	config.KindSynapse:     8008,
	// Not WriteFreely's default 8080: Mbin publishes 8080, and a pinned blog
	// on an apps site sits beside a clustered Mbin. WriteFreely reads its
	// port from the config.ini this toolkit renders, so the number is ours.
	config.KindWriteFreely: 8081,
	// Not the fork's default 3000: Outline publishes 3000, and two apps on
	// one site publish on the same mesh address.
	config.KindUptime: 3001,
}

// AppPort is the port a kind listens on, which is also the port it publishes
// on its site's mesh address. A command that calls an application's own API
// from its host needs it, and reading it here keeps one number in one place.
func AppPort(kind config.Kind) int { return appPort[kind] }

// gateMembersPort is the oauth2-proxy kind's second instance, one port above
// the primary. It is a literal here, not appPort[config.KindOAuth2Proxy]+1:
// the kind ships exactly two fixed ports, both declared explicitly in
// internal/kinds/kinds.go's service list and in the kind's own templates, so
// nothing computes a port at render time that those files did not already
// name.
const gateMembersPort = 4181

// masPort is where Matrix Authentication Service listens, one port above the
// homeserver's 8008. MAS's own documented example binds 8080, and that is not
// used here: 8080 is already the port two other kinds in this toolkit publish,
// and a stack whose two containers sit on adjacent ports is easier to read on
// a host than one that borrows a number from somewhere else. The port is not
// a default being relied on either way, because the listener in mas.yaml binds
// it explicitly.
const masPort = 8009

// patroniAPIPort is where Patroni answers the health check HAProxy uses to
// find the primary. It is fixed by Spilo.
const patroniAPIPort = 8008

// bgMonPort is where Spilo's bg_mon extension serves its monitoring page.
// bg_mon defaults to 8080, the most common application port there is, and on
// the first real host it took 8080 on a data site before Mbin could publish
// there. It is moved beside the Patroni API instead of moving every app.
const bgMonPort = 8009

// haproxyStatsPort is where HAProxy serves its statistics, on loopback only.
// Nothing else the toolkit renders listens on 8404.
const haproxyStatsPort = 8404

// HAProxyStatsPort is haproxyStatsPort, for the command that reads it.
const HAProxyStatsPort = haproxyStatsPort

// postgresPort is the cluster's real Postgres port. Applications never use it:
// they connect to the local HAProxy instead, from the first install, so that
// adding a site changes a backend list rather than application configuration.
const postgresPort = 5432

type plannedApp struct {
	Name string
	// Dir is the app's stack directory on a host, under the deployment's root.
	Dir       string
	Kind      config.Kind
	Hostname  string
	Pinned    bool
	Site      string
	Port      int
	OwnsDB    bool
	DBName    string
	DBUser    string
	MediaDirs []string
	// Images is the reference each of the kind's services runs, resolved from
	// the configuration where it declared one and from the kind's default
	// otherwise. The template reads it rather than deciding, so what runs is
	// decided in one place.
	Images map[string]string
}

type siteView struct {
	Name      string
	Address   string
	Endpoint  string
	Roles     []config.Role
	IsData    bool
	IsApps    bool
	IsGateway bool
	IsWitness bool
	IsEtcd    bool
	IsGarage  bool
	// RunsCaddy is set where the toolkit's Caddy runs: a gateway, and a
	// monitor in ingress mode paisans, which serves its own apps.
	RunsCaddy  bool
	Apps       []plannedApp
	NeedsProxy bool
	// WatchdogOff renders Patroni without fencing and drops the device
	// mapping, which compose would otherwise refuse to start without.
	WatchdogOff bool
}

type planner struct {
	cfg     *config.Config
	secrets *config.Secrets
	mesh    string
	sites   map[string]*siteView
	order   []string
	// etcdInitial is how each etcd member was first started, by site, as
	// read from its host. See EtcdInitialPath.
	etcdInitial map[string]EtcdInitial
}

// Build produces the plan for a configuration. It assumes validation has
// already refused anything incoherent: a planner that re-checks policy is a
// second place for the rules to drift.
//
// opts carry facts a caller read from the live deployment, such as the flags
// an etcd member was born with. Without them every site renders as it would
// on a fresh deployment.
func Build(cfg *config.Config, secrets *config.Secrets, opts ...Option) (*Plan, error) {
	p := &planner{cfg: cfg, secrets: secrets, mesh: cfg.Mesh.Subnet, sites: map[string]*siteView{}}
	for _, opt := range opts {
		opt(p)
	}
	for _, name := range cfg.SiteNames() {
		site := cfg.Sites[name]
		p.order = append(p.order, name)
		p.sites[name] = &siteView{
			Name:      name,
			Address:   site.Address,
			Endpoint:  site.Endpoint,
			Roles:     site.Roles,
			IsData:    site.Has(config.RoleData),
			IsApps:    site.Has(config.RoleApps),
			IsGateway: site.Has(config.RoleGateway),
			IsWitness: site.Has(config.RoleWitness),
			IsEtcd:    contains(cfg.Etcd.Members, name),
			IsGarage:  contains(cfg.Storage.Garage.Sites, name),
			RunsCaddy: site.RunsCaddy(),

			WatchdogOff: site.WatchdogMode() == config.WatchdogOff,
		}
	}

	if err := p.placeApps(); err != nil {
		return nil, err
	}

	var files []File
	for _, name := range p.order {
		siteFiles, err := p.renderSite(p.sites[name])
		if err != nil {
			return nil, err
		}
		files = append(files, siteFiles...)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return &Plan{Files: files, Deployment: cfg.Deployment()}, nil
}

// placeApps decides where each app runs.
//
// A pinned app runs at its named site and is self contained there: its own
// compose file, its own Postgres, its own volumes. An app with cluster
// placement runs on every site holding the apps role and connects to the local
// HAProxy.
func (p *planner) placeApps() error {
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		switch app.Placement.Mode {
		case config.PlacementPinned:
			site, ok := p.sites[app.Placement.Site]
			if !ok {
				return fmt.Errorf("apps.%s.placement: site %q is not declared", name, app.Placement.Site)
			}
			planned, err := p.planApp(name, app, site, true)
			if err != nil {
				return err
			}
			site.Apps = append(site.Apps, planned)
		case config.PlacementCluster:
			hosts := p.cfg.AppsSites()
			if len(hosts) == 0 {
				return fmt.Errorf("apps.%s.placement: is `cluster`, but no site holds the apps role. Give a site the apps role, or pin the app", name)
			}
			for _, hostName := range hosts {
				site := p.sites[hostName]
				planned, err := p.planApp(name, app, site, false)
				if err != nil {
					return err
				}
				site.Apps = append(site.Apps, planned)
				site.NeedsProxy = true
			}
		default:
			return fmt.Errorf("apps.%s.placement: %q is not a placement", name, app.Placement.Literal)
		}
	}
	for _, name := range p.order {
		site := p.sites[name]
		sort.Slice(site.Apps, func(i, j int) bool { return site.Apps[i].Name < site.Apps[j].Name })
	}
	return nil
}

func (p *planner) planApp(name string, app config.App, site *siteView, pinned bool) (plannedApp, error) {
	planned := plannedApp{
		Name:     name,
		Dir:      p.dep().Dir(name),
		Kind:     app.Kind,
		Hostname: app.Hostname,
		Pinned:   pinned,
		Site:     site.Name,
		Port:     appPort[app.Kind],
		OwnsDB:   pinned,
		DBName:   dbIdentifier(name),
		DBUser:   dbIdentifier(name),
	}
	planned.Images = p.appImages(app)
	return planned, nil
}

// appImages resolves what each of the kind's services runs.
//
// A declared reference wins, and an absent one takes the default the kind
// ships, so an adopter who never opens the stanza runs a tested set. The
// database is the one service with no fixed default: every database in the
// deployment is the same major version, declared once in cluster, so it is
// derived rather than repeated per app.
func (p *planner) appImages(app config.App) map[string]string {
	out := map[string]string{}
	for _, service := range kinds.Services(app.Kind) {
		switch {
		case app.Images[service.Name] != "":
			out[service.Name] = app.Images[service.Name]
		case service.Image != "":
			out[service.Name] = service.Image
		case service.Name == kinds.PostgresService:
			out[service.Name] = fmt.Sprintf("postgres:%s-alpine", p.postgresVersion())
		}
	}
	return out
}

// dbIdentifier keeps app names usable as Postgres identifiers without
// quoting.
func dbIdentifier(name string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(name)
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// DBIdentifier is the role and database name an app's rendered connection
// uses. apply creates them, and must name them exactly as the render did.
func DBIdentifier(name string) string { return dbIdentifier(name) }

// PatroniAPIPort is where the rendered Patroni serves its REST API, on the
// site's mesh address.
const PatroniAPIPort = patroniAPIPort

// PatroniScope is the Patroni cluster name every site renders as SCOPE.
// Patroni keeps its state in etcd under /service/<scope>/ (Patroni v4.1.0,
// patroni/dcs/__init__.py: the namespace defaults to "service"), and Spilo
// passes Patroni a namespace only when NAMESPACE is set to something other
// than "default" (zalando/spilo 4.1-p2, postgres-appliance/scripts/
// configure_spilo.py), which the rendered patroni.env never sets. A command
// that reads a key such as /sync names it with this.
const PatroniScope = "paisans"
