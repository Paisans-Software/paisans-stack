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
	add := func(owner, proto, address string, port int) {
		out = append(out, Listener{Owner: owner, Proto: proto, Address: address, Port: port})
	}

	add("WireGuard", "udp", "", wireguardPort)
	if contains(cfg.Etcd.Members, site) {
		add("etcd client", "tcp", addr, etcdClientPort)
		add("etcd client", "tcp", loopback, etcdClientPort)
		add("etcd peer", "tcp", addr, etcdPeerPort)
	}
	if s.Has(config.RoleData) {
		add("Postgres", "tcp", addr, postgresPort)
		add("Postgres", "tcp", loopback, postgresPort)
		add("Patroni API", "tcp", addr, patroniAPIPort)
		add("bg_mon", "tcp", addr, bgMonPort)
	}
	if RunsHAProxy(cfg, site) {
		port := ClusterPort(cfg)
		add("HAProxy cluster port", "tcp", addr, port)
		add("HAProxy cluster port", "tcp", loopback, port)
		add("HAProxy stats", "tcp", loopback, haproxyStatsPort)
	}
	if contains(cfg.Storage.Garage.Sites, site) {
		add("Garage S3 API", "tcp", addr, garageS3Port)
		add("Garage RPC", "tcp", addr, garageRPCPort)
		add("Garage web endpoint", "tcp", addr, garageWebPort)
		add("Garage admin API", "tcp", addr, garageAdminPort)
	}
	if s.Has(config.RoleGateway) {
		add("Caddy", "tcp", "", caddyHTTPPort)
		add("Caddy", "tcp", "", caddyHTTPSPort)
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
			add(owner, "tcp", addr, port)
		}
		switch kind {
		case config.KindOAuth2Proxy:
			add(owner+" members instance", "tcp", addr, gateMembersPort)
		case config.KindSynapse:
			add(owner+" MAS", "tcp", addr, masPort)
		case config.KindPocketID:
			add(owner+" admin reconciler", "tcp", addr, reconcilerPort)
		}
	}
	return out
}
