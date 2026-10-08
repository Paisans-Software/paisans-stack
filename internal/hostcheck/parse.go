package hostcheck

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// composeProject is the label Compose puts on every container, volume and
// network it creates.
const composeProject = "com.docker.compose.project"

// anonymousLabel is the label Docker puts on an anonymous volume, and
// anonymousName the name an engine that sets no label gives one.
const anonymousLabel = "com.docker.volume.anonymous"

var anonymousName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// containerID is how a cgroup path names a container: the full 64 hex ID,
// as docker-<id>.scope under the systemd driver or docker/<id> under
// cgroupfs.
var containerID = regexp.MustCompile(`[0-9a-f]{64}`)

// processField is the first process ss names for a socket:
// users:(("caddy",pid=812,fd=7),...).
var processField = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

// Socket is one listener `ss -Hltnup` printed.
type Socket struct {
	Proto string
	// Address is the bound address, "" for every address.
	Address string
	Port    int
	// Process and PID are empty for a socket the kernel holds itself, as
	// WireGuard's is.
	Process string
	PID     int
}

// Listener is the socket as render describes a bind, so the two compare
// with render.Listener.Overlaps.
func (s Socket) Listener() render.Listener {
	return render.Listener{Proto: s.Proto, Address: s.Address, Port: s.Port}
}

// parseSockets reads `ss -Hltnup`: Netid, State, Recv-Q, Send-Q, Local
// Address:Port, Peer Address:Port, then the process. The port is after the
// last colon, which holds for "*:22", "[::]:22" and "127.0.0.53%lo:53".
func parseSockets(out string) ([]Socket, error) {
	var list []Socket
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || (f[0] != "tcp" && f[0] != "udp") {
			continue
		}
		local := f[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			return nil, fmt.Errorf("unreadable ss address %q", local)
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			return nil, fmt.Errorf("unreadable ss port in %q", local)
		}
		s := Socket{Proto: f[0], Address: bindAddress(local[:i]), Port: port}
		if m := processField.FindStringSubmatch(line); m != nil {
			s.Process = m[1]
			s.PID, _ = strconv.Atoi(m[2])
		}
		list = append(list, s)
	}
	return list, nil
}

// bindAddress normalises an address as ss or Docker prints it: the interface
// scope and brackets dropped, and every spelling of "any address" made "",
// as render.Listener has it.
func bindAddress(a string) string {
	if i := strings.Index(a, "%"); i >= 0 {
		a = a[:i]
	}
	a = strings.TrimSuffix(strings.TrimPrefix(a, "["), "]")
	switch a {
	case "*", "0.0.0.0", "::":
		return ""
	}
	return a
}

// Container is one container, running or stopped.
type Container struct {
	ID, Name string
	// Project is its compose project label, "" for none.
	Project string
	// PID is its main process, 0 when it is not running.
	PID int
	// Bindings are the host ports it publishes. They come from its
	// configuration rather than from what is listening, because Docker
	// without its userland proxy publishes a port with no listener at all,
	// and a stopped container binds them again when it starts, at boot
	// under a restart policy.
	Bindings []render.Listener
}

type inspectedContainer struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	PID    int               `json:"pid"`
	Labels map[string]string `json:"labels"`
	Ports  map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	} `json:"ports"`
}

func parseContainers(out string) ([]Container, error) {
	var list []Container
	err := eachJSONLine(out, func(line []byte) error {
		var in inspectedContainer
		if err := json.Unmarshal(line, &in); err != nil {
			return err
		}
		c := Container{ID: in.ID, Name: strings.TrimPrefix(in.Name, "/"), Project: in.Labels[composeProject], PID: in.PID}
		ports := make([]string, 0, len(in.Ports))
		for p := range in.Ports {
			ports = append(ports, p)
		}
		sort.Strings(ports)
		for _, p := range ports {
			proto := "tcp"
			if _, after, ok := strings.Cut(p, "/"); ok {
				proto = after
			}
			for _, b := range in.Ports[p] {
				port, err := strconv.Atoi(b.HostPort)
				if err != nil || port == 0 {
					continue // an ephemeral port, which no claim can name
				}
				c.Bindings = append(c.Bindings, render.Listener{Owner: c.Name, Proto: proto, Address: bindAddress(b.HostIP), Port: port})
			}
		}
		list = append(list, c)
		return nil
	})
	return list, err
}

// Volume is one Docker volume.
type Volume struct {
	Name, Project string
	// Anonymous is a volume Docker made for a container, not one somebody
	// named.
	Anonymous bool
}

func parseVolumes(out string) ([]Volume, error) {
	var list []Volume
	err := eachJSONLine(out, func(line []byte) error {
		var in struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		}
		if err := json.Unmarshal(line, &in); err != nil {
			return err
		}
		_, labelled := in.Labels[anonymousLabel]
		list = append(list, Volume{Name: in.Name, Project: in.Labels[composeProject], Anonymous: labelled || anonymousName.MatchString(in.Name)})
		return nil
	})
	return list, err
}

// Network is one Docker network and the subnets it hands out.
type Network struct {
	ID, Name, Project string
	Subnets           []string
}

func parseNetworks(out string) ([]Network, error) {
	var list []Network
	err := eachJSONLine(out, func(line []byte) error {
		var in struct {
			ID     string            `json:"id"`
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
			IPAM   []struct {
				Subnet string `json:"Subnet"`
			} `json:"ipam"`
		}
		if err := json.Unmarshal(line, &in); err != nil {
			return err
		}
		n := Network{ID: in.ID, Name: in.Name, Project: in.Labels[composeProject]}
		for _, c := range in.IPAM {
			if c.Subnet != "" {
				n.Subnets = append(n.Subnets, c.Subnet)
			}
		}
		list = append(list, n)
		return nil
	})
	return list, err
}

// eachJSONLine calls fn for every line that holds a JSON object.
func eachJSONLine(out string, fn func([]byte) error) error {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return fmt.Errorf("unreadable line %q: %w", line, err)
		}
	}
	return nil
}

// Route is one entry of `ip -j route`.
type Route struct {
	Dst string `json:"dst"`
	Dev string `json:"dev"`
}

func parseRoutes(out string) ([]Route, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var list []Route
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("unreadable `ip -j route` output: %w", err)
	}
	return list, nil
}

// parseLinks reads interface names from `ip -o link`: "3: wg0: <...>", or
// "7: veth1@if6: <...>" for one end of a pair.
func parseLinks(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimSuffix(f[1], ":"), "@")
		names = append(names, name)
	}
	return names
}

// Firewall is what the host's firewalls say about themselves.
type Firewall struct {
	// UFW is installed, and Active is its status.
	UFW, Active bool
	// Incoming is ufw's default for incoming traffic: deny, reject or
	// allow, as `ufw status verbose` prints it.
	Incoming string
	// Firewalld is active.
	Firewalld bool
}

// parseUFW reads `ufw status verbose`:
//
//	Status: active
//	Default: deny (incoming), allow (outgoing), disabled (routed)
func parseUFW(out string) Firewall {
	var f Firewall
	if strings.TrimSpace(out) == "ufw absent" {
		return f
	}
	f.UFW = true
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Status:"); ok {
			f.Active = strings.TrimSpace(v) == "active"
		}
		if v, ok := strings.CutPrefix(line, "Default:"); ok {
			for _, part := range strings.Split(v, ",") {
				if fields := strings.Fields(part); len(fields) == 2 && fields[1] == "(incoming)" {
					f.Incoming = fields[0]
				}
			}
		}
	}
	return f
}

// parsePackages names where Docker came from, from dpkg-query's
// "<package> <status>" lines and `snap list docker`'s table.
func parsePackages(out string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case (f[0] == "docker-ce" || f[0] == "docker.io") && strings.HasSuffix(strings.TrimSpace(line), "ok installed"):
			found = append(found, f[0])
		case f[0] == "docker" && len(f) > 1:
			found = append(found, "snap")
		}
	}
	return found
}

// parseCgroups reads cgroupProbe's lines, "<pid> <cgroup lines>", into the
// container each PID belongs to, for those that belong to one.
func parseCgroups(out string) map[int]string {
	in := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		if id := containerID.FindString(strings.Join(f[1:], " ")); id != "" {
			in[pid] = id
		}
	}
	return in
}
