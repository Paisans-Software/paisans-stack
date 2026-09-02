// Package render turns a validated configuration and its secrets into a tree
// of per site artifacts on local disk. It writes files and nothing else: no
// SSH, no Docker, no network. Pushing is a separate concern and a later slice.
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/josephquigley/paisans-stack/internal/config"
)

// File is one rendered artifact. Content is complete: rendering never appends
// to a file that already exists.
type File struct {
	// Path is relative to the output directory, always slash separated.
	Path    string
	Content string
	Mode    uint32
}

// Plan is every file a configuration renders to, sorted by path so that two
// runs over the same input produce the same tree in the same order.
type Plan struct {
	Files []File
}

// appPort is the port an application listens on inside its container. The
// gateway needs it to route, and it is a property of the software rather than
// of a deployment, so it is not configurable.
var appPort = map[config.Kind]int{
	config.KindMbin:        8080,
	config.KindOutline:     3000,
	config.KindPocketID:    1411,
	config.KindSynapse:     8008,
	config.KindWriteFreely: 8080,
}

// patroniAPIPort is where Patroni answers the health check HAProxy uses to
// find the primary. It is fixed by Spilo.
const patroniAPIPort = 8008

// postgresPort is the cluster's real Postgres port. Applications never use it:
// they connect to the local HAProxy instead, from the first install, so that
// adding a site changes a backend list rather than application configuration.
const postgresPort = 5432

type plannedApp struct {
	Name      string
	Kind      config.Kind
	Hostname  string
	Pinned    bool
	Site      string
	Port      int
	Env       []envVar
	OwnsDB    bool
	DBName    string
	DBUser    string
	MediaDirs []string
}

type envVar struct {
	Key   string
	Value string
}

type siteView struct {
	Name       string
	Address    string
	Endpoint   string
	Roles      []config.Role
	IsData     bool
	IsApps     bool
	IsGateway  bool
	IsWitness  bool
	IsEtcd     bool
	IsGarage   bool
	Apps       []plannedApp
	NeedsProxy bool
}

type planner struct {
	cfg     *config.Config
	secrets *config.Secrets
	mesh    string
	sites   map[string]*siteView
	order   []string
}

// Build produces the plan for a configuration. It assumes validation has
// already refused anything incoherent: a planner that re-checks policy is a
// second place for the rules to drift.
func Build(cfg *config.Config, secrets *config.Secrets) (*Plan, error) {
	addresses := make([]string, 0, len(cfg.Sites))
	for _, name := range cfg.SiteNames() {
		addresses = append(addresses, cfg.Sites[name].Address)
	}
	mesh, err := meshSubnet(addresses)
	if err != nil {
		return nil, err
	}

	p := &planner{cfg: cfg, secrets: secrets, mesh: mesh, sites: map[string]*siteView{}}
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
	return &Plan{Files: files}, nil
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
		Kind:     app.Kind,
		Hostname: app.Hostname,
		Pinned:   pinned,
		Site:     site.Name,
		Port:     appPort[app.Kind],
		OwnsDB:   pinned,
		DBName:   dbIdentifier(name),
		DBUser:   dbIdentifier(name),
	}
	env, err := p.appEnv(planned, app)
	if err != nil {
		return plannedApp{}, err
	}
	planned.Env = env
	return planned, nil
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
