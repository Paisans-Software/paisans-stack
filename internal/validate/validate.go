// Package validate applies the design's policy to a loaded configuration.
//
// The distinction between refusing and warning is load bearing and is not an
// implementation detail. A refusal means the configuration is incoherent: it
// describes something that cannot work, and rendering it would produce
// artifacts that damage a deployment. A warning means the configuration is
// legitimate but risky, and the risk is acceptable when it is chosen rather
// than stumbled into.
//
// Every rule here traces to a rule in README.md. Nothing is invented.
package validate

import (
	"fmt"
	"net"
	"sort"

	"github.com/josephquigley/paisans-stack/internal/config"
)

// Level says how much a finding costs.
type Level int

const (
	// Warn is legitimate but risky. Rendering proceeds.
	Warn Level = iota
	// Refuse is incoherent. Rendering must fail.
	Refuse
)

func (l Level) String() string {
	if l == Refuse {
		return "REFUSE"
	}
	return "WARN"
}

// Finding is one problem, named so it can be looked up in README.md.
type Finding struct {
	Level Level
	// Rule is a stable identifier, quoted in tests and in output.
	Rule string
	// Key is the configuration key the finding is about.
	Key string
	// Message says what is wrong and what to do instead.
	Message string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s  %s\n  %s: %s", f.Level, f.Rule, f.Key, f.Message)
}

// Result is every finding for one configuration, refusals first and each group
// sorted, so that two runs over the same file print the same thing.
type Result struct {
	Findings []Finding
}

// Refused reports whether anything in the result blocks rendering.
func (r Result) Refused() bool {
	for _, f := range r.Findings {
		if f.Level == Refuse {
			return true
		}
	}
	return false
}

// Refusals returns only the findings that block rendering.
func (r Result) Refusals() []Finding { return r.byLevel(Refuse) }

// Warnings returns only the findings that do not block rendering.
func (r Result) Warnings() []Finding { return r.byLevel(Warn) }

func (r Result) byLevel(l Level) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Level == l {
			out = append(out, f)
		}
	}
	return out
}

// Has reports whether a rule fired. Tests use it.
func (r Result) Has(rule string) bool {
	for _, f := range r.Findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

type checker struct {
	cfg      *config.Config
	findings []Finding
}

func (c *checker) refuse(rule, key, format string, args ...any) {
	c.findings = append(c.findings, Finding{Refuse, rule, key, fmt.Sprintf(format, args...)})
}

func (c *checker) warn(rule, key, format string, args ...any) {
	c.findings = append(c.findings, Finding{Warn, rule, key, fmt.Sprintf(format, args...)})
}

// Check applies every rule and returns all of them. It never stops at the
// first problem: fixing a configuration one error per run is miserable, and an
// operator wants the whole list.
func Check(cfg *config.Config) Result {
	c := &checker{cfg: cfg}

	c.witnessSharesFailureDomain()
	c.twoVoters()
	c.undeclaredSites()
	c.placementShape()
	c.outlineBucketName()
	c.clusterSiteWithoutData()
	c.clusterAppWithoutAppsSite()
	c.garageReplicationExceedsSites()
	c.siteOutsideMesh()

	c.evenVoters()
	c.meshIsNotPrivate()
	c.pinnedOntoWitness()
	c.gatewayOnDataSite()
	c.pocketIDFileBackend()

	sort.SliceStable(c.findings, func(i, j int) bool {
		if c.findings[i].Level != c.findings[j].Level {
			return c.findings[i].Level > c.findings[j].Level
		}
		if c.findings[i].Rule != c.findings[j].Rule {
			return c.findings[i].Rule < c.findings[j].Rule
		}
		return c.findings[i].Key < c.findings[j].Key
	})
	return Result{Findings: c.findings}
}

// witnessSharesFailureDomain refuses a witness beside a voter.
//
// One site with one etcd member has quorum 1: it works, and the only thing
// that stops it is the machine itself, which would stop Postgres anyway. Add a
// witness on that same machine and quorum becomes 2 of 2, so the witness
// container crashing takes the database down while Postgres is healthy. A
// tiebreaker between two things that are always gone at once breaks no ties.
func (c *checker) witnessSharesFailureDomain() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if site.Has(config.RoleWitness) && site.Has(config.RoleData) {
			c.refuse("witness-shares-failure-domain", fmt.Sprintf("sites.%s.roles", name),
				"the witness role is on a site that also holds data. Quorum becomes 2 of 2, so losing the witness takes down a healthy database, and losing the machine loses both members at once. Move the witness to a location that fails independently, or drop it: one site correctly runs exactly one etcd member.",
			)
		}
	}
}

// twoVoters refuses a two member etcd cluster.
//
// Quorum is a majority: 1 member survives nothing but works, 3 survive one
// loss, and 2 survive nothing while requiring both. Two voters are strictly
// worse than one, so a join goes from one to three in a single operation and
// never rests at two.
func (c *checker) twoVoters() {
	if len(c.cfg.Etcd.Members) == 2 {
		c.refuse("two-etcd-voters", "etcd.members",
			"exactly two etcd voters (%s and %s). Two voters are strictly worse than one: a majority of two is two, so either failing stops the cluster. Declare one member, or three.",
			c.cfg.Etcd.Members[0], c.cfg.Etcd.Members[1])
	}
}

// clusterSiteWithoutData refuses a cluster member that does not hold the data
// role.
//
// The roles say what a site provides and the cluster list says which sites
// hold the database. A site named in the cluster without the data role
// declares both that it stores data and that it does not, and the two
// renderings disagree: it would receive HAProxy backends pointing at a Patroni
// that was never rendered for it. There is no reading of this that works, so
// it is a refusal rather than a warning.
func (c *checker) clusterSiteWithoutData() {
	for i, name := range c.cfg.Cluster.Sites {
		site, ok := c.cfg.Sites[name]
		if !ok {
			continue // undeclaredSites reports this
		}
		if site.Has(config.RoleData) {
			continue
		}
		c.refuse("cluster-site-without-data-role", fmt.Sprintf("cluster.sites[%d]", i),
			"names site %q, which does not hold the data role. A site in the cluster runs Patroni and stores the database. Add the data role to %s, or remove it from the cluster.", name, name)
	}
}

// clusterAppWithoutAppsSite refuses cluster placement when nowhere can run it.
//
// Cluster placement means the app runs on the sites holding the apps role and
// connects to the local proxy. With no such site the placement names no
// location at all, so the app would be declared and never rendered anywhere,
// which is the quietest possible failure.
func (c *checker) clusterAppWithoutAppsSite() {
	if len(c.cfg.AppsSites()) > 0 {
		return
	}
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Placement.Mode != config.PlacementCluster {
			continue
		}
		c.refuse("cluster-app-without-apps-site", fmt.Sprintf("apps.%s.placement", name),
			"is `cluster`, but no site holds the apps role, so there is nowhere to run it. Give a site the apps role, or pin the app to a site.")
	}
}

// garageReplicationExceedsSites refuses a replication factor larger than the
// number of Garage nodes.
//
// Garage will not store an object it cannot place on the requested number of
// distinct nodes. The deployment comes up, the buckets exist, and every upload
// fails, which looks like an application bug rather than a configuration one.
func (c *checker) garageReplicationExceedsSites() {
	garage := c.cfg.Storage.Garage
	if garage.Replication == 0 || len(garage.Sites) == 0 {
		return
	}
	if garage.Replication <= len(garage.Sites) {
		return
	}
	c.refuse("garage-replication-exceeds-sites", "storage.garage.replication",
		"is %d, but only %d site(s) run Garage. Garage cannot place a copy on a node that does not exist, so every upload fails while everything else looks healthy. Lower the factor, or add a Garage site.",
		garage.Replication, len(garage.Sites))
}

// evenVoters warns about an even number of etcd members above two.
//
// Quorum is a majority, so four members need three and tolerate one loss:
// exactly what three members tolerate, with an extra machine that can fail and
// an extra vote to collect on every write. An even count buys nothing and adds
// a failure domain. Two is refused separately, because two is worse than one
// rather than merely pointless.
func (c *checker) evenVoters() {
	count := len(c.cfg.Etcd.Members)
	if count <= 2 || count%2 != 0 {
		return
	}
	c.warn("even-etcd-voters", "etcd.members",
		"declares %d voters. A majority of %d is %d, which is the same number of losses %d members tolerate, so the extra member adds a machine that can fail and a vote to collect without improving anything. Prefer %d.",
		count, count, count/2+1, count-1, count-1)
}

// siteOutsideMesh refuses a site address that is not in the declared mesh.
//
// Applications trust the mesh subnet for forwarded client addresses, and every
// service binds to a mesh address. A site outside it is unreachable over the
// tunnel and its traffic would arrive from an untrusted source, so nothing
// about the deployment works as described.
func (c *checker) siteOutsideMesh() {
	if c.cfg.Mesh.Subnet == "" {
		return // the loader has already reported this
	}
	for _, name := range c.cfg.SiteNames() {
		address := c.cfg.Sites[name].Address
		if address == "" || c.cfg.Mesh.Contains(address) {
			continue
		}
		c.refuse("site-address-outside-mesh", fmt.Sprintf("sites.%s.address", name),
			"is %s, which is outside mesh.subnet %s. Every site binds its services to a mesh address, and applications trust that subnet for forwarded client addresses. Give the site an address inside it, or widen the mesh.",
			address, c.cfg.Mesh.Subnet)
	}
}

// meshIsNotPrivate warns about a mesh on publicly routable space.
//
// Preflight checks that the subnet does not collide with anything a host
// already routes, and that check needs a host. This is the part that can be
// checked from a file: a mesh on public address space collides with the real
// internet everywhere at once, and the symptom is a host that can no longer
// reach whoever actually owns those addresses.
func (c *checker) meshIsNotPrivate() {
	if c.cfg.Mesh.Subnet == "" {
		return
	}
	_, network, err := net.ParseCIDR(c.cfg.Mesh.Subnet)
	if err != nil {
		return // the loader has already reported this
	}
	if network.IP.IsPrivate() || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
		return
	}
	c.warn("mesh-subnet-is-not-private", "mesh.subnet",
		"is %s, which is publicly routable address space. Every host would route those addresses into the tunnel instead of to whoever owns them. Use RFC 1918 space, for example 10.44.0.0/24, unless this range is genuinely yours.",
		c.cfg.Mesh.Subnet)
}

// undeclaredSites refuses any reference to a site that does not exist. A
// typo here renders artifacts for a machine nobody owns.
func (c *checker) undeclaredSites() {
	check := func(key string, names []string) {
		for i, name := range names {
			if _, ok := c.cfg.Sites[name]; !ok {
				c.refuse("undeclared-site", fmt.Sprintf("%s[%d]", key, i),
					"names site %q, which is not declared under sites. Add the site, or correct the name.", name)
			}
		}
	}
	check("cluster.sites", c.cfg.Cluster.Sites)
	check("etcd.members", c.cfg.Etcd.Members)
	check("storage.garage.sites", c.cfg.Storage.Garage.Sites)

	for _, appName := range c.cfg.AppNames() {
		app := c.cfg.Apps[appName]
		if app.Placement.Mode != config.PlacementPinned {
			continue
		}
		if _, ok := c.cfg.Sites[app.Placement.Site]; !ok {
			c.refuse("undeclared-site", fmt.Sprintf("apps.%s.placement", appName),
				"pins the app to site %q, which is not declared under sites. Add the site, or correct the name.", app.Placement.Site)
		}
	}
}

// placementShape refuses a placement that is neither of the two the design
// allows. There are exactly two, and a third spelling is a silent
// misconfiguration rather than a new mode.
func (c *checker) placementShape() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Placement.Mode != config.PlacementInvalid {
			continue
		}
		c.refuse("invalid-placement", fmt.Sprintf("apps.%s.placement", name),
			"is %q. Placement is either `cluster` or `{ pinned: <site> }`, and nothing else.", app.Placement.Literal)
	}
}

// outlineBucketName refuses the one bucket name upstream cannot handle. It is
// a known Outline bug, and it fails after the deployment is up rather than at
// configuration time.
func (c *checker) outlineBucketName() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindOutline {
			continue
		}
		bucket, _ := app.Settings["s3_bucket"].(string)
		if bucket == "outline" {
			c.refuse("outline-bucket-named-outline", fmt.Sprintf("apps.%s.settings.s3_bucket", name),
				"is \"outline\". Upstream Outline cannot use a bucket of that name. Choose another, for example %s-uploads.", name)
		}
	}
}

// pinnedOntoWitness warns about disk contention with etcd.
//
// etcd's stability depends on fsync latency, and an application with its own
// database is not a quiet neighbour. Contention means missed heartbeats,
// spurious elections, and the database failing over because something else was
// compacting. This is risky rather than incoherent, so it warns.
func (c *checker) pinnedOntoWitness() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Placement.Mode != config.PlacementPinned {
			continue
		}
		site, ok := c.cfg.Sites[app.Placement.Site]
		if !ok || !site.Has(config.RoleWitness) {
			continue
		}
		c.warn("pinned-app-on-witness", fmt.Sprintf("apps.%s.placement", name),
			"pins the app to %s, which holds the witness role. etcd depends on fsync latency and an application with its own database is not a quiet neighbour: the failure mode is the database failing over for no reason. Prefer a separate site for pinned apps, or give etcd its own device there.",
			app.Placement.Site)
	}
}

// gatewayOnDataSite warns that failover will not reach the public path.
//
// With one site this is correct and total failure of that site is the expected
// behaviour of having one site. Once a second data site exists, the database
// fails over in about a minute while public DNS still points at the dead
// machine, and automatic failover behind a manual DNS change is not automatic.
func (c *checker) gatewayOnDataSite() {
	if len(c.cfg.DataSites()) < 2 {
		return
	}
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if site.Has(config.RoleGateway) && site.Has(config.RoleData) {
			c.warn("gateway-on-data-site", fmt.Sprintf("sites.%s.roles", name),
				"holds both gateway and data while %d data sites are declared. The database would fail over in about a minute, but public DNS would still point here, so nothing is reachable until a record changes and propagates. Move the gateway off both data sites.",
				len(c.cfg.DataSites()))
		}
	}
}

// pocketIDFileBackend warns about anything other than the database backend.
//
// Its uploads are avatars and admin branding, about a megabyte in total. In
// Postgres they ride streaming replication, so a promoted site has them
// already with no sync job. It also keeps object storage off the dependency
// list of the one service that gates every other one.
func (c *checker) pocketIDFileBackend() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindPocketID {
			continue
		}
		backend, _ := app.Settings["file_backend"].(string)
		if backend == "database" {
			continue
		}
		shown := backend
		if shown == "" {
			shown = "unset, which means filesystem"
		}
		c.warn("pocket-id-file-backend", fmt.Sprintf("apps.%s.settings.file_backend", name),
			"is %s. Prefer `database`: the uploads are about a megabyte of avatars and branding, in Postgres they ride streaming replication with no sync job, and sign in then does not depend on object storage being up. On filesystem, a promoted site serves broken avatars and stock branding mid incident.",
			shown)
	}
}
