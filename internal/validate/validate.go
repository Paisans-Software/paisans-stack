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
	"slices"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/adminreconciler"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/render"
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
	c.dashboardLinkIsAPath()
	c.mbinQueueIsKnown()
	c.clusterSiteWithoutData()
	c.clusterAppWithoutAppsSite()
	c.garageReplicationExceedsSites()
	c.garageConsistency()
	c.storageRoleWithoutGarage()
	c.garageCapacities()
	c.garageAvailability()
	c.siteOutsideMesh()
	c.imageServices()
	c.floatingImages()
	c.clusterPlacementWithoutACluster()
	c.acmeProviderHasAnImage()
	c.acmeImageIsNotStockCaddy()
	c.floatingACMEImage()
	c.hostnameRoles()
	c.duplicateHostname()
	c.gateWithoutAGate()
	c.visibilityGateOnUngateableKind()
	c.visibilityGateWithoutSignedFetch()
	c.homeserverMustBePinned()
	c.uptimeNeedsAnAdminGroup()
	c.adminGroupNotAdmins()
	c.oidcMemberGroupNotAName()
	c.oidcMemberGroupDisagreesWithGate()
	c.oidcClientUnrestricted()
	c.monitorRoles()
	c.ingress()
	c.smtpOnAKindWithoutMail()
	c.mediaHostnameShape()
	c.mediaHostnameUnderAnAppHostname()
	c.outlineBucketInMediaURL()
	c.configKeyIsNestedInAnEnvFile()
	c.configKeyLooksLikeASecret()
	c.configKeySteersCompose()
	c.portCollision()

	c.evenVoters()
	c.meshIsNotPrivate()
	c.pinnedOntoWitness()
	c.gatewayOnDataSite()
	c.pocketIDFileBackend()
	c.uptimeWithoutSMTP()
	c.pocketIDStandbyMarkerUnknown()
	c.mbinRabbitMQAcrossSites()
	c.imageForAbsentPostgres()
	c.publicAddress()
	c.watchdogOffOnDataSite()
	c.visibilityGateProvisional()

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

// garageConsistency refuses a consistency mode Garage does not know, and
// warns on the two that do not mean what they appear to.
//
// Garage v1.0.1 accepts exactly consistent, degraded and dangerous
// (src/rpc/replication_mode.rs) and refuses to start on anything else, so a
// typo would take object storage down at the next apply. dangerous confirms
// an upload once one copy exists; degraded relaxes only the read quorum, which
// is already one at replication 2, so there it changes nothing
// (doc/book/reference-manual/configuration.md, "consistency_mode").
func (c *checker) garageConsistency() {
	garage := c.cfg.Storage.Garage
	switch garage.Consistency {
	case "", config.GarageConsistent:
	case config.GarageDangerous:
		if len(garage.Sites) > 1 {
			c.warn("garage-consistency-dangerous", "storage.garage.consistency",
				"is dangerous: Garage confirms an upload once one copy exists and sends the rest in the background, so an upload can live on one disk until the other sites catch up, and a read can miss a recent change. Uploads continue while a site is down; that is the trade.")
		}
	case config.GarageDegraded:
		if c.replication() <= 2 {
			c.warn("garage-consistency-degraded-is-consistent", "storage.garage.consistency",
				"is degraded, which at replication %d is identical to consistent: it lowers only the read quorum, which is already one. Use dangerous if uploads must continue while a site is down.", c.replication())
		}
	default:
		c.refuse("garage-consistency-unknown", "storage.garage.consistency",
			"is %q. Garage accepts consistent, degraded or dangerous, and refuses to start on anything else.", garage.Consistency)
	}
}

// storageRoleWithoutGarage refuses the storage role on a site that
// storage.garage.sites does not list. The role says the host exists to run
// Garage, and the render places Garage only from that list, so the site would
// come up with nothing on it while its entry promised object storage.
func (c *checker) storageRoleWithoutGarage() {
	for _, name := range c.cfg.SiteNames() {
		if !c.cfg.Sites[name].Has(config.RoleStorage) || slices.Contains(c.cfg.Storage.Garage.Sites, name) {
			continue
		}
		c.refuse("storage-role-without-garage", fmt.Sprintf("sites.%s.roles", name),
			"includes storage, but storage.garage.sites does not list %s, so it would run no Garage at all. Add it to storage.garage.sites, at the end if the cluster is already running, or drop the role.", name)
	}
}

// garageCapacities refuses a capacity Garage would refuse, and an override
// for a site that runs no Garage.
//
// Garage's `layout assign -c` takes a number with a unit and refuses a bare
// number or zero, so a bad value would surface as a failed layout stage on a
// live cluster rather than here. A capacities key that is not a Garage site
// is a typo or a leftover, and silently ignoring it would leave the node it
// meant at the default.
func (c *checker) garageCapacities() {
	garage := c.cfg.Storage.Garage
	if len(garage.Sites) > 0 {
		if _, err := config.ParseSize(garage.Capacity); err != nil {
			c.refuse("garage-capacity-not-a-size", "storage.garage.capacity", "%v. Garage's layout needs a capacity with a unit.", err)
		}
	}
	keys := make([]string, 0, len(garage.Capacities))
	for site := range garage.Capacities {
		keys = append(keys, site)
	}
	sort.Strings(keys)
	for _, site := range keys {
		key := "storage.garage.capacities." + site
		if !slices.Contains(garage.Sites, site) {
			c.refuse("garage-capacity-for-no-garage-site", key,
				"names %s, which storage.garage.sites does not list, so no node would take it. Remove it, or list the site.", site)
			continue
		}
		if _, err := config.ParseSize(garage.Capacities[site]); err != nil {
			c.refuse("garage-capacity-not-a-size", key, "%v. Garage's layout needs a capacity with a unit.", err)
		}
	}
}

// garageAvailability warns on layouts whose behaviour with one site down
// surprises. Each is a legitimate choice, so none is refused.
//
// Quorums are Garage v1.0.1's (src/rpc/replication_mode.rs): at replication 2
// a write needs both copies unless the mode is dangerous; at replication 2 on
// three or more sites each partition sits on two of them, so the partitions a
// down site holds stop taking writes.
func (c *checker) garageAvailability() {
	garage := c.cfg.Storage.Garage
	n := len(garage.Sites)
	rf := c.replication()
	switch {
	case n > 1 && rf == 1:
		c.warn("garage-single-copy", "storage.garage.replication",
			"is 1 across %d sites: each object lives on one site only, and is unreadable while that site is down. Raise it to 2 or more.", n)
	case rf == 2 && n == 2 && garage.Consistency != config.GarageDangerous:
		c.warn("garage-two-sites-stop-uploads", "storage.garage.replication",
			"is 2 on two sites at consistency %s: every upload needs both, so uploads stop while either site is down; reads continue. A third Garage site at replication 3 keeps both, or consistency dangerous keeps uploads at a durability cost.", c.consistency())
	case rf == 2 && n > 2 && garage.Consistency != config.GarageDangerous:
		c.warn("garage-partial-uploads", "storage.garage.replication",
			"is 2 on %d sites: each object lives on two of them, so while any one is down the uploads that would land on it fail, roughly %d in %d. Replication 3 keeps every upload working with one site down.", n, 2, n)
	}
}

func (c *checker) replication() int {
	if c.cfg.Storage.Garage.Replication == 0 {
		return 1
	}
	return c.cfg.Storage.Garage.Replication
}

func (c *checker) consistency() string {
	if c.cfg.Storage.Garage.Consistency == "" {
		return config.GarageConsistent
	}
	return c.cfg.Storage.Garage.Consistency
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

// dashboardLinkIsAPath refuses a sso_dashboard_link that is not a path on
// the app's own hostname. `oidc client create` sends it to the identity
// provider as part of a launch URL, so a malformed one is caught here, before
// any client is planned, rather than at the provider.
func (c *checker) dashboardLinkIsAPath() {
	for _, name := range c.cfg.AppNames() {
		v, ok := c.cfg.Apps[name].Settings[kinds.DashboardLinkSetting]
		if !ok {
			continue
		}
		if err := kinds.CheckDashboardLink(v); err != nil {
			c.refuse("sso-dashboard-link-not-a-path", fmt.Sprintf("apps.%s.settings.%s", name, kinds.DashboardLinkSetting), "%s", err.Error())
		}
	}
}

// mbinQueueIsKnown refuses an Mbin queue setting that names no backend the
// toolkit renders. Rendering would otherwise have to pick one silently.
func (c *checker) mbinQueueIsKnown() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindMbin {
			continue
		}
		switch q := kinds.MbinQueue(app.Settings); q {
		case kinds.MbinQueuePostgres, kinds.MbinQueueRabbitMQ:
		default:
			c.refuse("mbin-queue-unknown", fmt.Sprintf("apps.%s.settings.%s", name, kinds.MbinQueueSetting),
				"is %q. Choose %s (the default: the queues live in the replicated database) or %s (a broker per site, faster, and lost with its site).",
				q, kinds.MbinQueuePostgres, kinds.MbinQueueRabbitMQ)
		}
	}
}

// mbinRabbitMQAcrossSites warns when an Mbin app on more than one apps site
// keeps its queues in RabbitMQ. Each site then has its own broker, and an
// activity a site has acknowledged, or a delivery it has queued, is lost with
// that site; the remote server that got the acknowledgement never sends it
// again. A warning rather than a refusal: it is a risk an operator may take
// for throughput, not a configuration that cannot work.
func (c *checker) mbinRabbitMQAcrossSites() {
	sites := len(c.cfg.AppsSites())
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindMbin || app.Placement.Mode == config.PlacementPinned || sites < 2 {
			continue
		}
		if kinds.MbinQueue(app.Settings) != kinds.MbinQueueRabbitMQ {
			continue
		}
		c.warn("mbin-rabbitmq-across-sites", fmt.Sprintf("apps.%s.settings.%s", name, kinds.MbinQueueSetting),
			"is %s on %d apps sites. Each site runs its own broker, so work a site has accepted (an inbox delivery already answered, a delivery queued for another server) is lost or stranded with that site. %s keeps the queues in the replicated database, where the other site's consumers take them over.",
			kinds.MbinQueueRabbitMQ, sites, kinds.MbinQueuePostgres)
	}
}

// clusterPlacementWithoutACluster refuses cluster placement for an application
// that cannot join the cluster.
//
// Cluster placement means the app runs on every site holding the apps role and
// shares one database through the local proxy. An application that stores its
// data anywhere else gets neither half of that: it would be rendered onto each
// apps site with its own file, so one hostname would serve two deployments
// that diverge from the moment anybody writes to them, and a failover would
// move readers between them.
//
// Two kinds are in this position: Element, which is a static client, and
// oauth2-proxy, which keeps a session in a cookie. It is refused rather than
// warned about because there is no reading of it that works, and pinning is
// not a downgrade: it is the plain case, and the app's availability becomes
// its site's, which is what it was always going to be.
func (c *checker) clusterPlacementWithoutACluster() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Placement.Mode != config.PlacementCluster || kinds.UsesPostgres(app.Kind) {
			continue
		}
		c.refuse("cluster-placement-without-a-cluster", fmt.Sprintf("apps.%s.placement", name),
			"is `cluster`, but %s keeps its data outside the Postgres cluster, so there is nothing for it to join. It would be rendered onto every apps site with its own separate storage, which is two deployments behind one hostname. Pin it to a site instead.",
			app.Kind)
	}
}

// acmeProviderHasAnImage refuses a provider whose module cannot reach the
// gateway, or a monitor's own Caddy, which runs the same image.
//
// A DNS provider in Caddy is a Go module compiled into the binary, and nothing
// is built on a host, so a provider the toolkit publishes no image for needs an
// image declared that carries it. Without one the gateway would run a binary
// that cannot load its own configuration, and would hold no certificate for
// any hostname. That is incoherent rather than risky, so it is a refusal.
func (c *checker) acmeProviderHasAnImage() {
	if len(c.cfg.CaddySites()) == 0 || c.cfg.ACME.Provider == "" {
		return // no Caddy needs certificates; a missing provider is structural
	}
	if _, ok := acme.Image(c.cfg.ACME.Provider); ok {
		return
	}
	if c.cfg.ACME.Image != "" {
		return
	}
	c.refuse("acme-provider-needs-an-image", "acme.provider",
		"is %q, which this toolkit publishes no image for, and acme.image declares none. A DNS provider is a module compiled into Caddy rather than a setting, and nothing is built on a host, so the gateway would run a binary that cannot load its own configuration. Published providers are %s; for any other, declare acme.image with the module compiled in.",
		c.cfg.ACME.Provider, strings.Join(acme.Providers(), ", "))
}

// acmeImageIsNotStockCaddy refuses upstream's own image as an override.
//
// This is the one thing about an image that can be judged from its reference.
// Upstream's Caddy carries no DNS provider module, so it certainly cannot serve
// a configuration that names one. Every other reference is accepted here and
// verified at apply time by asking the binary, because what is compiled into a
// binary is not a property of its name.
func (c *checker) acmeImageIsNotStockCaddy() {
	if c.cfg.ACME.Image == "" || !acme.IsStockCaddy(c.cfg.ACME.Image) {
		return
	}
	c.refuse("acme-image-is-stock-caddy", "acme.image",
		"is %q, which is upstream's own Caddy image and carries no DNS provider module. Caddy would fail to load a configuration using `acme_dns %s`. Remove this key to take the image this toolkit publishes, or declare one with the module compiled in.",
		c.cfg.ACME.Image, c.cfg.ACME.Provider)
}

// imageServices refuses an image declared for a service the kind does not
// have.
//
// The map is keyed on compose service names precisely so this check can exist.
// An unknown key is a typo, and a typo that renders nothing is the worst
// outcome available: the operator believes they moved to their fork, the
// deployment keeps running the default, and nothing anywhere says so.
func (c *checker) imageServices() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		for _, service := range sortedKeys(app.Images) {
			if kinds.Has(app.Kind, service) {
				continue
			}
			c.refuse("unknown-image-service", fmt.Sprintf("apps.%s.images.%s", name, service),
				"names a service %q, which the %s template does not define. Its services are %s. A key that matches nothing renders nothing, so the deployment would keep running the default image while the file says otherwise.",
				service, app.Kind, strings.Join(kinds.ServiceNames(app.Kind), ", "))
		}
	}
}

// floatingImages refuses a reference that does not name one build.
//
// `latest` makes two runs of `apply` produce different deployments from
// identical inputs, which contradicts the premise that the configuration plus
// the secrets reconstruct the stack. That is incoherent rather than risky, so
// it is a refusal. A digest is accepted and is the stronger form.
//
// What this does not do is judge whether a version is safe to move to. Many of
// these applications run schema migrations at boot against live member data,
// and the toolkit cannot know which release does. It must not imply it
// checked.
func (c *checker) floatingImages() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		for _, service := range sortedKeys(app.Images) {
			ref := kinds.ParseReference(app.Images[service])
			why := ref.Floating()
			if why == "" {
				continue
			}
			c.refuse("floating-image-tag", fmt.Sprintf("apps.%s.images.%s", name, service),
				"is %q and %s. Two runs of `apply` would then deploy different builds from the same inputs, so the configuration and the secrets no longer reconstruct the stack. Name a version tag, or a digest, which is stronger.",
				app.Images[service], why)
		}
	}
}

// imageForAbsentPostgres warns about an image for a database that is not
// rendered.
//
// A clustered app has no Postgres service: it connects to the local HAProxy,
// and the cluster's version is set once for every database rather than per
// app. The declaration is harmless, and placement can change, so this warns
// rather than refusing.
func (c *checker) imageForAbsentPostgres() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Placement.Mode != config.PlacementCluster {
			continue
		}
		if _, ok := app.Images[kinds.PostgresService]; !ok {
			continue
		}
		c.warn("image-for-absent-postgres", fmt.Sprintf("apps.%s.images.%s", name, kinds.PostgresService),
			"declares a database image, but the app has cluster placement and runs no database of its own: it connects to the local HAProxy, and the cluster's version comes from cluster.postgres_version. Nothing renders this. Remove it, or pin the app if it was meant to have its own database.")
	}
}

// sortedKeys returns a map's keys in sorted order, so that one file reports its
// problems in the same order every run.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// portCollision refuses two things on one site binding the same port.
//
// Infrastructure runs with host networking and every app publishes on its
// site's mesh address, so two of them on one port cannot both start, and the
// second fails only when its container does: on a real data and apps site,
// Spilo's bg_mon took 8080 before Mbin could publish there. The listeners are
// read from render, which takes them from the same conditions and constants
// the rendered files use, so the check cannot drift from what lands on a host.
// A clustered app is counted on every apps site and a pinned one on its own.
func (c *checker) portCollision() {
	for _, name := range c.cfg.SiteNames() {
		listeners := render.SiteListeners(c.cfg, name)
		reported := map[string]bool{}
		for i, a := range listeners {
			for _, b := range listeners[i+1:] {
				if !collides(a, b) {
					continue
				}
				// One finding per pair and port: a service on the mesh
				// address and loopback is one collision, not two.
				pair := fmt.Sprintf("%s|%s|%d/%s", a.Owner, b.Owner, a.Port, a.Proto)
				if reported[pair] {
					continue
				}
				reported[pair] = true
				c.refuse("port-collision", fmt.Sprintf("sites.%s", name),
					"%s binds %s and %s binds %s, so whichever starts second fails at container start. Pin one of them to another site, or move the role that brings the other.",
					a.Owner, a, b.Owner, b)
			}
		}
	}
}

// collides reports whether two listeners on one site cannot both bind. The
// same owner under the same key is one listener named twice, such as a
// service on its mesh address and on loopback, which never overlaps anyway;
// the same owner under two keys, such as an app published on its mesh
// address and again on an ingress listen, is two binds like any others.
func collides(a, b render.Listener) bool {
	if a.Owner == b.Owner && a.Key == b.Key {
		return false
	}
	return a.Overlaps(b)
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

// pocketIDStandbyMarkerUnknown warns when a Pocket ID on more than one apps
// site runs an image whose refusal text the toolkit has not recorded. Every
// site but one stands by on that text (kinds.PocketIDStandbyMarker); an
// unknown image gets the default image's, which may not match. A warning,
// not a refusal: a marker that does not match fails safe, the standby site's
// container restarting under Docker's policy rather than running twice, and
// apply's gate says so.
func (c *checker) pocketIDStandbyMarkerUnknown() {
	sites := len(c.cfg.AppsSites())
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindPocketID || app.Placement.Mode == config.PlacementPinned || sites < 2 {
			continue
		}
		image := app.Images["app"]
		if image == "" {
			continue
		}
		if _, known := kinds.StandbyMarker(image); known {
			continue
		}
		c.warn("pocket-id-standby-marker-unknown", fmt.Sprintf("apps.%s.images.app", name),
			"is %s, whose refusal text is not recorded, on %d apps sites. Every site but one stands by when Pocket ID logs that another instance holds the database, and this image is assumed to log what the default does. If it does not, the standby sites restart in a loop instead of standing by, and apply's health gate fails there. Check its backend/internal/bootstrap/bootstrap.go.",
			image, sites)
	}
}

// floatingACMEImage refuses a gateway image that does not name one build.
//
// The same rule as floating-image-tag, applied to the one image that was
// exempt from it because it lives in its own stanza rather than under an app.
// It matters more here, not less: a moving tag on an app image deploys a
// different build of that app, while a moving tag on the gateway can silently
// drop the DNS provider module, and a gateway that cannot answer a challenge
// holds no certificate for any hostname in the deployment.
func (c *checker) floatingACMEImage() {
	if c.cfg.ACME.Image == "" {
		return
	}
	why := kinds.ParseReference(c.cfg.ACME.Image).Floating()
	if why == "" {
		return
	}
	c.refuse("acme-image-is-floating", "acme.image",
		"is %q and %s. The gateway's image carries the DNS provider module compiled in, so a tag that moves can drop the module the configuration names, and the gateway would then hold no certificate for any hostname. Name a version tag, or a digest, which is stronger.",
		c.cfg.ACME.Image, why)
}

// gateWithoutAGate refuses an app declaring a gate when the deployment runs
// none.
//
// The import would name a snippet nothing renders, and Caddy fails to load its
// whole configuration over one missing import, so every hostname goes down
// rather than one.
func (c *checker) gateWithoutAGate() {
	gated := false
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Gate() != "" {
			gated = true
		}
	}
	if !gated {
		return
	}
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Kind == config.KindOAuth2Proxy {
			return
		}
	}
	c.refuse("gate-without-a-gate-app", "apps",
		"an app declares a gate, but no app of kind oauth2-proxy is declared, so nothing renders the snippet the gateway would import. Caddy fails to load its entire configuration over one missing import, so this would take every hostname down rather than one. Declare the gate app, or set visibility_gate: public.")
}

// visibilityGateOnUngateableKind refuses a gate on a kind that cannot sit
// behind one.
//
// README: "A Matrix hostname must never be gated." The gate answers a request
// with no session with a redirect to a passkey prompt. A Matrix client is not
// a browser and does not follow it, and neither does a federating server
// fetching the delegation documents, so a gate on a homeserver does not
// restrict access: it ends client login and federation outright. Pocket ID and
// the gate itself are the sign-in flow, so gating either gates the login. The
// failure in every case reads as something mysteriously unable
// to sign in, with nothing in the configuration saying why.
//
// Access control for these lives elsewhere: for a homeserver at the identity
// provider, where the group restriction decides who may be provisioned an
// account at all. That is why this is a refusal and not a warning: there is no
// reading of a gated one that works.
func (c *checker) visibilityGateOnUngateableKind() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Gate() == "" || kinds.GateFor(app.Kind).Gateable {
			continue
		}
		c.refuse("visibility-gate-on-ungateable-kind", fmt.Sprintf("apps.%s.visibility_gate", name),
			"is %q on an app of kind %s, which cannot sit behind the gate. The gate redirects a request with no session to a passkey prompt: a Matrix client or a federating server will not follow it, and Pocket ID and the gate are the sign-in flow itself. Set visibility_gate: public.",
			app.VisibilityGate, app.Kind)
	}
}

// visibilityGateWithoutSignedFetch refuses a gate on a federating app whose
// kind records no way to refuse an unsigned ActivityPub read.
//
// The gate sends every ActivityPub request to the app without asking for a
// session, because a federating peer has none and proves who it is by
// signature instead. Anyone can send an ActivityPub Accept header, so that is
// only safe when the app refuses a read that is not signed by an instance on
// its allow list. A kind with no such setting would serve its whole read
// surface to anyone who asked for JSON.
func (c *checker) visibilityGateWithoutSignedFetch() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Gate() == "" || !kinds.Federates(app) || kinds.SignedFetchFor(app) != nil {
			continue
		}
		c.refuse("visibility-gate-without-signed-fetch", fmt.Sprintf("apps.%s.visibility_gate", name),
			"is %q, but this %s app federates and the toolkit records no way for it to refuse an unsigned ActivityPub read. The gate leaves ActivityPub requests to the app, and anyone can send an ActivityPub Accept header, so every page would be readable as JSON by anyone. Turn federation off for this app, or set visibility_gate: public. A WriteFreely checks signatures only in private mode, so for one this also means private is not false.",
			app.VisibilityGate, app.Kind)
	}
}

// visibilityGateProvisional warns about an app gated on provisional.
//
// The provisional instance admits everyone signed in to Pocket ID, including
// people not yet accepted into the community. That is the right gate for a
// surface whose audience is exactly those people, and the wrong one for
// anything member facing, where it reads as protected while admitting anyone
// who made an account.
func (c *checker) visibilityGateProvisional() {
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Gate() != config.GateProvisional {
			continue
		}
		c.warn("visibility-gate-provisional", fmt.Sprintf("apps.%s.visibility_gate", name),
			"is provisional, which admits everyone signed in to Pocket ID, including people who are not yet members. Use member for anything only the community should read.")
	}
}

// homeserverMustBePinned refuses cluster placement for a homeserver.
//
// Synapse's S3 storage provider supplements a local media directory rather
// than replacing it, so the media store cannot follow a failover: a promoted
// site would serve a homeserver whose media is on another machine. The kind
// also renders its own Postgres and an initialisation hook that creates a
// second database beside it, neither of which exists under cluster placement,
// so the authentication service would start against a database nobody created
// and the failure would arrive at the first login rather than at render.
//
// There is no half-pinned form worth having. A stack pinned for its database
// and clustered for its app is less available than either, so this is a
// refusal rather than a warning.
func (c *checker) homeserverMustBePinned() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindSynapse {
			continue
		}
		if app.Placement.Mode != config.PlacementCluster {
			continue
		}
		c.refuse("homeserver-must-be-pinned", fmt.Sprintf("apps.%s.placement", name),
			"is cluster, and the synapse kind has no clustered form. Its media store cannot follow a failover, because the S3 storage provider supplements a local media directory rather than replacing it, and the kind renders its own Postgres plus the hook that creates the authentication service's database beside it. Pin it: placement: { pinned: <site> }.")
	}
}

// mediaHostnameShape refuses a media hostname that cannot be one.
//
// Every app that stores objects serves them on a hostname of its own, derived
// as <label>-media.<domain> from the app's own hostname unless the app
// declares `hostnames.media`. Either way the name becomes a site address on
// the gateway and a URL an application writes into every page, feed and
// federated post, so a name that is not a hostname is a gateway that will not
// load or a URL nothing can fetch.
//
// A declared name must also sit under community.domain. The certificate is
// issued over DNS-01 against the deployment's own zone, and a name outside it
// is a name the configured DNS provider cannot answer a challenge for. A
// derived name is under the domain by construction, and can still fail the
// hostname check: a 60 character label is legal and is not once -media is
// added to it.
func (c *checker) mediaHostnameShape() {
	domain := strings.ToLower(c.cfg.Community.Domain)
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		media := kinds.MediaHostname(app, c.cfg.Community.Domain)
		if media == "" {
			continue
		}
		key := fmt.Sprintf("apps.%s.hostnames.%s", name, kinds.MediaRole)
		derived := kinds.MediaHostnameIsDerived(app)
		if problem := hostnameProblem(media); problem != "" {
			if derived {
				c.refuse("media-hostname-is-not-a-hostname", key,
					"is not declared, and the name derived from apps.%s.hostname is %q, which is not a hostname: %s. The media hostname is <label>-media.<domain>, a sibling of the app's own hostname. Declare hostnames.media to choose another name under %s.",
					name, media, problem, c.cfg.Community.Domain)
			} else {
				c.refuse("media-hostname-is-not-a-hostname", key,
					"is %q, which is not a hostname: %s. It becomes a site address on the gateway and a URL the app publishes to readers, so it has to be a name a browser can fetch, for example %s.",
					media, problem, name+kinds.MediaSuffix+"."+c.cfg.Community.Domain)
			}
			continue
		}
		if derived || domain == "" {
			continue
		}
		if !strings.HasSuffix(media, "."+domain) {
			c.refuse("media-hostname-outside-the-domain", key,
				"is %q, which is not under community.domain (%s). Its certificate is issued over DNS-01 against the deployment's own zone, so the configured DNS provider cannot answer for a name outside it. Use a name under %s, for example %s.",
				media, c.cfg.Community.Domain, c.cfg.Community.Domain, name+kinds.MediaSuffix+"."+c.cfg.Community.Domain)
		}
	}
}

// mediaHostnameUnderAnAppHostname refuses a media hostname that is a child of
// an app's own hostname.
//
// media.talk.example.org would sit under talk.example.org, so any cookie an
// app sets with a Domain attribute on its own host is sent to the media
// hostname too, and a file served from there could set cookies the app would
// then read back. The media hostname exists partly to be a different origin
// from every app, and a child shares exactly the cookie scope that origin was
// meant to leave behind. A sibling, <label>-media.<domain>, shares nothing
// with the app but the community's own domain.
//
// That domain is the one exception, because it cannot be otherwise: every
// hostname in a deployment sits under it, and a synapse kind commonly serves
// its delegation documents on it. A cookie scoped to the community's domain is
// a deliberate choice, made by the gate, and it already reaches every app.
func (c *checker) mediaHostnameUnderAnAppHostname() {
	domain := strings.ToLower(c.cfg.Community.Domain)
	type claim struct{ key, hostname string }
	var appHosts []claim
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		appHosts = append(appHosts, claim{fmt.Sprintf("apps.%s.hostname", name), app.Hostname})
		for _, role := range sortedKeys(app.Hostnames) {
			if role == kinds.MediaRole {
				continue
			}
			appHosts = append(appHosts, claim{fmt.Sprintf("apps.%s.hostnames.%s", name, role), app.Hostnames[role]})
		}
	}
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		media := strings.ToLower(kinds.MediaHostname(app, c.cfg.Community.Domain))
		if media == "" {
			continue
		}
		for _, host := range appHosts {
			parent := strings.ToLower(strings.TrimSpace(host.hostname))
			if parent == "" || parent == domain {
				continue
			}
			if !strings.HasSuffix(media, "."+parent) {
				continue
			}
			c.refuse("media-hostname-under-an-app-hostname", fmt.Sprintf("apps.%s.hostnames.%s", name, kinds.MediaRole),
				"is %q, a child of %q (%s). Cookies an app scopes to its own host reach every name under it, so a media hostname there shares the cookie scope it exists to be outside of. Use a sibling instead, for example %s.",
				media, host.hostname, host.key, name+kinds.MediaSuffix+"."+c.cfg.Community.Domain)
			break
		}
	}
}

// outlineBucketInMediaURL refuses an Outline app whose bucket name appears in
// its media URL.
//
// Outline decides between path style and virtual host addressing by looking
// for the bucket name anywhere in AWS_S3_UPLOAD_BUCKET_URL, as a substring,
// rather than by asking: getPublicEndpoint in server/storage/files/
// S3Storage.ts, read at v1.10.0, is `host.includes(AWS_S3_UPLOAD_BUCKET_NAME)`.
// When it matches, Outline stops appending the bucket to the URL a browser
// uploads to and builds attachment URLs on, while its S3 client still signs
// path style. Garage resolves a bucket from the path here, because the media
// hostname is not under its S3 root domain, so every upload would go to a
// path with no bucket in it and fail after the deployment is up.
//
// With the default names it cannot happen: docs-uploads is not a substring of
// https://docs-media.example.org. A bucket named docs, or one named media, is.
func (c *checker) outlineBucketInMediaURL() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindOutline {
			continue
		}
		media := kinds.MediaHostname(app, c.cfg.Community.Domain)
		if media == "" {
			continue
		}
		bucket := kinds.BucketName(name, app)
		url := "https://" + media
		if !strings.Contains(url, bucket) {
			continue
		}
		c.refuse("outline-bucket-in-media-url", fmt.Sprintf("apps.%s.hostnames.%s", name, kinds.MediaRole),
			"makes the bucket URL %s, which contains the bucket name %q. Outline reads a bucket name anywhere in that URL as virtual host addressing and stops putting the bucket in the path, while Garage needs it there, so every attachment upload would fail. Rename the bucket (settings.s3_bucket) or choose a media hostname that does not contain it.",
			url, bucket)
	}
}

// hostnameProblem says why a name is not a usable DNS hostname, or returns
// empty when it is one. It is the RFC 1123 shape, written lowercase: labels of
// 1 to 63 letters, digits and hyphens, not starting or ending with a hyphen,
// at least two of them, 253 characters in all.
//
// Lowercase is required rather than folded. Caddy and DNS both ignore case,
// but the name is also written into an application's configuration and from
// there into URLs other servers store, and two spellings of one name are two
// URLs to anything comparing them as strings.
func hostnameProblem(host string) string {
	switch {
	case host == "":
		return "it is empty"
	case len(host) > 253:
		return "it is longer than 253 characters"
	case host != strings.ToLower(host):
		return "it contains capital letters; write it lowercase"
	case strings.Contains(host, "://") || strings.ContainsAny(host, "/:"):
		return "it carries a scheme, a port or a path; give the name alone"
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "it is a single label, not a name under a domain"
	}
	for _, label := range labels {
		switch {
		case label == "":
			return "it has an empty label"
		case len(label) > 63:
			return fmt.Sprintf("its label %q is longer than 63 characters", label)
		case label[0] == '-' || label[len(label)-1] == '-':
			return fmt.Sprintf("its label %q starts or ends with a hyphen", label)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Sprintf("its label %q contains %q, and only letters, digits and hyphens are allowed", label, r)
			}
		}
	}
	return ""
}

// duplicateHostname refuses the same public name claimed twice.
//
// Every hostname an app declares, primary or role, becomes a site address in
// the gateway's Caddyfile. Caddy refuses a configuration in which two site
// blocks claim the same address, because it cannot decide which one a request
// belongs to, and it refuses the whole file rather than the one block. So a
// name typed twice takes every hostname in the deployment down, not the one
// that was duplicated.
//
// It became reachable when an app gained the ability to answer on several
// hostnames: before that an app had exactly one name and a clash needed two
// apps to be given the same one, which is hard to do by accident. Setting one
// app's `hostname` to the value another app already uses for a role is not.
// This is the same failure unknown-hostname-role and gate-without-a-gate-app
// exist to prevent, arrived at from a different direction.
func (c *checker) duplicateHostname() {
	type claim struct {
		key, hostname string
		// derived is set for a media hostname nobody wrote, so the message
		// can say where the name came from rather than quoting a line the
		// file does not have.
		derived bool
	}
	describe := func(cl claim) string {
		if cl.derived {
			return cl.key + ", derived from the app's own hostname,"
		}
		return cl.key
	}
	first := map[string]claim{} // hostname -> the claim that took it first
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		claims := []claim{{key: fmt.Sprintf("apps.%s.hostname", name), hostname: app.Hostname}}
		for _, role := range sortedKeys(app.Hostnames) {
			claims = append(claims, claim{key: fmt.Sprintf("apps.%s.hostnames.%s", name, role), hostname: app.Hostnames[role]})
		}
		// A derived media hostname is claimed exactly as if it had been
		// written, because it becomes a site address exactly as if it had
		// been. It is the claim most likely to collide unseen: an app whose
		// hostname is talk-media.example.org clashes with talk's media
		// hostname, and neither line in the file says so.
		if kinds.MediaHostnameIsDerived(app) {
			if media := kinds.MediaHostname(app, c.cfg.Community.Domain); media != "" {
				claims = append(claims, claim{key: fmt.Sprintf("apps.%s.hostnames.%s", name, kinds.MediaRole), hostname: media, derived: true})
			}
		}
		for _, cl := range claims {
			if cl.hostname == "" {
				continue
			}
			if earlier, taken := first[cl.hostname]; taken {
				said := fmt.Sprintf("is %q", cl.hostname)
				if cl.derived {
					said = fmt.Sprintf("is not declared, so it is derived as %q", cl.hostname)
				}
				c.refuse("duplicate-hostname", cl.key,
					"%s, which %s already claims. Every hostname becomes a site address in the gateway's Caddyfile, and Caddy refuses a configuration where two site blocks claim one address rather than choosing between them, so this takes every hostname in the deployment down rather than these two. Give each name to one app and one role; a media hostname can be chosen with hostnames.media.",
					said, describe(earlier))
				continue
			}
			first[cl.hostname] = cl
		}
	}
}

// hostnameRoles refuses a hostname role the kind ships no snippet for.
//
// A role selects a Caddy snippet. A role nobody ships renders a host block
// importing a file that does not exist, so the gateway fails to load its whole
// configuration, taking every other hostname down with it. That is incoherent
// rather than risky.
func (c *checker) hostnameRoles() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		for _, role := range sortedKeys(app.Hostnames) {
			if kinds.HasHostnameRole(app.Kind, role) {
				continue
			}
			c.refuse("unknown-hostname-role", fmt.Sprintf("apps.%s.hostnames.%s", name, role),
				"names a hostname role %q, which the %s kind does not understand. A role selects the Caddy snippet that hostname gets, so an unknown one would import a file that does not exist and the gateway would fail to load at all. Roles for this kind: %s.",
				role, app.Kind, strings.Join(kinds.HostnameRoles(app.Kind), ", "))
		}
	}
}

// configKeyIsNestedInAnEnvFile refuses a dotted key on a kind configured by
// environment.
//
// Env has no nesting. In ini and yaml a dot is a path and means something, so
// the same habit carried over here produces a variable called `a.b` that the
// application never reads and the shell may not even accept. It is refused
// rather than passed through because the likeliest reading is a typo for an
// underscore, and a typo that renders is the worst outcome available.
func (c *checker) configKeyIsNestedInAnEnvFile() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if kinds.ConfigFormat(app.Kind) != kinds.ConfigEnv {
			continue
		}
		for _, key := range sortedKeys(app.Config) {
			if !strings.Contains(key, ".") {
				continue
			}
			c.refuse("config-key-is-nested-in-an-env-file", fmt.Sprintf("apps.%s.config.%s", name, key),
				"is a dotted key, and %s is an env file. Env has no nesting, so a dot is a typo here rather than a path. Write the variable name the application reads, for example with underscores.",
				kinds.ConfigFile(app.Kind))
		}
	}
}

// secretWords are the fragments of a key name that suggest a credential,
// matched against the lowercased name with `_`, `-` and `.` removed, so that
// SENDGRID_API_KEY, privateKey and private-key all read as the same word.
var secretWords = []string{"password", "secret", "token", "apikey", "privatekey"}

// configKeyLooksLikeASecret refuses a key named like a credential.
//
// paisans.yaml is plaintext and meant to be read, committed and diffed. A
// passthrough map is exactly where somebody pastes an API token at the end of
// a long day. The check is by name and is deliberately narrow: it refuses the
// one thing it can see, and says so, rather than claiming to be a policy.
func (c *checker) configKeyLooksLikeASecret() {
	squash := strings.NewReplacer("_", "", "-", "", ".", "")
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		for _, key := range sortedKeys(app.Config) {
			squashed := squash.Replace(strings.ToLower(key))
			for _, word := range secretWords {
				if !strings.Contains(squashed, word) {
					continue
				}
				c.refuse("config-key-looks-like-a-secret", fmt.Sprintf("apps.%s.config.%s", name, key),
					"is named like a credential (it contains %q, ignoring case and the separators _ - and .). paisans.yaml is plaintext and meant to be committed, so a secret does not belong in it: put the value in secrets.enc.yaml. This check is by name and will not catch a credential named something else, so it is no substitute for looking.",
					word)
				break
			}
		}
	}
}

// composeSteeringPrefixes are the variable name prefixes refused on an env
// kind by configKeySteersCompose.
var composeSteeringPrefixes = []string{"COMPOSE_", "DOCKER_"}

// configKeySteersCompose refuses an env key that would configure compose
// rather than the application.
//
// apply runs `docker compose -f /srv/paisans/<token>/<app>/compose.yaml up -d`, and compose
// reads the .env beside that file for its own settings as well as handing it
// to the containers. Run with `docker compose config` on Docker Compose
// v5.4.0 (the operator's Mac, not a host): COMPOSE_PROJECT_NAME in that .env
// renamed the project and COMPOSE_PROFILES changed which services were
// active. That apply would then address containers under another project
// name than the ones it started before is reasoned, not run. DOCKER_HOST in
// the same .env did NOT redirect `docker compose ps` in that run; DOCKER_ is
// refused anyway as the docker CLI's own namespace, which no application
// variable should need, so the rule does not depend on knowing which of
// those compose honours from a file.
func (c *checker) configKeySteersCompose() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if kinds.ConfigFormat(app.Kind) != kinds.ConfigEnv {
			continue
		}
		for _, key := range sortedKeys(app.Config) {
			for _, prefix := range composeSteeringPrefixes {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				c.refuse("config-key-steers-compose", fmt.Sprintf("apps.%s.config.%s", name, key),
					"starts with %s. When apply runs compose for this stack, compose reads the stack's %s for its own settings as well as handing it to the application, and a variable there can rename the project or change which services are active. That is compose's configuration, not the application's: it belongs in the toolkit, not in config.",
					prefix, kinds.ConfigFile(app.Kind))
				break
			}
		}
	}
}

// watchdogOffOnDataSite warns that a data site's Patroni runs unfenced.
//
// It is a warning rather than a refusal because there are hosts with no device
// and no way to load one (some container based VPS kernels), and a one node
// cluster has nobody to split brain with. It is still risky: without a
// watchdog, a Patroni that hangs while holding the leader key can keep
// accepting writes after its lease expires and another node is promoted.
func (c *checker) watchdogOffOnDataSite() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if site.Has(config.RoleData) && site.WatchdogMode() == config.WatchdogOff {
			c.warn("watchdog-off-on-data-site", fmt.Sprintf("sites.%s.watchdog", name),
				"is off on a site holding the data role. Patroni is rendered with PATRONI_WATCHDOG_MODE=off, so a Patroni that hangs while it is leader is not fenced and can keep taking writes after another node is promoted. Use auto unless this host genuinely cannot load any watchdog driver.")
		}
	}
}

// uptimeNeedsAnAdminGroup refuses an uptime app that names no admin group.
//
// The fork admits exactly the members of the groups it is given and refuses
// everyone else, and group names are the community's own, so there is no
// default to fall back to: without one, nobody but the break glass password
// could ever sign in.
func (c *checker) uptimeNeedsAnAdminGroup() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindUptime {
			continue
		}
		if group, _ := app.Settings["admin_group"].(string); strings.TrimSpace(group) != "" {
			continue
		}
		c.refuse("uptime-needs-an-admin-group", fmt.Sprintf("apps.%s.settings.admin_group", name),
			"is required. Name the identity provider group whose members may sign in to the monitor, for example admins. Only that group is admitted, and group names are this community's own, so there is no default.")
	}
}

// adminGroupNotAdmins warns about an app that reads its administrators from a
// group other than admins in a deployment that runs Pocket ID. The admin
// reconciler beside Pocket ID keeps every Pocket ID administrator in admins and
// fails its health check while admins has fewer than two members
// (docs/specs/2026-10-08-admin-reconciler.md), so an app reading another group is
// one the reconciler does not reconcile or report on.
func (c *checker) adminGroupNotAdmins() {
	runsPocketID := false
	for _, app := range c.cfg.Apps {
		if app.Kind == config.KindPocketID {
			runsPocketID = true
		}
	}
	if !runsPocketID {
		return
	}
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
		if !ok {
			continue
		}
		admin, _ := spec.Groups(app)
		if admin == "" || admin == adminreconciler.Group {
			continue
		}
		c.warn("admin-group-not-admins", spec.AdminGroupSource(name),
			"is %s. The admin reconciler keeps Pocket ID's administrators in %s and alerts while it has fewer than two members, so %s's administrators are a group nobody is watching. Name %s here, or accept that this app's admin group is unwatched.",
			admin, adminreconciler.Group, name, adminreconciler.Group)
	}
}

// oidcMemberGroupNotAName refuses a member_group or admin_group setting that
// is not a group name.
//
// A blank or non-string value is read as absent, and the kind's default
// holds, so the client is still restricted. But the operator wrote something,
// and what they wrote is not what the client admits; left alone, that reads
// as a restriction to a group that does not exist.
func (c *checker) oidcMemberGroupNotAName() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
		if !ok {
			continue
		}
		for _, key := range []string{spec.MemberGroupSetting, spec.AdminGroupSetting} {
			if key == "" {
				continue
			}
			v, present := app.Settings[key]
			if !present {
				continue
			}
			if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
				continue
			}
			c.refuse("oidc-member-group-not-a-name", fmt.Sprintf("apps.%s.settings.%s", name, key),
				"is %v, which is not a group name. Name the identity provider group, or remove the key to take the kind's default. The client at Pocket ID admits exactly the groups named here, so a value that is not a name is a restriction to nothing.", v)
		}
	}
}

// oidcMemberGroupDisagreesWithGate refuses a member gated app whose client
// admits a different member group than the gate does.
//
// Two groups decide who reads a member gated app: the gate's members
// instance admits its members_group, and the app's client at Pocket ID admits
// the app's member group. When they differ, someone in one and not the other
// is let through the gate and refused by the app, or admitted by the app on a
// path the gate does not cover, and nothing in the configuration says which
// group "member" means. Both default to the same name, so this fires only
// when one was changed and the other was not.
func (c *checker) oidcMemberGroupDisagreesWithGate() {
	gateApp := ""
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Kind == config.KindOAuth2Proxy {
			gateApp = name
		}
	}
	if gateApp == "" {
		return
	}
	gateGroup := kinds.GateMembersGroup(c.cfg.Apps[gateApp])
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Gate() != config.GateMember {
			continue
		}
		spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
		if !ok {
			continue
		}
		_, member := spec.Groups(app)
		if member == gateGroup {
			continue
		}
		c.refuse("oidc-member-group-disagrees-with-gate", fmt.Sprintf("apps.%s.settings.%s", name, spec.MemberGroupSetting),
			"is %s, but apps.%s.settings.%s is %s, and this app is behind the member gate. The gate admits one group and the app's client at Pocket ID admits the other, so the two disagree about who a member is. Name the same group in both.",
			member, gateApp, kinds.GateMembersGroupSetting, gateGroup)
	}
}

// oidcClientUnrestricted refuses a gateable app whose client would admit
// every Pocket ID account.
//
// A kind that can sit behind the gate is member facing, and its client is the
// control that stops a person outside the community before the app sees them.
// Every such kind has a default member group today, so this is the floor
// under that: a kind added without one, or a change that empties one, is
// refused here rather than shipping an open client.
func (c *checker) oidcClientUnrestricted() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
		if !ok || !kinds.GateFor(app.Kind).Gateable {
			continue
		}
		if _, member := spec.Groups(app); member != "" {
			continue
		}
		c.refuse("oidc-client-unrestricted", fmt.Sprintf("apps.%s", name),
			"names no member group, so its client at Pocket ID would admit anyone with an account there. A member facing app's client is restricted to the member group and admins; name one in apps.%s.settings.%s.", name, spec.MemberGroupSetting)
	}
}

// smtpOnAKindWithoutMail refuses an app level smtp block that nothing reads.
func (c *checker) smtpOnAKindWithoutMail() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.SMTP == nil || kinds.SendsMail(app.Kind) {
			continue
		}
		c.refuse("smtp-on-a-kind-without-mail", fmt.Sprintf("apps.%s.smtp", name),
			"is declared, but %s does not read the toolkit's smtp settings, so this override would be ignored without a word. Remove it, or configure that application's mail where it keeps it.",
			app.Kind)
	}
}

// uptimeWithoutSMTP warns that a monitor with no mail server cannot email.
func (c *checker) uptimeWithoutSMTP() {
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Kind != config.KindUptime || c.cfg.SMTPFor(name).Host != "" {
			continue
		}
		c.warn("uptime-without-smtp", fmt.Sprintf("apps.%s.smtp", name),
			"resolves no SMTP host, so no email channel in the monitor can send. It still checks and records incidents. Declare a top level smtp block, or one on this app, or set SMTP in the monitor's own settings page, which the toolkit then leaves alone.")
	}
}
