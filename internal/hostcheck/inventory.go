package hostcheck

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Transport is how the inventory reads a host. apply.SSHTransport and the
// other packages' transports satisfy it. It has no way to write, because the
// inventory never changes anything.
type Transport interface {
	// Run executes a command and returns its combined output. A non zero
	// exit is an error.
	Run(command string) (string, error)
	// ReadFile returns a file's contents, a missing file reported through
	// found.
	ReadFile(path string) (content string, found bool, err error)
	// Describe names the destination, for messages.
	Describe() string
}

// Inventory is what one host already runs.
type Inventory struct {
	Host       string
	Docker     Docker
	Containers []Container
	Volumes    []Volume
	Networks   []Network
	Sockets    []Socket
	// Cgroups maps a listening process's PID to the container its
	// /proc/<pid>/cgroup names, for the PIDs that are in one.
	Cgroups  map[int]string
	Links    []string
	Routes   []Route
	Firewall Firewall
	// Manifest is whether this deployment's record of what it wrote exists
	// at ManifestPath, and ManifestWireGuard whether that record lists
	// its WireGuard config, which is how a mesh interface the deployment
	// wrote is told from one it did not.
	ManifestPath      string
	Manifest          bool
	ManifestWireGuard bool
	// ManifestFiles is every file that record lists, nil when there is
	// none or it is unreadable.
	ManifestFiles []render.ManifestFile
	// HostSites are the *.caddy files in the host's own render.HostSitesDir,
	// by full path and sorted: site blocks somebody added to the gateway's
	// Caddy, which no deployment owns.
	HostSites []string
	// Registered maps each deployment id the host's registry has an entry
	// for to what that entry says is kept, "" for nothing. It is nil when
	// the registry cannot be read or parsed, which says nothing about any
	// entry: the host check reads it only to word a conflict.
	Registered map[string]string
}

// Docker is the engine, if there is one.
type Docker struct {
	Present bool
	Version string
	// Packages names where it came from: docker-ce, docker.io or snap.
	Packages []string
}

// The probes, one command per fact. Every one only reads. The transport runs
// them under sudo, which `ss -p` needs to name another user's process, and
// rootProbe checks that it did.
//
// The three inspects take a list read a moment earlier, and a container,
// volume or network removed in between makes `docker inspect` complain and
// exit non zero while still printing the rest. That is not a failure to
// look: what is gone holds nothing, so the complaint is discarded and what
// answered is read.
const (
	rootProbe      = "id -u"
	dockerProbe    = `command -v docker >/dev/null 2>&1 || { echo absent; exit 0; }; docker version --format '{{.Server.Version}}'`
	packageProbe   = `dpkg-query -W -f='${Package} ${Status}\n' docker-ce docker.io 2>/dev/null; snap list docker 2>/dev/null; true`
	containerProbe = `docker ps -aq --no-trunc | xargs -r docker inspect --format '{"id":{{json .Id}},"name":{{json .Name}},"image":{{json .Image}},"pid":{{.State.Pid}},"labels":{{json .Config.Labels}},"networks":{{json .NetworkSettings.Networks}},"ports":{{json .HostConfig.PortBindings}}}' 2>/dev/null || true`
	volumeProbe    = `docker volume ls -q | xargs -r docker volume inspect --format '{"name":{{json .Name}},"labels":{{json .Labels}}}' 2>/dev/null || true`
	networkProbe   = `docker network ls -q --no-trunc | xargs -r docker network inspect --format '{"id":{{json .Id}},"name":{{json .Name}},"labels":{{json .Labels}},"ipam":{{json .IPAM.Config}}}' 2>/dev/null || true`
	socketProbe    = "ss -Hltnup"
	linkProbe      = "ip -o link"
	routeProbe     = "ip -j route"
	ufwProbe       = `command -v ufw >/dev/null 2>&1 || { echo 'ufw absent'; exit 0; }; ufw status verbose`
	firewalldProbe = "systemctl is-active firewalld || true"
	// Each file is printed only when it is one, since an unmatched glob is
	// left as the literal pattern.
	hostSitesProbe = `for f in ` + render.HostSitesDir + `/*.caddy; do [ -f "$f" ] && echo "$f"; done; true`

	cLocale = "export LC_ALL=C; "
)

// ReadCommands is every fixed command Inspect sends, exactly as it sends it,
// for a caller's allow list of reads. The one command that varies is the
// cgroup read, which starts with CgroupPrefix.
func ReadCommands() []string {
	var out []string
	for _, probe := range []string{rootProbe, dockerProbe, packageProbe, containerProbe, volumeProbe, networkProbe, socketProbe, linkProbe, routeProbe, ufwProbe, firewalldProbe, hostSitesProbe} {
		out = append(out, cLocale+probe)
	}
	return out
}

// CgroupPrefix starts the cgroup read, whose PID list varies.
const CgroupPrefix = cLocale + "for p in "

// cgroupProbe prints, per PID, the PID and its cgroup lines on one line. A
// process that exited since ss ran prints its PID alone.
func cgroupProbe(pids []int) string {
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	// 2>/dev/null comes first: redirections apply left to right, and the
	// one that fails for an exited process is the < after it.
	return fmt.Sprintf(`for p in %s; do printf '%%s ' "$p"; tr '\n' ' ' 2>/dev/null < /proc/$p/cgroup; echo; done`, strings.Join(list, " "))
}

// Inspect reads what a host already runs. It changes nothing. A probe that
// fails is an error rather than an empty answer: a host check that could not
// look has not found the host clean. d is the deployment whose manifest is
// read.
func Inspect(t Transport, d deployment.Deployment) (*Inventory, error) {
	inv := &Inventory{Host: t.Describe(), Cgroups: map[int]string{}, ManifestPath: d.Manifest()}
	fail := func(what string, err error) error {
		return fmt.Errorf("host check on %s: %s: %w", inv.Host, what, err)
	}
	// Every probe runs in the C locale, because ufw, dpkg, ss and systemctl
	// translate what they print and the parsers read the English words.
	run := func(what, command string) (string, error) {
		out, err := t.Run(cLocale + command)
		if err != nil {
			return "", fail(what, fmt.Errorf("%v: %s", err, firstLine(out)))
		}
		return out, nil
	}

	out, err := run("asking who the probes run as", rootProbe)
	if err != nil {
		return nil, err
	}
	if uid := strings.TrimSpace(out); uid != "0" {
		return nil, fail("asking who the probes run as", fmt.Errorf("they run as uid %s, not root, so ss cannot name the process behind another user's listener and every one of them would read as unowned. Run without --sudo=false, or as root", uid))
	}

	out, err = run("asking Docker for its version", dockerProbe)
	if err != nil {
		return nil, fmt.Errorf("%w. Docker is installed and did not answer, so the containers on this host cannot be seen. Start it (Eg: systemctl start docker) and run again", err)
	}
	if v := strings.TrimSpace(out); v != "absent" {
		inv.Docker = Docker{Present: true, Version: v}
	}
	if out, err = run("reading Docker's package", packageProbe); err != nil {
		return nil, err
	}
	inv.Docker.Packages = parsePackages(out)

	if inv.Docker.Present {
		if out, err = run("listing containers", containerProbe); err != nil {
			return nil, err
		}
		if inv.Containers, err = parseContainers(out); err != nil {
			return nil, fail("listing containers", err)
		}
		if out, err = run("listing volumes", volumeProbe); err != nil {
			return nil, err
		}
		if inv.Volumes, err = parseVolumes(out); err != nil {
			return nil, fail("listing volumes", err)
		}
		if out, err = run("listing networks", networkProbe); err != nil {
			return nil, err
		}
		if inv.Networks, err = parseNetworks(out); err != nil {
			return nil, fail("listing networks", err)
		}
	}

	if out, err = run("listing listeners", socketProbe); err != nil {
		return nil, err
	}
	if inv.Sockets, err = parseSockets(out); err != nil {
		return nil, fail("listing listeners", err)
	}
	if pids := listeningPIDs(inv.Sockets); len(pids) > 0 {
		if out, err = run("reading listeners' cgroups", cgroupProbe(pids)); err != nil {
			return nil, err
		}
		inv.Cgroups = parseCgroups(out)
	}

	if out, err = run("listing interfaces", linkProbe); err != nil {
		return nil, err
	}
	inv.Links = parseLinks(out)
	if out, err = run("listing routes", routeProbe); err != nil {
		return nil, err
	}
	if inv.Routes, err = parseRoutes(out); err != nil {
		return nil, fail("listing routes", err)
	}

	if out, err = run("reading ufw's status", ufwProbe); err != nil {
		return nil, err
	}
	inv.Firewall = parseUFW(out)
	if out, err = run("asking whether firewalld is active", firewalldProbe); err != nil {
		return nil, err
	}
	inv.Firewall.Firewalld = strings.TrimSpace(out) == "active"

	if out, err = run("listing "+render.HostSitesDir, hostSitesProbe); err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			inv.HostSites = append(inv.HostSites, line)
		}
	}
	sort.Strings(inv.HostSites)

	if reg, _, err := t.ReadFile(registry.Path); err == nil {
		if r, err := registry.Parse([]byte(reg)); err == nil {
			inv.Registered = map[string]string{}
			for id, e := range r.Deployments {
				inv.Registered[id] = e.Kept
			}
		}
	}

	content, found, err := t.ReadFile(inv.ManifestPath)
	if err != nil {
		return nil, fail("reading "+inv.ManifestPath, err)
	}
	inv.Manifest = found
	if found {
		// An unreadable manifest records nothing, so a mesh interface beside
		// it is not known to be the toolkit's. apply refuses that manifest itself.
		var m render.Manifest
		if json.Unmarshal([]byte(content), &m) == nil {
			inv.ManifestFiles = m.Files
			for _, f := range m.Files {
				if f.Path == d.WireGuardConf() {
					inv.ManifestWireGuard = true
				}
			}
		}
	}
	return inv, nil
}

func listeningPIDs(sockets []Socket) []int {
	seen := map[int]bool{}
	var pids []int
	for _, s := range sockets {
		if s.PID > 0 && !seen[s.PID] {
			seen[s.PID] = true
			pids = append(pids, s.PID)
		}
	}
	sort.Ints(pids)
	return pids
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
