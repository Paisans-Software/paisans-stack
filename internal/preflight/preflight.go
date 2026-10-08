// Package preflight is the first stage of `paisans site add`: read only checks
// on the site being added and on every site already running, so that a join
// that cannot work is refused before anything on a live host changes.
//
// Every check here only reads. A check that cannot be made (a host that does
// not answer, an output that cannot be parsed) is a refusal rather than a
// skip: the point of preflight is that every assumption the join rests on was
// looked at, and "could not look" is not "looked and it was fine".
package preflight

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/patroni"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Check is one finding. A check that is neither refused nor warned passed.
type Check struct {
	Site, Name, Detail string
	Refused, Warned    bool
}

// Report is every check, in the order they were made.
type Report struct {
	Checks []Check
}

// Refused reports whether any check refused. A warning does not: it is
// printed and the join goes ahead, because a risk the operator has been shown
// is theirs to take.
func (r Report) Refused() bool {
	for _, c := range r.Checks {
		if c.Refused {
			return true
		}
	}
	return false
}

// Print writes the report, one line per check.
func (r Report) Print(w io.Writer) {
	for _, c := range r.Checks {
		label := "ok"
		switch {
		case c.Refused:
			label = "REFUSED"
		case c.Warned:
			label = "WARNING"
		}
		fmt.Fprintf(w, "  %-8s %-8s %-10s %s\n", label, c.Site, c.Name, c.Detail)
	}
}

// diskHeadroom is what the new site must have free beyond the leader's
// databases: room for WAL that arrives while pg_basebackup copies, and for
// the infrastructure images. The number is the spec's.
const diskHeadroom = 2 << 30

// buildHostPrep is hostprep.Build, replaceable in tests: a profile's probes
// are tested in hostprep, and repeating them here would test them twice.
var buildHostPrep = hostprep.Build

// Run makes every check. transports holds one transport per site, keyed by
// site name; a site without one is refused, since it cannot be read.
//
// The error is for a call that makes no sense (an undeclared site). Anything
// a host says is a Check, so the operator sees every problem at once rather
// than the first.
func Run(cfg *config.Config, newSite string, transports map[string]apply.Transport) (Report, error) {
	site, ok := cfg.Sites[newSite]
	if !ok {
		return Report{}, fmt.Errorf("preflight: no site %q is declared. Declared sites are %s", newSite, strings.Join(cfg.SiteNames(), ", "))
	}
	r := &runner{cfg: cfg, newSite: newSite, site: site, transports: transports, reachable: map[string]bool{}}

	// SSH first, everywhere, because every other check needs it.
	for _, name := range cfg.SiteNames() {
		r.ssh(name)
	}
	for _, name := range cfg.SiteNames() {
		r.clock(name)
	}
	if r.reachable[newSite] {
		t := transports[newSite]
		r.hostPrepare(t)
		r.osMatch(t)
		r.wireguard(t)
		r.watchdog(t)
		r.ports(t)
		r.routes(t)
		r.disk(t)
		r.storage(t)
		r.rtt(t)
	}
	return Report{Checks: r.checks}, nil
}

type runner struct {
	cfg        *config.Config
	newSite    string
	site       config.Site
	transports map[string]apply.Transport
	reachable  map[string]bool
	checks     []Check
}

func (r *runner) pass(site, name, format string, args ...any) {
	r.checks = append(r.checks, Check{Site: site, Name: name, Detail: fmt.Sprintf(format, args...)})
}

func (r *runner) refuse(site, name, format string, args ...any) {
	r.checks = append(r.checks, Check{Site: site, Name: name, Detail: fmt.Sprintf(format, args...), Refused: true})
}

func (r *runner) warn(site, name, format string, args ...any) {
	r.checks = append(r.checks, Check{Site: site, Name: name, Detail: fmt.Sprintf(format, args...), Warned: true})
}

// existing is every declared site other than the one being added.
func (r *runner) existing() []string {
	var out []string
	for _, name := range r.cfg.SiteNames() {
		if name != r.newSite {
			out = append(out, name)
		}
	}
	return out
}

// ssh checks the site answers and that sudo can be used.
//
// One command does both: `sudo -n` fails rather than prompting, and its
// failure says "sudo:", which is how a refused sudo is told apart from a host
// that never answered. A transport that already runs everything through sudo
// wraps this in another sudo, which runs it as root, where it always passes;
// the outer sudo is then the one tested, and a host where it cannot be used
// (a password and no terminal to ask on, a password it refused) is reported
// with apply.ErrSudo.
func (r *runner) ssh(name string) {
	t, ok := r.transports[name]
	if !ok || t == nil {
		r.refuse(name, "ssh", "no way to reach this site was given, so nothing on it could be checked")
		return
	}
	out, err := t.Run("sudo -n true")
	switch {
	case err == nil:
		r.reachable[name] = true
		r.pass(name, "ssh", "%s answers, and sudo works", t.Describe())
	case errors.Is(err, apply.ErrSudo):
		r.refuse(name, "ssh", "%s", firstLine(err.Error()))
	case strings.Contains(out, "sudo:"):
		r.refuse(name, "ssh", "%s answers, but sudo asks for a password: %s. Every command here runs through sudo; it asks for the password on a terminal, and an unattended run needs the login user given passwordless sudo", t.Describe(), firstLine(out))
	default:
		r.refuse(name, "ssh", "%s does not answer: %s", t.Describe(), firstLine(errText(out, err)))
	}
}

// clock checks systemd's view of time synchronisation. etcd's leases and
// Patroni's leader key both expire on time, so a site with a drifting clock
// can lose or keep the leader key on a schedule nobody else agrees with.
func (r *runner) clock(name string) {
	if !r.reachable[name] {
		return
	}
	out, err := r.transports[name].Run("timedatectl show -p NTPSynchronized --value")
	switch v := strings.TrimSpace(out); {
	case err != nil:
		r.refuse(name, "clock", "could not ask timedatectl: %s", firstLine(errText(out, err)))
	case v == "yes":
		r.pass(name, "clock", "synchronised")
	default:
		r.refuse(name, "clock", "not synchronised (NTPSynchronized=%s). Enable a time service, Eg: `timedatectl set-ntp true`, and wait for it to sync", v)
	}
}

// hostPrepare requires that `host prepare` would do nothing. Every check
// below assumes a prepared host (WireGuard tools, Docker, the firewall), and
// reusing the plan rather than re-checking each part keeps one definition of
// "prepared".
func (r *runner) hostPrepare(t apply.Transport) {
	plan, err := buildHostPrep(r.newSite, r.cfg, t)
	if err != nil {
		r.refuse(r.newSite, "prepared", "host prepare could not plan: %v", err)
		return
	}
	if len(plan.Steps) == 0 {
		r.pass(r.newSite, "prepared", "host prepare has nothing left to do (%s)", plan.Profile)
		return
	}
	var steps []string
	for _, s := range plan.Steps {
		steps = append(steps, s.Describe)
	}
	r.refuse(r.newSite, "prepared", "host prepare still has %d step(s): %s. Run `paisans host prepare --site %s --execute` first", len(steps), strings.Join(steps, "; "), r.newSite)
}

// platform is what osMatch compares.
type platform struct {
	id, version, glibc string
}

func (p platform) String() string { return fmt.Sprintf("%s %s, glibc %s", p.id, p.version, p.glibc) }

// readPlatform reads /etc/os-release and the version `ldd --version` prints
// at the end of its first line ("ldd (Ubuntu GLIBC 2.39-0ubuntu8.3) 2.39").
func readPlatform(t apply.Transport) (platform, error) {
	content, found, err := t.ReadFile("/etc/os-release")
	if err != nil {
		return platform{}, err
	}
	if !found {
		return platform{}, fmt.Errorf("no /etc/os-release")
	}
	host := hostprep.ParseOSRelease(content)
	out, err := t.Run("ldd --version")
	if err != nil {
		return platform{}, fmt.Errorf("ldd --version: %s", firstLine(errText(out, err)))
	}
	fields := strings.Fields(firstLine(strings.TrimSpace(out)))
	if len(fields) == 0 {
		return platform{}, fmt.Errorf("ldd --version printed nothing")
	}
	return platform{id: host.ID, version: host.VersionID, glibc: glibcRelease(fields[len(fields)-1])}, nil
}

// glibcRelease keeps the "2.N" of a glibc version. glibc has been 2.x since
// 1997 and numbers its releases by the second part, so the "major version"
// the collation concern is about is 2.N: collations change between 2.N
// releases (2.28 is the well known one), never within one.
func glibcRelease(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// osMatch compares the new data site's platform with every existing data
// site. A replica's indexes are the primary's bytes, sorted by the primary's
// collation library; a replica whose glibc sorts differently reads them as
// corrupt once it is promoted.
func (r *runner) osMatch(t apply.Transport) {
	if !r.site.Has(config.RoleData) {
		return
	}
	mine, err := readPlatform(t)
	if err != nil {
		r.refuse(r.newSite, "platform", "could not read: %v", err)
		return
	}
	compared := false
	for _, name := range r.existing() {
		if !r.cfg.Sites[name].Has(config.RoleData) {
			continue
		}
		compared = true
		if !r.reachable[name] {
			r.refuse(r.newSite, "platform", "%s, not compared with %s, which could not be reached", mine, name)
			continue
		}
		theirs, err := readPlatform(r.transports[name])
		switch {
		case err != nil:
			r.refuse(r.newSite, "platform", "could not read %s's: %v", name, err)
		case mine != theirs:
			r.refuse(r.newSite, "platform", "%s, but %s runs %s. A replica must match its primary's OS and glibc release, or indexes built under one collation are read under another", mine, name, theirs)
		default:
			r.pass(r.newSite, "platform", "%s, same as %s", mine, name)
		}
	}
	if !compared {
		r.pass(r.newSite, "platform", "%s, and no other data site to compare with", mine)
	}
}

// wireguard checks the kernel has WireGuard: loaded already, or loadable.
// `modprobe -n` resolves the module without inserting it.
func (r *runner) wireguard(t apply.Transport) {
	out, err := t.Run("test -d /sys/module/wireguard || modprobe -n wireguard")
	if err != nil {
		r.refuse(r.newSite, "wireguard", "the kernel has no WireGuard module: %s", firstLine(errText(out, err)))
		return
	}
	r.pass(r.newSite, "wireguard", "kernel module available")
}

// watchdog checks /dev/watchdog on a data site whose watchdog mode wants one.
// Patroni renders `watchdog: required` for every mode but off, and refuses to
// take the leader key without the device.
func (r *runner) watchdog(t apply.Transport) {
	if !r.site.Has(config.RoleData) {
		return
	}
	mode := r.site.WatchdogMode()
	if mode == config.WatchdogOff {
		r.pass(r.newSite, "watchdog", "mode off, none needed")
		return
	}
	if out, err := t.Run("test -e /dev/watchdog"); err != nil {
		r.refuse(r.newSite, "watchdog", "mode %s, and there is no /dev/watchdog: %s. Run `paisans host prepare --site %s`", mode, firstLine(errText(out, "absent")), r.newSite)
		return
	}
	r.pass(r.newSite, "watchdog", "mode %s, /dev/watchdog present", mode)
}

// port is one listener the new site will need.
type port struct {
	proto  string
	number int
	what   string
}

// wanted is every port the site will bind, by role.
func (r *runner) wanted() []port {
	ports := []port{{"udp", render.WireGuardPort, "WireGuard"}}
	for _, m := range r.cfg.Etcd.Members {
		if m == r.newSite {
			ports = append(ports, port{"tcp", render.EtcdClientPort, "etcd client"}, port{"tcp", render.EtcdPeerPort, "etcd peer"})
		}
	}
	if r.site.Has(config.RoleData) {
		ports = append(ports,
			port{"tcp", render.PostgresPort, "Postgres"},
			port{"tcp", render.PatroniAPIPort, "Patroni API"},
			port{"tcp", render.BgMonPort, "bg_mon"})
	}
	if render.RunsHAProxy(r.cfg, r.newSite) {
		ports = append(ports, port{"tcp", render.ClusterPort(r.cfg), "HAProxy cluster port"})
	}
	return ports
}

// listening parses `ss -Hltnu`: Netid, State, Recv-Q, Send-Q, Local
// Address:Port, Peer Address:Port. The port is after the last colon, which
// holds for "*:22", "0.0.0.0:22", "[::]:22" and "127.0.0.53%lo:53".
func listening(out string) map[string]bool {
	in := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		local := f[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		in[f[0]+"/"+local[i+1:]] = true
	}
	return in
}

// ports checks nothing already listens where the site's services will.
func (r *runner) ports(t apply.Transport) {
	out, err := t.Run("ss -Hltnu")
	if err != nil {
		r.refuse(r.newSite, "ports", "could not list listeners: %s", firstLine(errText(out, err)))
		return
	}
	in := listening(out)
	var taken, free []string
	for _, p := range r.wanted() {
		name := fmt.Sprintf("%d/%s", p.number, p.proto)
		if in[p.proto+"/"+strconv.Itoa(p.number)] {
			taken = append(taken, fmt.Sprintf("%s (%s)", name, p.what))
		} else {
			free = append(free, name)
		}
	}
	if len(taken) > 0 {
		r.refuse(r.newSite, "ports", "already in use: %s. Find the owner with `sudo ss -ltnup`", strings.Join(taken, ", "))
		return
	}
	r.pass(r.newSite, "ports", "free: %s", strings.Join(free, ", "))
}

// routes checks the mesh subnet overlaps no route the host already has: a
// Docker bridge, a LAN or another VPN on the same range would capture mesh
// traffic, or have its own captured. A route through wg0 is the mesh itself
// and is not a collision.
func (r *runner) routes(t apply.Transport) {
	_, mesh, err := net.ParseCIDR(r.cfg.Mesh.Subnet)
	if err != nil {
		r.refuse(r.newSite, "routes", "mesh.subnet %q is not a network", r.cfg.Mesh.Subnet)
		return
	}
	out, err := t.Run("ip -j route")
	if err != nil {
		r.refuse(r.newSite, "routes", "could not list routes: %s", firstLine(errText(out, err)))
		return
	}
	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &routes); err != nil {
		r.refuse(r.newSite, "routes", "unreadable `ip -j route` output: %v", err)
		return
	}
	var clash []string
	for _, route := range routes {
		if route.Dst == "default" || route.Dev == "wg0" {
			continue
		}
		dst := route.Dst
		if !strings.Contains(dst, "/") {
			dst += "/32"
		}
		_, network, err := net.ParseCIDR(dst)
		if err != nil {
			continue
		}
		if network.Contains(mesh.IP) || mesh.Contains(network.IP) {
			clash = append(clash, fmt.Sprintf("%s dev %s", route.Dst, route.Dev))
		}
	}
	if len(clash) > 0 {
		r.refuse(r.newSite, "routes", "mesh subnet %s overlaps %s", r.cfg.Mesh.Subnet, strings.Join(clash, ", "))
		return
	}
	r.pass(r.newSite, "routes", "no route overlaps the mesh subnet %s", r.cfg.Mesh.Subnet)
}

// diskProbe reads free bytes on the deepest directory that exists on the way
// to Spilo's data directory, which apply bind mounts from /srv/infra/postgres.
// On a blank host none of it exists yet, and / is where it will be made.
const diskProbe = `for d in /srv/infra/postgres /srv/infra /srv /; do if [ -d "$d" ]; then df -B1 --output=avail "$d"; exit; fi; done`

// disk checks the new data site has room for a copy of the leader's
// databases. pg_basebackup copies all of them, and running out partway leaves
// a replica that will not start and a disk that is full.
func (r *runner) disk(t apply.Transport) {
	if !r.site.Has(config.RoleData) {
		return
	}
	size, from, err := r.leaderSize()
	if err != nil {
		r.refuse(r.newSite, "disk", "could not read the leader's database size: %v", err)
		return
	}
	out, err := t.Run(diskProbe)
	if err != nil {
		r.refuse(r.newSite, "disk", "could not read free space: %s", firstLine(errText(out, err)))
		return
	}
	fields := strings.Fields(out)
	var free int64 = -1
	if len(fields) > 0 {
		free, err = strconv.ParseInt(fields[len(fields)-1], 10, 64)
	}
	if err != nil || free < 0 {
		r.refuse(r.newSite, "disk", "unreadable `df` output %q", strings.TrimSpace(out))
		return
	}
	need := size + diskHeadroom
	if free < need {
		r.refuse(r.newSite, "disk", "%s free, and a replica of %s needs %s (its databases, %s, plus %s)",
			apply.FormatSize(free), from, apply.FormatSize(need), apply.FormatSize(size), apply.FormatSize(diskHeadroom))
		return
	}
	r.pass(r.newSite, "disk", "%s free, %s needed (%s's databases, %s, plus %s)",
		apply.FormatSize(free), apply.FormatSize(need), from, apply.FormatSize(size), apply.FormatSize(diskHeadroom))
}

// networkFilesystems are filesystem types that live on another machine.
// Postgres and etcd both make durability promises on fsync: a commit is
// acknowledged, and an etcd write is agreed, once the bytes are on stable
// storage. Over NFS, SMB, GlusterFS, CephFS or a FUSE mount of a remote, what
// fsync means depends on the server, the mount options and the cache, and a
// network blip stalls fsync for as long as it lasts, which is a missed etcd
// heartbeat and a failover for nothing. A Docker root there also holds every
// container's writable layer. Ceph's RBD is a block device and reads as the
// local filesystem on top of it, so it is not listed and not caught.
var networkFilesystems = map[string]bool{
	"nfs": true, "nfs4": true,
	"cifs": true, "smb3": true, "smbfs": true,
	"glusterfs": true, "fuse.glusterfs": true,
	"ceph": true, "fuse.ceph": true, "fuse.cephfs": true,
	"fuse.sshfs": true, "9p": true, "afs": true, "lustre": true,
	"beegfs": true, "gpfs": true, "fuse.s3fs": true, "fuse.rclone": true,
}

// localFilesystems are the block filesystems a host's own disk carries.
var localFilesystems = map[string]bool{
	"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true,
}

// dockerRootProbe reads Docker's root directory from the daemon, since an
// operator may have moved it from /var/lib/docker.
const dockerRootProbe = `docker info --format '{{.DockerRootDir}}'`

// fsProbe names the filesystem holding path, or the deepest directory on the
// way to it that exists: on a blank host /srv may not exist yet, and its
// parent is where it will be made.
func fsProbe(path string) string {
	return fmt.Sprintf(`d=%s; while [ ! -d "$d" ]; do d=$(dirname "$d"); done; findmnt -no FSTYPE,SOURCE --target "$d"`, quote(path))
}

// storage checks the new site keeps its state on a local filesystem: Docker's
// root, which holds every container's writable layer, and /srv, where every
// stack bind mounts its data. A
// network filesystem is refused; a type in neither list is warned about,
// since it is not known to be wrong and refusing it would stop a join over a
// filesystem this list has not heard of.
func (r *runner) storage(t apply.Transport) {
	out, err := t.Run(dockerRootProbe)
	root := strings.TrimSpace(out)
	if err != nil || root == "" || !strings.HasPrefix(root, "/") {
		r.refuse(r.newSite, "storage", "could not read Docker's root directory: %s", firstLine(errText(out, err)))
		return
	}
	var found []string
	var unknown []string
	for _, path := range []string{root, "/srv"} {
		out, err := t.Run(fsProbe(path))
		fields := strings.Fields(out)
		if err != nil || len(fields) < 1 {
			r.refuse(r.newSite, "storage", "could not read the filesystem under %s: %s", path, firstLine(errText(out, err)))
			return
		}
		fstype, source := fields[0], ""
		if len(fields) > 1 {
			source = fields[1]
		}
		detail := fmt.Sprintf("%s on %s (%s)", path, fstype, source)
		switch {
		case networkFilesystems[fstype]:
			r.refuse(r.newSite, "storage", "%s is network attached. Postgres and etcd acknowledge a write once fsync returns, and over a network filesystem that promise depends on the server and stalls with the network. Put %s on a local disk", detail, path)
			return
		case !localFilesystems[fstype]:
			unknown = append(unknown, detail)
		}
		found = append(found, detail)
	}
	if len(unknown) > 0 {
		r.warn(r.newSite, "storage", "%s: not a filesystem known to be local or network attached. Confirm it is a local disk", strings.Join(unknown, ", "))
		return
	}
	r.pass(r.newSite, "storage", "local: %s", strings.Join(found, ", "))
}

// leaderSize finds the leader through any existing cluster member's /cluster
// and asks it, through its own container, how big its databases are.
func (r *runner) leaderSize() (int64, string, error) {
	var asked []string
	for _, name := range r.cfg.Cluster.Sites {
		if name == r.newSite || !r.reachable[name] {
			continue
		}
		api := fmt.Sprintf("%s:%d", r.cfg.Sites[name].Address, render.PatroniAPIPort)
		out, err := r.transports[name].Run(patroni.ClusterCommand(api))
		if err != nil {
			asked = append(asked, fmt.Sprintf("%s: %s", name, firstLine(errText(out, err))))
			continue
		}
		cluster, err := patroni.Parse(out)
		if err != nil {
			asked = append(asked, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		leader, ok := cluster.Leader()
		if !ok {
			return 0, "", fmt.Errorf("%s's Patroni reports no running leader", name)
		}
		t, ok := r.transports[leader.Name]
		if !ok || !r.reachable[leader.Name] {
			return 0, "", fmt.Errorf("the leader is %s, which could not be reached", leader.Name)
		}
		out, err = t.Run(patroni.DatabaseSizeCommand)
		if err != nil {
			return 0, "", fmt.Errorf("%s: %s", leader.Name, firstLine(errText(out, err)))
		}
		size, err := patroni.ParseSize(out)
		return size, leader.Name, err
	}
	if len(asked) == 0 {
		return 0, "", fmt.Errorf("no existing cluster site could be reached")
	}
	return 0, "", fmt.Errorf("no existing cluster site answered: %s", strings.Join(asked, "; "))
}

// rttProbe times three TCP connects from the new site to host:port, printing
// microseconds per line. It is bash's /dev/tcp and $EPOCHREALTIME, both in
// bash 5.0 and later (Ubuntu 24.04 ships 5.2), timed inside one shell so no
// process start is counted. timeout bounds a connect the network drops, which
// would otherwise wait out the kernel's SYN retries.
func rttProbe(host string, port int) string {
	script := `for i in 1 2 3; do s=$EPOCHREALTIME; exec 3<>/dev/tcp/$0/$1 || exit 1; e=$EPOCHREALTIME; exec 3<&-; echo $(( ${e//[.,]/} - ${s//[.,]/} )); done`
	return fmt.Sprintf("timeout 20 bash -c %s %s %d", quote(script), quote(host), port)
}

// median of the samples, in milliseconds.
func medianMS(out string) (float64, []string, error) {
	var samples []int64
	for _, f := range strings.Fields(out) {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("unreadable sample %q", f)
		}
		samples = append(samples, n)
	}
	if len(samples) == 0 {
		return 0, nil, fmt.Errorf("no samples")
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var shown []string
	for _, s := range samples {
		shown = append(shown, fmt.Sprintf("%.1f", float64(s)/1000))
	}
	return float64(samples[len(samples)/2]) / 1000, shown, nil
}

// rtt measures from the new site to each existing site's public address,
// over its ssh port because that is the one port every site is known to
// accept from anywhere. A TCP connect is one round trip (SYN, SYN ACK), and
// the median of three is reported so one slow sample does not decide.
//
// The thresholds. etcd's tuning guide (etcd v3.5 docs, "Tuning", Time
// parameters) recommends a heartbeat "around the maximum of average
// round-trip time (RTT) between members, normally around 0.5-1.5x", and says
// election timeouts "must be at least 10 times the round-trip time". The spec
// sets the refusal at five times: below that, an ordinary bad minute on the
// path is an election. Between five and ten times is what etcd's own guide
// calls too low, so it warns; a heartbeat under one round trip warns too.
func (r *runner) rtt(t apply.Transport) {
	heartbeat := float64(render.HeartbeatMS(r.cfg))
	election := float64(render.ElectionTimeoutMS(r.cfg))
	for _, name := range r.existing() {
		s := r.cfg.Sites[name]
		if s.PublicAddress == "" {
			r.refuse(r.newSite, "rtt", "to %s: it declares no public_address, so the round trip cannot be measured. Every site needs one for a join", name)
			continue
		}
		port := s.SSH.PortOrDefault()
		out, err := t.Run(rttProbe(s.PublicAddress, port))
		if err != nil {
			r.refuse(r.newSite, "rtt", "to %s (%s:%d): no TCP connection: %s", name, s.PublicAddress, port, firstLine(errText(out, err)))
			continue
		}
		ms, samples, err := medianMS(out)
		if err != nil {
			r.refuse(r.newSite, "rtt", "to %s: %v", name, err)
			continue
		}
		detail := fmt.Sprintf("to %s %.1f ms (samples %s ms); etcd heartbeat %.0f ms, election timeout %.0f ms", name, ms, strings.Join(samples, ", "), heartbeat, election)
		switch {
		case election < 5*ms:
			r.refuse(r.newSite, "rtt", "%s. The election timeout is under five times the round trip; raise etcd.election_timeout_ms to at least %.0f", detail, 5*ms)
		case election < 10*ms:
			r.warn(r.newSite, "rtt", "%s. etcd's tuning guide asks for an election timeout of at least ten round trips; consider raising etcd.election_timeout_ms to %.0f", detail, 10*ms)
		case heartbeat < ms:
			r.warn(r.newSite, "rtt", "%s. The heartbeat is under one round trip; consider raising etcd.heartbeat_ms to at least %.0f", detail, ms)
		default:
			r.pass(r.newSite, "rtt", "%s", detail)
		}
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// errText is what a failed command said, or the error when it said nothing.
func errText(out string, err any) string {
	if strings.TrimSpace(out) != "" {
		return out
	}
	return fmt.Sprint(err)
}
