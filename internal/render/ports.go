package render

import (
	"fmt"
	"sort"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The infrastructure's fixed ports. Each is what a rendered file says, and is
// named here so that SiteListeners and the templates read one number rather
// than two copies of it.
const (
	// wireguardPort is wg0's ListenPort, on every site.
	wireguardPort = 51820
	// etcdClientPort and etcdPeerPort are etcd's two listeners.
	etcdClientPort = 2379
	etcdPeerPort   = 2380
	// Garage's four listeners: the S3 API, RPC between nodes, the web
	// endpoint and the admin API.
	garageS3Port    = 3900
	garageRPCPort   = 3901
	garageWebPort   = 3902
	garageAdminPort = 3903
	// caddyHTTPPort and caddyHTTPSPort are where the gateway's Caddy serves,
	// on every interface: it runs with host networking and binds Caddy's
	// defaults, which no rendered file overrides.
	caddyHTTPPort  = 80
	caddyHTTPSPort = 443
)

// WireGuardPort is wg0's listen port, for a command that checks it is free.
const WireGuardPort = wireguardPort

// EtcdClientPort and EtcdPeerPort are an etcd member's two listeners.
const (
	EtcdClientPort = etcdClientPort
	EtcdPeerPort   = etcdPeerPort
)

// Listener is one address and port something on a site binds.
type Listener struct {
	// Owner names what binds it, as an operator would recognise it: an
	// infrastructure service, or an app by name and kind.
	Owner string
	// Key is the paisans.yaml key that makes the site bind it, for a
	// message that has to tell the operator what to change. A listener
	// added here carries its key, and the host check claims it with no
	// change of its own.
	Key string
	// Proto is "tcp" or "udp".
	Proto string
	// Address is the IP bound, or "" for every address on the host.
	Address string
	Port    int
}

func (l Listener) String() string {
	address := l.Address
	if address == "" {
		address = "*"
	}
	return fmt.Sprintf("%s:%d/%s", address, l.Port, l.Proto)
}

// Overlaps reports whether two listeners cannot both bind: the same protocol
// and port, on the same address or with either on every address. Linux
// refuses a specific address beside a wildcard one on the same port, and
// Docker's published port is a bind like any other.
func (l Listener) Overlaps(o Listener) bool {
	return l.Proto == o.Proto && l.Port == o.Port &&
		(l.Address == o.Address || l.Address == "" || o.Address == "")
}

// SiteListeners is everything the rendered stacks bind on one site: the
// infrastructure its roles run and the ports every app placed there
// publishes, read from the same conditions and constants the renderer uses.
// The order is stable: infrastructure first, then apps by name.
func SiteListeners(cfg *config.Config, site string) []Listener {
	s, ok := cfg.Sites[site]
	if !ok {
		return nil
	}
	addr := s.Address
	const loopback = "127.0.0.1"
	var out []Listener
	add := func(owner, key, proto, address string, port int) {
		out = append(out, Listener{Owner: owner, Key: key, Proto: proto, Address: address, Port: port})
	}
	roles := func(role config.Role) string { return fmt.Sprintf("sites.%s.roles (%s)", site, role) }

	add("WireGuard", "mesh", "udp", "", wireguardPort)
	if contains(cfg.Etcd.Members, site) {
		add("etcd client", "etcd.members", "tcp", addr, etcdClientPort)
		add("etcd client", "etcd.members", "tcp", loopback, etcdClientPort)
		add("etcd peer", "etcd.members", "tcp", addr, etcdPeerPort)
	}
	if s.Has(config.RoleData) {
		add("Postgres", roles(config.RoleData), "tcp", addr, postgresPort)
		add("Postgres", roles(config.RoleData), "tcp", loopback, postgresPort)
		add("Patroni API", roles(config.RoleData), "tcp", addr, patroniAPIPort)
		add("bg_mon", roles(config.RoleData), "tcp", addr, bgMonPort)
	}
	if RunsHAProxy(cfg, site) {
		// HAProxy runs on an apps site whenever an app is clustered. The
		// cluster port is the number cluster.port sets; the stats port is
		// fixed, so what claims it is the role that runs HAProxy at all.
		port := ClusterPort(cfg)
		add("HAProxy cluster port", "cluster.port", "tcp", addr, port)
		add("HAProxy cluster port", "cluster.port", "tcp", loopback, port)
		add("HAProxy stats", roles(config.RoleApps), "tcp", loopback, haproxyStatsPort)
	}
	if contains(cfg.Storage.Garage.Sites, site) {
		add("Garage S3 API", "storage.garage.sites", "tcp", addr, garageS3Port)
		add("Garage RPC", "storage.garage.sites", "tcp", addr, garageRPCPort)
		add("Garage web endpoint", "storage.garage.sites", "tcp", addr, garageWebPort)
		add("Garage admin API", "storage.garage.sites", "tcp", addr, garageAdminPort)
	}
	if s.Has(config.RoleGateway) {
		add("Caddy", roles(config.RoleGateway), "tcp", "", caddyHTTPPort)
		add("Caddy", roles(config.RoleGateway), "tcp", "", caddyHTTPSPort)
	}

	var apps []string
	for name, sites := range AppSites(cfg) {
		if contains(sites, site) {
			apps = append(apps, name)
		}
	}
	sort.Strings(apps)
	for _, name := range apps {
		kind := cfg.Apps[name].Kind
		owner := fmt.Sprintf("app %s (%s)", name, kind)
		if port, ok := appPort[kind]; ok {
			add(owner, "apps."+name, "tcp", addr, port)
		}
		switch kind {
		case config.KindOAuth2Proxy:
			add(owner+" members instance", "apps."+name, "tcp", addr, gateMembersPort)
		case config.KindSynapse:
			add(owner+" MAS", "apps."+name, "tcp", addr, masPort)
		}
	}
	return out
}
