// Package config loads and structurally checks a paisans.yaml deployment
// declaration. It performs no policy checks: those live in internal/validate,
// so that every problem in a file can be reported at once rather than one per
// run.
package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// Role is a capability a site provides. Sites declare roles; apps declare
// placement. Keeping those separate is what stops every new app from becoming
// a new role.
type Role string

const (
	RoleData    Role = "data"
	RoleApps    Role = "apps"
	RoleGateway Role = "gateway"
	RoleWitness Role = "witness"
	// RoleStorage is a host that runs Garage and nothing else. A data or apps
	// site that also runs Garage needs no such role: storage.garage.sites is
	// what places Garage, and this role only names a host that exists for it,
	// so its purpose reads in its own entry. Validation refuses it on a site
	// that list does not name. Founder decision, over accepting an empty
	// role list for such a site.
	RoleStorage Role = "storage"
	// RoleMonitor is a host that runs the uptime monitor. It is never the
	// gateway or the witness, because reporting their failures is its job,
	// and it serves its own apps rather than routing them through the
	// gateway, which would take it dark at exactly the moment it is needed.
	// See docs/specs/2026-10-08-monitor-role-and-host-check.md. Founder
	// decision.
	RoleMonitor Role = "monitor"
)

var knownRoles = map[Role]bool{RoleData: true, RoleApps: true, RoleGateway: true, RoleWitness: true, RoleStorage: true, RoleMonitor: true}

// Kind is an application the toolkit knows how to render.
type Kind string

const (
	KindElement     Kind = "element"
	KindMbin        Kind = "mbin"
	KindOAuth2Proxy Kind = "oauth2-proxy"
	KindOutline     Kind = "outline"
	KindPocketID    Kind = "pocket-id"
	KindSynapse     Kind = "synapse"
	KindWriteFreely Kind = "writefreely"
	KindUptime      Kind = "uptime"
)

var knownKinds = map[Kind]bool{
	KindElement: true, KindMbin: true, KindOAuth2Proxy: true, KindOutline: true, KindPocketID: true, KindSynapse: true, KindWriteFreely: true, KindUptime: true,
}

// Kinds returns every kind this toolkit knows, sorted. It exists so that a
// test asserting something about every kind, such as that each ships a
// pinned default image, walks this list rather than carrying a second, hand
// maintained one that can fall out of step with knownKinds the moment a kind
// is added.
func Kinds() []Kind {
	out := make([]Kind, 0, len(knownKinds))
	for k := range knownKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Config is a whole deployment, declared. It records intent and never status:
// which node is primary lives in etcd, not here.
type Config struct {
	Version int `yaml:"version"`
	// ID is the deployment's identity, a version 4 UUID that `paisans init`
	// writes once and nothing changes afterwards. Every name and path on a host
	// is derived from it (see internal/deployment), and the host registry is
	// keyed by it, so changing it would orphan everything already deployed.
	ID        string    `yaml:"id"`
	Community Community `yaml:"community"`
	Mesh      Mesh      `yaml:"mesh"`
	// ACME is how certificates are obtained. It is deployment wide rather than
	// a property of the gateway site, because the gateway role moves between
	// machines by design and certificates do not move with it.
	ACME    ACME            `yaml:"acme"`
	Sites   map[string]Site `yaml:"sites"`
	Cluster Cluster         `yaml:"cluster"`
	Etcd    Etcd            `yaml:"etcd"`
	Storage Storage         `yaml:"storage"`
	// SMTP is how apps send mail, the default for every app that sends any.
	// An app's own smtp block overrides it field by field: see SMTPFor.
	SMTP SMTP           `yaml:"smtp"`
	Apps map[string]App `yaml:"apps"`

	// Path is where this configuration was read from. Error messages use it.
	Path string `yaml:"-"`
}

type Community struct {
	Name string `yaml:"name"`
	// Domain is a one way door. Federation identity is the hostname and remote
	// instances have recorded it, so changing it after federating is a rebuild.
	Domain string `yaml:"domain"`
}

// Mesh is the private network every site sits on.
//
// The subnet is declared rather than derived. Applications are rendered to
// trust it for forwarded client addresses, so it has to be a value that never
// changes: deriving the tightest network containing today's site addresses
// would widen it the moment a site was added, silently rewriting
// TRUSTED_PROXIES in every app. Declaring it also lets an operator pick a
// range that does not collide with something their hosts already route.
type Mesh struct {
	Subnet string `yaml:"subnet"`
}

// Prefix returns the subnet's prefix length, for rendering an interface
// address. It assumes the subnet has already been checked.
func (m Mesh) Prefix() int {
	_, network, err := net.ParseCIDR(m.Subnet)
	if err != nil {
		return 0
	}
	ones, _ := network.Mask.Size()
	return ones
}

// Contains reports whether an address sits inside the mesh.
func (m Mesh) Contains(address string) bool {
	_, network, err := net.ParseCIDR(m.Subnet)
	if err != nil {
		return false
	}
	ip := net.ParseIP(address)
	return ip != nil && network.Contains(ip)
}

// ACME is the certificate story: which DNS provider answers the challenge, and
// optionally which Caddy image carries that provider's module.
//
// DNS-01 is not configurable. A gateway has to be able to hold valid
// certificates before DNS points at it, which is what makes moving a gateway an
// overlap rather than a cutover, and a hostname served behind a VPN has nothing
// on the internet that can answer an HTTP-01 challenge.
type ACME struct {
	// Provider is the Caddy DNS provider name, as it appears in `acme_dns`.
	Provider string `yaml:"provider"`
	// Image overrides the Caddy image. It is only meaningful for a provider the
	// toolkit publishes no image for: an image is the only way that provider's
	// module reaches the gateway, since nothing is built on a host.
	Image string `yaml:"image"`
}

type Site struct {
	Roles    []Role `yaml:"roles"`
	Address  string `yaml:"address"`
	Endpoint string `yaml:"endpoint"`
	// SSH is how the toolkit reaches the site and who may log in to it. See
	// the SSH type.
	SSH SSH `yaml:"ssh"`

	// PublicAddress is the IPv4 address the internet reaches this site on. It
	// is optional, and only `paisans dns` reads it: it is the content of the A
	// records that command creates. It is declared rather than discovered
	// because the address a host sees on its own interface is often not the
	// one the internet sees, behind NAT or a cloud provider's 1:1 mapping, and
	// a guessed address published in DNS sends every visitor somewhere wrong.
	PublicAddress string `yaml:"public_address"`
	// PublicAddress6 is the IPv6 counterpart, for AAAA records. Optional even
	// where PublicAddress is set.
	PublicAddress6 string `yaml:"public_address6"`
	// Watchdog is how `host prepare` gives Patroni a /dev/watchdog on this
	// site. It only matters where the site holds the data role. Empty means
	// auto; read it through WatchdogMode rather than directly.
	Watchdog WatchdogMode `yaml:"watchdog"`
	// Ingress is what sits in front of a monitor site's apps. Only a site
	// holding the monitor role may declare it: a gateway always runs the
	// toolkit's Caddy, and every other site is reached through the gateway.
	// Absent means mode paisans; read it through IngressMode.
	Ingress *Ingress `yaml:"ingress"`
}

// IngressMode is what serves a monitor site's apps to the internet.
type IngressMode string

const (
	// IngressPaisans runs the toolkit's own Caddy on the monitor, the same
	// image as the gateway's, with certificates over DNS-01.
	IngressPaisans IngressMode = "paisans"
	// IngressExternal runs nothing in front of the app: a web server the
	// operator already runs terminates TLS and proxies to Listen. Its
	// certificates are the operator's, because the toolkit cannot know how
	// an unfamiliar proxy obtains them and must not edit a configuration it
	// does not own.
	IngressExternal IngressMode = "external"
)

var knownIngressModes = map[IngressMode]bool{IngressPaisans: true, IngressExternal: true}

// Ingress is how the internet reaches a monitor site's apps.
type Ingress struct {
	Mode IngressMode `yaml:"mode"`
	// Listen is where the app is published for the operator's web server, as
	// an IPv4 address and a port. Required with mode external, refused with
	// mode paisans.
	Listen string `yaml:"listen"`
}

// ListenHostPort splits Listen into an IPv4 address and a port, false when it
// is not one.
func (i Ingress) ListenHostPort() (string, int, bool) {
	host, port, err := net.SplitHostPort(i.Listen)
	if err != nil || !isIPv4(host) {
		return "", 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, false
	}
	return host, n, true
}

// IngressMode returns the site's declared ingress mode, paisans when none is
// declared. It is meaningful only on a monitor site.
func (s Site) IngressMode() IngressMode {
	if s.Ingress == nil || s.Ingress.Mode == "" {
		return IngressPaisans
	}
	return s.Ingress.Mode
}

// RunsCaddy reports whether the toolkit's Caddy runs on the site: on a
// gateway, and on a monitor that serves its own apps.
func (s Site) RunsCaddy() bool {
	return s.Has(RoleGateway) || (s.Has(RoleMonitor) && s.IngressMode() == IngressPaisans)
}

// WatchdogMode is how a data site's watchdog device is provided.
//
// It is declared per site because the answer is a property of the machine: a
// board with an iTCO timer, a VM with an emulated i6300esb, and a small cloud
// instance with nothing at all each want a different one, and the toolkit
// cannot tell which from the configuration alone.
type WatchdogMode string

const (
	// WatchdogAuto uses whatever device the host already has, whatever its
	// driver, and falls back to loading softdog with a warning.
	WatchdogAuto WatchdogMode = "auto"
	// WatchdogRequired refuses a host whose only watchdog is softdog, or
	// which has none.
	WatchdogRequired WatchdogMode = "required"
	// WatchdogSoftdog loads and persists softdog because the operator chose
	// it, so no warning is printed.
	WatchdogSoftdog WatchdogMode = "softdog"
	// WatchdogOff leaves the host alone and renders Patroni without a
	// watchdog at all.
	WatchdogOff WatchdogMode = "off"
)

var knownWatchdogModes = map[WatchdogMode]bool{WatchdogAuto: true, WatchdogRequired: true, WatchdogSoftdog: true, WatchdogOff: true}

// WatchdogMode returns the site's declared mode, auto when none is declared.
func (s Site) WatchdogMode() WatchdogMode {
	if s.Watchdog == "" {
		return WatchdogAuto
	}
	return s.Watchdog
}

// Has reports whether the site declares a role.
func (s Site) Has(r Role) bool {
	for _, got := range s.Roles {
		if got == r {
			return true
		}
	}
	return false
}

type Cluster struct {
	// Sites is the cluster's data sites: as written, or, when the file
	// leaves the key out, every site with the data role, in name order.
	// Load fills it either way, so every reader reads one list.
	Sites []string `yaml:"sites"`
	// SitesDerived records that the file left cluster.sites out and Sites
	// came from the roles. validate's comparisons between the list and the
	// data role hold by construction then, and the yaml writers leave a key
	// the file does not have alone.
	SitesDerived      bool   `yaml:"-"`
	Port              int    `yaml:"port"`
	PostgresVersion   string `yaml:"postgres_version"`
	Synchronous       bool   `yaml:"synchronous"`
	SynchronousStrict bool   `yaml:"synchronous_strict"`
}

type Etcd struct {
	// Members is etcd's voters: as written, or, when the file leaves the
	// key out, every site with the witness role, then every site with the
	// data role, each in name order. Witnesses come first because that is
	// the order a new deployment is founded in. Load fills it either way.
	Members []string `yaml:"members"`
	// MembersDerived records that the file left etcd.members out, as
	// Cluster.SitesDerived does for cluster.sites.
	MembersDerived    bool `yaml:"-"`
	HeartbeatMS       int  `yaml:"heartbeat_ms"`
	ElectionTimeoutMS int  `yaml:"election_timeout_ms"`
}

type Storage struct {
	Garage Garage `yaml:"garage"`
	// There is no deployment wide media hostname. Each app that stores objects
	// serves them on a hostname of its own, derived from its own hostname and
	// overridable as `hostnames.media` on that app: see kinds.MediaHostname.
	// A single hostname with a bucket as a path under it was the earlier
	// shape, and it was replaced because a path cannot be pointed at a CDN or
	// another provider one app at a time, while a hostname can.
}

type Garage struct {
	Sites       []string `yaml:"sites"`
	Replication int      `yaml:"replication"`
	// Capacity is what this node advertises to Garage's layout. Garage
	// requires a unit suffix, as in 100G. It is not derived from the disk
	// because the toolkit cannot know how much of that disk is meant for
	// objects. Defaulted in Load when left unset.
	Capacity string `yaml:"capacity"`
	// Capacities overrides Capacity for the sites it names, so nodes of
	// different sizes can share a layout: Garage spreads partitions in
	// proportion to capacity. Founder decision, over a field on each site,
	// which would spread Garage's configuration across site entries.
	Capacities map[string]string `yaml:"capacities"`
	// Consistency is Garage's consistency_mode: consistent, degraded or
	// dangerous. At replication 2 on two sites, dangerous is the only mode in
	// which uploads continue while a site is down, at the price of an upload
	// being confirmed once one copy exists (Garage v1.0.1,
	// doc/book/reference-manual/configuration.md, "consistency_mode").
	// Garage reads it at start and does not record it in the layout, so
	// changing it is an ordinary apply. Defaulted in Load when left unset.
	Consistency string `yaml:"consistency"`
}

// Garage consistency modes, as Garage v1.0.1 spells them
// (src/rpc/replication_mode.rs).
const (
	GarageConsistent = "consistent"
	GarageDegraded   = "degraded"
	GarageDangerous  = "dangerous"
)

type App struct {
	Kind      Kind      `yaml:"kind"`
	Hostname  string    `yaml:"hostname"`
	Placement Placement `yaml:"placement"`
	// Images overrides the container image a service runs, keyed by service
	// name in the kind's compose template. Every key is optional and an unset
	// one takes the default that kind ships.
	//
	// It is a map rather than a single reference because a stack is several
	// containers, and it is one full reference per key rather than a repository
	// and a tag because switching to a fork changes registry, repository and
	// tag together: split into two fields, a half finished switch parses
	// cleanly and pulls something nobody intended.
	//
	// Changing this is not changing Kind. A fork that renames a variable or
	// moves a config path needs a different template set, which is a different
	// kind; this is for a different build of the same software.
	Images   map[string]string `yaml:"images"`
	Settings map[string]any    `yaml:"settings"`

	// Config is passed through to the file this app's kind reads, in that file's
	// own syntax, without the toolkit interpreting the key.
	//
	// It is not `settings`. A setting is an input the toolkit reasons about: it
	// reads it, sometimes validates it, and decides things with it. A config key
	// is one the toolkit has no opinion about and only places.
	//
	// A key the kind's template already writes is refused rather than overridden,
	// because two sources of truth for one value is how a deployment ends up with
	// a setting nobody can locate.
	Config map[string]any `yaml:"config"`

	// Hostnames are the additional names this app answers on, keyed by a role
	// the kind understands. The primary hostname stays in Hostname; a role here
	// selects a different Caddy snippet, because a second hostname usually
	// exists to serve something different rather than the same thing twice.
	//
	// `media` is the one role that exists without being declared: every kind
	// that stores objects serves them on <label>-media.<domain>, and declaring
	// it here only chooses another name. See kinds.MediaHostname.
	Hostnames map[string]string `yaml:"hostnames"`

	// VisibilityGate is who may read the app through the gateway: public,
	// member or provisional. Empty means public. Read it through Gate.
	//
	// It is declared rather than inferred because it is access policy. It is
	// also the setting most likely to be wrong in a way nothing notices: a
	// gated Matrix hostname authenticates a browser and breaks every client,
	// because a client will not follow a redirect to a passkey prompt.
	VisibilityGate string `yaml:"visibility_gate"`

	// SMTP overrides the deployment's smtp block for this app, field by
	// field: a field left out is inherited. Only kinds that send mail read
	// it, and validate refuses it on any other (smtp-on-a-kind-without-mail).
	SMTP *SMTP `yaml:"smtp"`
}

// The values of visibility_gate. Public is also what an absent key means.
const (
	GatePublic      = "public"
	GateMember      = "member"
	GateProvisional = "provisional"
)

// Gate is the gate instance in front of the app: member, provisional, or
// empty when the app is public. An absent key and an explicit public mean
// the same thing, so callers never compare against both.
func (a App) Gate() string {
	if a.VisibilityGate == GatePublic {
		return ""
	}
	return a.VisibilityGate
}

// SMTP is how an app sends mail. The deployment's block is the default for
// every app that sends any; an app's own block overrides it field by field, so
// one app can use a different sender or a different account without
// repeating the rest. The password is not here: it is a secret, under
// apps.<app>.smtp_password or else external.smtp_password.
type SMTP struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Security    string `yaml:"security"`
	Username    string `yaml:"username"`
	FromAddress string `yaml:"from_address"`
	FromName    string `yaml:"from_name"`
}

// SMTP security modes. There is no "none": no consumer needs it, and the
// uptime fork cannot express it, since its smtp_secure is nodemailer's
// boolean `secure` and false already means STARTTLS.
const (
	SMTPStartTLS = "starttls"
	SMTPTLS      = "tls"
)

// PortOrDefault is the declared port, or the conventional one for the
// security mode: 465 for implicit TLS, 587 for STARTTLS.
func (s SMTP) PortOrDefault() int {
	if s.Port != 0 {
		return s.Port
	}
	if s.Security == SMTPTLS {
		return 465
	}
	return 587
}

// SMTPFor is an app's effective SMTP settings: the deployment's block with
// every field the app declares replacing the deployment's.
func (c *Config) SMTPFor(app string) SMTP {
	out := c.SMTP
	o := c.Apps[app].SMTP
	if o == nil {
		return out
	}
	if o.Host != "" {
		out.Host = o.Host
	}
	if o.Port != 0 {
		out.Port = o.Port
	}
	if o.Security != "" {
		out.Security = o.Security
	}
	if o.Username != "" {
		out.Username = o.Username
	}
	if o.FromAddress != "" {
		out.FromAddress = o.FromAddress
	}
	if o.FromName != "" {
		out.FromName = o.FromName
	}
	return out
}

// smtpProblems checks one smtp block's shape, wherever it is declared.
func smtpProblems(key string, s SMTP) []string {
	var out []string
	if s.Security != "" && s.Security != SMTPStartTLS && s.Security != SMTPTLS {
		out = append(out, fmt.Sprintf("%s.security: unknown mode %q. Valid modes are starttls and tls.", key, s.Security))
	}
	if s.Port < 0 || s.Port > 65535 {
		out = append(out, fmt.Sprintf("%s.port: %d is not a port. Give one from 1 to 65535, or leave it out for 587 with starttls and 465 with tls.", key, s.Port))
	}
	return out
}

// PlacementMode is where an app runs. There are exactly two, and pinned is the
// plain case: an app exactly as upstream ships it, self contained at one site.
type PlacementMode string

const (
	PlacementCluster PlacementMode = "cluster"
	PlacementPinned  PlacementMode = "pinned"
	// PlacementInvalid records a placement the file declared that is neither of
	// the above. It is carried rather than returned as a decode error so that
	// validate can report it alongside every other problem in the file.
	PlacementInvalid PlacementMode = "invalid"
)

type Placement struct {
	Mode PlacementMode
	Site string
	// Literal is what the file actually said, quoted back in the error when the
	// placement is invalid.
	Literal string
}

// UnmarshalYAML accepts the two forms the design allows, `cluster` and
// `{ pinned: <site> }`, and records anything else as invalid instead of
// failing the decode.
func (p *Placement) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		if s == string(PlacementCluster) {
			p.Mode = PlacementCluster
			p.Literal = s
			return nil
		}
		p.Mode = PlacementInvalid
		p.Literal = s
		return nil
	}
	var m map[string]string
	if err := node.Decode(&m); err == nil && len(m) == 1 {
		if site, ok := m["pinned"]; ok && site != "" {
			p.Mode = PlacementPinned
			p.Site = site
			p.Literal = fmt.Sprintf("{ pinned: %s }", site)
			return nil
		}
	}
	p.Mode = PlacementInvalid
	p.Literal = describeNode(node)
	return nil
}

func describeNode(node *yaml.Node) string {
	var v any
	if err := node.Decode(&v); err != nil {
		return "unreadable value"
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return "unreadable value"
	}
	return trimTrailingNewline(string(out))
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// Load reads and structurally checks a configuration file. Unknown keys are an
// error: `trusted_proxies` is deliberately absent from the schema, and a typo
// that silently does nothing is worse than a refusal.
func Load(path string) (*Config, error) { return load(path, false) }

// LoadForInit is Load for `paisans init`, which is what gives a declaration
// its mesh.subnet: a file without one is accepted, and every other problem
// is refused as Load refuses it. Nothing else may run on such a file, since
// every address a host binds is in that subnet.
func LoadForInit(path string) (*Config, error) { return load(path, true) }

func load(path string, noSubnet bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(newReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.Path = path
	written, err := writtenLists(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.deriveLists(written)
	if cfg.Storage.Garage.Capacity == "" {
		cfg.Storage.Garage.Capacity = "100G"
	}
	if cfg.Storage.Garage.Consistency == "" {
		cfg.Storage.Garage.Consistency = GarageConsistent
	}
	if err := cfg.structural(noSubnet); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Lists a file may leave out, to be derived from the roles.
var (
	membersKey      = []string{"etcd", "members"}
	clusterSitesKey = []string{"cluster", "sites"}
)

// writtenLists reports which of etcd.members and cluster.sites the file has,
// with any value, so an empty list or a key with no value counts as written.
// The decoded struct cannot tell a key left out from one written empty.
func writtenLists(data []byte) (map[string]bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return out, nil
	}
	for _, keys := range [][]string{membersKey, clusterSitesKey} {
		out[strings.Join(keys, ".")] = walk(doc.Content[0], keys...) != nil
	}
	return out, nil
}

// deriveLists fills each of etcd.members and cluster.sites the file left out
// from the roles, and records that it did.
func (c *Config) deriveLists(written map[string]bool) {
	if !written["etcd.members"] {
		c.Etcd.Members = c.DerivedEtcdMembers()
		c.Etcd.MembersDerived = true
	}
	if !written["cluster.sites"] {
		c.Cluster.Sites = c.DataSites()
		c.Cluster.SitesDerived = true
	}
}

// EtcdMembersKey names etcd.members in a message, saying when the list was
// derived, so an operator who never wrote the key is not sent looking for it.
func (c *Config) EtcdMembersKey() string {
	if c.Etcd.MembersDerived {
		return "etcd.members (derived from the roles)"
	}
	return "etcd.members"
}

// ClusterSitesKey names cluster.sites in a message, as EtcdMembersKey does.
func (c *Config) ClusterSitesKey() string {
	if c.Cluster.SitesDerived {
		return "cluster.sites (derived from the roles)"
	}
	return "cluster.sites"
}

// DerivedEtcdMembers is etcd.members as the roles give it: every witness, in
// name order, then every data site, in name order.
func (c *Config) DerivedEtcdMembers() []string {
	var witnesses []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].Has(RoleWitness) {
			witnesses = append(witnesses, name)
		}
	}
	return append(witnesses, c.DataSites()...)
}

// Deployment is this configuration's identity and every name and path
// derived from it.
func (c *Config) Deployment() deployment.Deployment {
	return deployment.Deployment{ID: c.ID}
}

// SiteNames returns declared site names in sorted order. Rendering and
// reporting both iterate sites, and both must be deterministic.
func (c *Config) SiteNames() []string {
	names := make([]string, 0, len(c.Sites))
	for name := range c.Sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AppNames returns declared app names in sorted order.
func (c *Config) AppNames() []string {
	names := make([]string, 0, len(c.Apps))
	for name := range c.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DataSites returns the sites holding the data role, sorted.
func (c *Config) DataSites() []string {
	var out []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].Has(RoleData) {
			out = append(out, name)
		}
	}
	return out
}

// AppsSites returns the sites holding the apps role, sorted.
func (c *Config) AppsSites() []string {
	var out []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].Has(RoleApps) {
			out = append(out, name)
		}
	}
	return out
}

// PinnedTo returns the apps pinned to a site, sorted.
func (c *Config) PinnedTo(site string) []string {
	var out []string
	for _, name := range c.AppNames() {
		if p := c.Apps[name].Placement; p.Mode == PlacementPinned && p.Site == site {
			out = append(out, name)
		}
	}
	return out
}

// CaddySites returns the sites that run the toolkit's Caddy, sorted: every
// gateway, and every monitor serving its own apps.
func (c *Config) CaddySites() []string {
	var out []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].RunsCaddy() {
			out = append(out, name)
		}
	}
	return out
}

// MonitorSites returns the sites holding the monitor role, sorted.
func (c *Config) MonitorSites() []string {
	var out []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].Has(RoleMonitor) {
			out = append(out, name)
		}
	}
	return out
}

// GatewaySites returns the sites holding the gateway role, sorted.
func (c *Config) GatewaySites() []string {
	var out []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].Has(RoleGateway) {
			out = append(out, name)
		}
	}
	return out
}

// structural checks the things that are wrong regardless of policy: missing
// required values, unknown enum members, malformed addresses. Every message
// names the key and says what to do about it.
func (c *Config) structural(noSubnet bool) error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Version != 1 {
		add("version: expected 1, got %d. This toolkit reads version 1 files.", c.Version)
	}
	switch {
	case c.ID == "":
		add("id: required. It is this deployment's identity, a random UUID every name and path on a host is derived from. Run `paisans init`, which adds one and never changes it afterwards.")
	case !deployment.ValidID(c.ID):
		add("id: %q is not a lowercase version 4 UUID in canonical form (8-4-4-4-12 hex digits). It is written once by `paisans init`; remove the line and run `paisans init` to generate one, unless this deployment already runs somewhere, in which case restore the id it was deployed with.", c.ID)
	}
	if c.Community.Domain == "" {
		add("community.domain: required. It is the community's federation identity and cannot be changed later.")
	}
	switch {
	case c.Mesh.Subnet == "" && noSubnet:
	case c.Mesh.Subnet == "":
		add("mesh.subnet: required. Run `paisans init`, which reaches every site, picks a /24 that overlaps nothing on any of them, and writes it here with each site's address moved into it. It is what applications trust for forwarded client addresses, so once a site is deployed it never changes.")
	default:
		ip, network, err := net.ParseCIDR(c.Mesh.Subnet)
		switch {
		case err != nil:
			add("mesh.subnet: %q is not a network in CIDR notation. Write it as an address and a prefix length, for example 10.44.0.0/24.", c.Mesh.Subnet)
		case ip.To4() == nil:
			add("mesh.subnet: %q is IPv6. The mesh is IPv4 only for now.", c.Mesh.Subnet)
		case !ip.Equal(network.IP):
			add("mesh.subnet: %q is a host address inside a network, not the network itself. Write %s.", c.Mesh.Subnet, network.String())
		}
	}

	if len(c.Sites) == 0 {
		add("sites: required. Declare at least one site.")
	}
	for _, name := range c.SiteNames() {
		site := c.Sites[name]
		if len(site.Roles) == 0 && len(c.PinnedTo(name)) == 0 {
			add("sites.%s.roles: required. Give the site at least one of data, apps, gateway, witness, storage, monitor. A site may have none only when an app is pinned to it, because then it exists to host that app.", name)
		}
		for _, role := range site.Roles {
			if !knownRoles[role] {
				add("sites.%s.roles: unknown role %q. Valid roles are data, apps, gateway, witness, storage, monitor.", name, role)
			}
		}
		if in := site.Ingress; in != nil {
			if in.Mode != "" && !knownIngressModes[in.Mode] {
				add("sites.%s.ingress.mode: unknown mode %q. Valid modes are paisans, where the toolkit's Caddy serves the site's apps, and external, where your own web server does.", name, in.Mode)
			}
			if in.Listen != "" {
				if _, _, ok := in.ListenHostPort(); !ok {
					add("sites.%s.ingress.listen: %q is not an IPv4 address and a port. Write it as the address your web server proxies to, for example 127.0.0.1:8480.", name, in.Listen)
				}
			}
		}
		if site.Address == "" {
			add("sites.%s.address: required. Every site needs a mesh address, and it never changes.", name)
		} else if !isIPv4(site.Address) {
			add("sites.%s.address: %q is not an IPv4 address. Use the site's WireGuard address, for example 10.44.0.1.", name, site.Address)
		}
		if site.Endpoint != "" {
			if _, err := ParseEndpointPort(site.Endpoint); err != nil {
				add("sites.%s.endpoint: %q %v. Write it as a host and a port, for example vm.example.org:51820: the port is also the one this site's WireGuard listens on.", name, site.Endpoint, err)
			}
		}
		if site.Watchdog != "" && !knownWatchdogModes[site.Watchdog] {
			add("sites.%s.watchdog: unknown mode %q. Valid modes are auto, required, softdog and off.", name, site.Watchdog)
		}
		problems = append(problems, sshProblems(name, site)...)
	}
	for _, name := range c.AppNames() {
		app := c.Apps[name]
		if app.Kind == "" {
			add("apps.%s.kind: required. Say which application this is, for example mbin or outline.", name)
		} else if !knownKinds[app.Kind] {
			add("apps.%s.kind: unknown kind %q. This toolkit renders element, mbin, oauth2-proxy, outline, pocket-id, synapse, uptime and writefreely.", name, app.Kind)
		}
		if app.Hostname == "" {
			add("apps.%s.hostname: required. It is the public name the gateway routes to.", name)
		}
		for _, service := range sortedKeys(app.Images) {
			if strings.TrimSpace(app.Images[service]) == "" {
				add("apps.%s.images.%s: empty. Give a full image reference, or remove the key to take the default this kind ships.", name, service)
			}
		}
		for _, role := range sortedKeys(app.Hostnames) {
			if role == "primary" {
				add("apps.%s.hostnames.primary: use the `hostname` key for the primary name. A role here names an additional hostname, and two places to write the same name is one place to get it wrong.", name)
			}
			if strings.TrimSpace(app.Hostnames[role]) == "" {
				add("apps.%s.hostnames.%s: empty. Give a hostname, or remove the role.", name, role)
			}
		}
		if app.SMTP != nil {
			problems = append(problems, smtpProblems("apps."+name+".smtp", *app.SMTP)...)
		}
		switch app.VisibilityGate {
		case "", GatePublic, GateMember, GateProvisional:
		default:
			add("apps.%s.visibility_gate: unknown value %q. Valid values are public, member and provisional.", name, app.VisibilityGate)
		}
	}
	problems = append(problems, smtpProblems("smtp", c.SMTP)...)
	if !c.Etcd.MembersDerived && len(c.Etcd.Members) == 0 {
		add("etcd.members: empty. No site would run etcd, so Patroni on every data site would have nowhere to keep its leader. Remove the key, and the voters are derived from the roles (every witness, then every data site), or list at least one site.")
	}
	if !c.Cluster.SitesDerived && len(c.Cluster.Sites) == 0 {
		add("cluster.sites: empty. Remove the key, and the cluster is derived from the roles (every data site), or list the data sites.")
	}
	if c.ACME.Provider == "" && len(c.CaddySites()) > 0 {
		// The providers are deliberately not listed here. This package does not
		// import internal/acme, by design, so any list written out would be a
		// second copy in prose that nothing keeps in step with the catalogue.
		// `validate` names them, built from acme.Providers.
		add("acme.provider: required, because %s runs the toolkit's Caddy (a gateway, or a monitor in ingress mode paisans). Certificates are issued over DNS-01, so the provider that answers the challenge has to be named. It must be one this toolkit publishes an image for, which `paisans validate` will list, or any other provider together with an acme.image carrying its module.", strings.Join(c.CaddySites(), ", "))
	}
	if len(problems) == 0 {
		return nil
	}
	return &LoadError{Path: c.Path, Problems: problems}
}

// sortedKeys returns a map's keys in sorted order, so that a file with several
// problems reports them in the same order every run.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadError carries every structural problem found in one file, because
// fixing them one run at a time is miserable.
type LoadError struct {
	Path     string
	Problems []string
}

func (e *LoadError) Error() string {
	out := fmt.Sprintf("%s is not a usable configuration:", e.Path)
	for _, p := range e.Problems {
		out += "\n  " + p
	}
	return out
}

// CapacityFor is the capacity a Garage site advertises: its entry in
// Capacities, or the deployment's Capacity.
func (g Garage) CapacityFor(site string) string {
	if c := strings.TrimSpace(g.Capacities[site]); c != "" {
		return c
	}
	return g.Capacity
}

// sizeShape is a size as Garage's -c flag takes it: a number, then a unit.
var sizeShape = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGTP]?)(I?)(B?)$`)

// ParseSize reads a size such as 100G, 3GB, 3GiB or 1.5T into bytes. K, M, G,
// T and P are decimal and Ki, Mi, Gi, Ti and Pi binary, with or without a
// trailing B, matching what dxflrs/garage:v1.0.1 accepts for a capacity and
// how it displays one (observed: 3G and 3GB show as 3.0 GB, 3GiB as 3.2 GB).
// A bare number is refused: Garage requires the unit, and so does this.
func ParseSize(s string) (int64, error) {
	m := sizeShape.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil || m[2] == "" {
		return 0, fmt.Errorf("%q is not a size with a unit, Eg: 100G, 3GiB or 1.5T", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", s)
	}
	base := 1000.0
	if m[3] == "I" {
		base = 1024
	}
	power := strings.Index("KMGTP", m[2]) + 1
	for i := 0; i < power; i++ {
		n *= base
	}
	return int64(n), nil
}
