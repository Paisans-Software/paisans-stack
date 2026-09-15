// Package config loads and structurally checks a paisans.yaml deployment
// declaration. It performs no policy checks: those live in internal/validate,
// so that every problem in a file can be reported at once rather than one per
// run.
package config

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
)

var knownRoles = map[Role]bool{RoleData: true, RoleApps: true, RoleGateway: true, RoleWitness: true}

// Kind is an application the toolkit knows how to render.
type Kind string

const (
	KindElement     Kind = "element"
	KindMbin        Kind = "mbin"
	KindOutline     Kind = "outline"
	KindPocketID    Kind = "pocket-id"
	KindSynapse     Kind = "synapse"
	KindWriteFreely Kind = "writefreely"
)

var knownKinds = map[Kind]bool{
	KindElement: true, KindMbin: true, KindOutline: true, KindPocketID: true, KindSynapse: true, KindWriteFreely: true,
}

// Config is a whole deployment, declared. It records intent and never status:
// which node is primary lives in etcd, not here.
type Config struct {
	Version   int       `yaml:"version"`
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
	Apps    map[string]App  `yaml:"apps"`

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
	SSH      string `yaml:"ssh"`
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
	Sites             []string `yaml:"sites"`
	Port              int      `yaml:"port"`
	PostgresVersion   string   `yaml:"postgres_version"`
	Synchronous       bool     `yaml:"synchronous"`
	SynchronousStrict bool     `yaml:"synchronous_strict"`
}

type Etcd struct {
	Members           []string `yaml:"members"`
	HeartbeatMS       int      `yaml:"heartbeat_ms"`
	ElectionTimeoutMS int      `yaml:"election_timeout_ms"`
}

type Storage struct {
	Garage Garage `yaml:"garage"`
}

type Garage struct {
	Sites       []string `yaml:"sites"`
	Replication int      `yaml:"replication"`
}

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
func Load(path string) (*Config, error) {
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
	if err := cfg.structural(); err != nil {
		return nil, err
	}
	return &cfg, nil
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
func (c *Config) structural() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Version != 1 {
		add("version: expected 1, got %d. This toolkit reads version 1 files.", c.Version)
	}
	if c.Community.Domain == "" {
		add("community.domain: required. It is the community's federation identity and cannot be changed later.")
	}
	switch {
	case c.Mesh.Subnet == "":
		add("mesh.subnet: required. Declare the private network every site sits on, for example 10.44.0.0/24. It is what applications trust for forwarded client addresses, so it is declared rather than guessed and it never changes.")
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
		if len(site.Roles) == 0 {
			add("sites.%s.roles: required. Give the site at least one of data, apps, gateway, witness.", name)
		}
		for _, role := range site.Roles {
			if !knownRoles[role] {
				add("sites.%s.roles: unknown role %q. Valid roles are data, apps, gateway, witness.", name, role)
			}
		}
		if site.Address == "" {
			add("sites.%s.address: required. Every site needs a mesh address, and it never changes.", name)
		} else if !isIPv4(site.Address) {
			add("sites.%s.address: %q is not an IPv4 address. Use the site's WireGuard address, for example 10.44.0.1.", name, site.Address)
		}
		if site.SSH == "" {
			add("sites.%s.ssh: required. It is the bootstrap route, used once before the mesh exists, so it must be an address you can already reach.", name)
		}
	}
	for _, name := range c.AppNames() {
		app := c.Apps[name]
		if app.Kind == "" {
			add("apps.%s.kind: required. Say which application this is, for example mbin or outline.", name)
		} else if !knownKinds[app.Kind] {
			add("apps.%s.kind: unknown kind %q. This toolkit renders element, mbin, outline, pocket-id, synapse and writefreely.", name, app.Kind)
		}
		if app.Hostname == "" {
			add("apps.%s.hostname: required. It is the public name the gateway routes to.", name)
		}
		for _, service := range sortedKeys(app.Images) {
			if strings.TrimSpace(app.Images[service]) == "" {
				add("apps.%s.images.%s: empty. Give a full image reference, or remove the key to take the default this kind ships.", name, service)
			}
		}
	}
	if len(c.GatewaySites()) > 0 && c.ACME.Provider == "" {
		// The providers are deliberately not listed here. This package does not
		// import internal/acme, by design, so any list written out would be a
		// second copy in prose that nothing keeps in step with the catalogue.
		// `validate` names them, built from acme.Providers.
		add("acme.provider: required, because a site holds the gateway role. Certificates are issued over DNS-01, so the provider that answers the challenge has to be named. It must be one this toolkit publishes an image for, which `paisans validate` will list, or any other provider together with an acme.image carrying its module.")
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
