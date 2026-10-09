// Package hostcaddy adds a monitor's site block to a Caddy the toolkit does
// not run: the web server that already holds ports 80 and 443 on a host where
// the monitor is in ingress mode external.
//
// Detect finds that Caddy and reads how it is configured, changing nothing.
// PlanChange decides the one edit the toolkit may make to it: a file of its
// own in a directory the Caddyfile already imports, or one marked block
// appended to the Caddyfile itself. Execute makes that edit, keeps a backup
// of any file that was not the toolkit's, validates with the Caddy in that
// container, reloads it gracefully, and restores the backup when either
// fails. Nothing else of that Caddy is touched: not another line of its
// configuration, not its container, its networks or its volumes.
package hostcaddy

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

// Transport is how this package reaches the host. apply.SSHTransport
// satisfies it. A file is written with RunInput rather than a temporary file
// moved into place, because a Caddyfile bind mounted on its own is mounted by
// inode: a file moved over it is never seen inside the container.
type Transport interface {
	Run(command string) (string, error)
	ReadFile(path string) (content string, found bool, err error)
	RunInput(command, stdin string) (string, error)
	Describe() string
}

// Finding is the web server holding 443/tcp on a host, as Detect read it.
type Finding struct {
	// Container is the container holding the port, as the host check found
	// it.
	Container hostcheck.Container
	Image     string
	// Version is what `caddy version` printed in it, empty when it is not
	// Caddy.
	Version string
	// HostNetwork is whether it shares the host's network namespace, where
	// the host's loopback is its own.
	HostNetwork bool
	// Network is the user defined Docker network it is on, when it is not on
	// the host's, and Address its IPv4 address there. The app joins Network
	// to be reached (render.HostProxy).
	Network string
	Address string
	// Config is the Caddyfile as the container sees it, from its `--config`
	// argument, and ConfigHost the same file on the host, through its bind
	// mount.
	Config     string
	ConfigHost string
	// Caddyfile is ConfigHost's content when it was read.
	Caddyfile string
	// Admin is the admin endpoint the Caddyfile sets, empty for Caddy's
	// default, which `caddy reload` reaches without being told.
	Admin string
	// Import is the directory the Caddyfile imports site files from, nil
	// when it imports none the toolkit can add a file to.
	Import *Import
	// ProbeAddress is where the host reaches this server's 443, for the
	// check after a reload.
	ProbeAddress string
	// Unsupported says why the toolkit cannot add a site block to this
	// server, empty when it can. A server it cannot edit gets the block
	// printed for its owner to add by hand.
	Unsupported string
}

// Name is the container's name, for messages.
func (f *Finding) Name() string {
	if f.Container.Project != "" {
		return fmt.Sprintf("container %s (compose project %s)", f.Container.Name, f.Container.Project)
	}
	return "container " + f.Container.Name
}

// Import is a directory the Caddyfile imports every matching file of, at its
// top level.
type Import struct {
	// Glob is the import as the container sees it, made absolute.
	Glob string
	// HostDir is the glob's directory on the host, through its bind mount.
	HostDir string
	// File is the name the toolkit's own file takes there, chosen so the
	// glob matches it.
	File string
}

// inspected is the part of `docker inspect` Detect reads. The format names
// each field, so the container's environment, which may hold credentials,
// never leaves the host.
type inspected struct {
	ID          string   `json:"id"`
	Image       string   `json:"image"`
	Path        string   `json:"path"`
	Args        []string `json:"args"`
	WorkingDir  string   `json:"workdir"`
	NetworkMode string   `json:"network_mode"`
	Running     bool     `json:"running"`
	Networks    map[string]struct {
		IPAddress string `json:"IPAddress"`
	} `json:"networks"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"mounts"`
}

const inspectFormat = `{"id":{{json .Id}},"image":{{json .Config.Image}},"path":{{json .Path}},"args":{{json .Args}},"workdir":{{json .Config.WorkingDir}},"network_mode":{{json .HostConfig.NetworkMode}},"running":{{json .State.Running}},"networks":{{json .NetworkSettings.Networks}},"mounts":{{json .Mounts}}}`

// InspectCommand is the read Detect makes of the container.
func InspectCommand(id string) string {
	return "docker inspect --format " + shellQuote(inspectFormat) + " " + shellQuote(id)
}

// VersionCommand asks the container whether it is Caddy.
func VersionCommand(id string) string {
	return "docker exec " + shellQuote(id) + " caddy version"
}

// Detect finds the container holding 443/tcp on the host the report is of,
// and reads how its Caddy is configured. It changes nothing. It returns nil
// when no container holds the port. A Finding whose Unsupported is set is a
// server the toolkit cannot add to, and says why.
//
// A probe that fails is an error, never a finding: a server the toolkit could
// not look at has not been found to be anything.
func Detect(report *hostcheck.Report, t Transport) (*Finding, error) {
	c, ok := report.ForeignContainer("tcp", 443)
	if !ok {
		return nil, nil
	}
	f := &Finding{Container: c, ProbeAddress: probeAddress(report, c)}
	out, err := t.Run(InspectCommand(c.ID))
	if err != nil {
		return nil, fmt.Errorf("reading %s, which holds 443/tcp: %w", f.Name(), err)
	}
	var in inspected
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &in); err != nil {
		return nil, fmt.Errorf("reading %s, which holds 443/tcp: docker inspect printed something that is not its answer: %w", f.Name(), err)
	}
	f.Image = in.Image
	if !in.Running {
		f.Unsupported = f.Name() + " is not running, so it cannot be asked whether it is Caddy, validate a change or reload one"
		return f, nil
	}

	out, err = t.Run(VersionCommand(c.ID))
	if err != nil {
		// Docker says so when the container has no such program. Any other
		// failure is a failure to look.
		if strings.Contains(out, "executable file not found") || strings.Contains(out, "no such file or directory") || strings.Contains(err.Error(), "executable file not found") {
			f.Unsupported = fmt.Sprintf("%s (image %s) is not Caddy: it has no caddy program", f.Name(), f.Image)
			return f, nil
		}
		return nil, fmt.Errorf("asking %s whether it is Caddy: %w", f.Name(), err)
	}
	f.Version = firstLine(out)
	if !strings.HasPrefix(f.Version, "v2.") {
		f.Unsupported = fmt.Sprintf("%s answers `caddy version` with %q, which is not Caddy 2", f.Name(), f.Version)
		return f, nil
	}

	switch mode := in.NetworkMode; {
	case mode == "host":
		f.HostNetwork = true
	case strings.HasPrefix(mode, "container:"):
		f.Unsupported = fmt.Sprintf("%s shares another container's network (%s), so where it can reach the app depends on a container the toolkit knows nothing about", f.Name(), mode)
		return f, nil
	default:
		f.Network, f.Address = userNetwork(report, c, in)
		if f.Network == "" {
			f.Unsupported = fmt.Sprintf("%s is on Docker's default bridge network alone, where the host's loopback is not its own and no other container can join it by name. Put it on a network of its own (a compose project's default network is one), or run it with network_mode: host, and apply again", f.Name())
			return f, nil
		}
		if f.Address == "" {
			return nil, fmt.Errorf("%s is on network %s with no IPv4 address, so the app cannot be told to trust it", f.Name(), f.Network)
		}
	}

	config, adapter := configArgs(append([]string{in.Path}, in.Args...))
	if config == "" {
		f.Unsupported = fmt.Sprintf("%s runs Caddy without --config, so its configuration is whatever was loaded through its admin API or is in the image, not a file the toolkit can add a block to", f.Name())
		return f, nil
	}
	if !path.IsAbs(config) {
		config = path.Join(orRoot(in.WorkingDir), config)
	}
	f.Config = config
	if !isCaddyfile(config, adapter) {
		f.Unsupported = fmt.Sprintf("%s loads %s with the %s adapter. The toolkit adds a site block to a Caddyfile only", f.Name(), config, adapterName(adapter))
		return f, nil
	}
	mounts := map[string]string{}
	for _, m := range in.Mounts {
		if m.Type == "bind" {
			mounts[path.Clean(m.Destination)] = m.Source
		}
	}
	host, ok := hostPath(mounts, config)
	if !ok {
		f.Unsupported = fmt.Sprintf("%s reads %s from inside the container (in its image or a volume), not from a file bind mounted from the host, so an edit would be lost when the container is recreated", f.Name(), config)
		return f, nil
	}
	f.ConfigHost = host
	content, found, err := t.ReadFile(host)
	if err != nil {
		return nil, fmt.Errorf("reading %s's Caddyfile, %s: %w", f.Name(), host, err)
	}
	if !found {
		return nil, fmt.Errorf("%s mounts %s as its Caddyfile, and the host has no such file", f.Name(), host)
	}
	f.Caddyfile = content
	parsed := Parse(content)
	if parsed.AdminOff {
		f.Unsupported = fmt.Sprintf("%s's Caddyfile turns its admin endpoint off (admin off), so a change could only take effect by restarting it, and the toolkit never restarts a container it does not run", f.Name())
		return f, nil
	}
	f.Admin = parsed.Admin
	for _, glob := range parsed.Imports {
		if imp, ok := importDir(mounts, path.Dir(config), glob); ok {
			f.Import = imp
			break
		}
	}
	return f, nil
}

// probeAddress is where the host reaches the server's 443: the address its
// listener binds, or loopback when it binds every address.
func probeAddress(report *hostcheck.Report, c hostcheck.Container) string {
	for _, b := range c.Bindings {
		if b.Proto == "tcp" && b.Port == 443 && b.Address != "" && b.Address != "0.0.0.0" && !strings.Contains(b.Address, ":") {
			return b.Address
		}
	}
	if report != nil && report.Inventory != nil {
		for _, s := range report.Inventory.Sockets {
			if s.Proto == "tcp" && s.Port == 443 && report.Inventory.Cgroups[s.PID] == c.ID {
				if s.Address == "" || s.Address == "0.0.0.0" || strings.Contains(s.Address, ":") {
					return "127.0.0.1"
				}
				return s.Address
			}
		}
	}
	return "127.0.0.1"
}

// userNetwork is the user defined network the container is on that the app
// should join, with the container's address there: one belonging to the
// container's own compose project first, since that is the network its
// owner made for it, and otherwise the first by name. Docker's default
// bridge is never one: a container cannot be reached there by name.
func userNetwork(report *hostcheck.Report, c hostcheck.Container, in inspected) (string, string) {
	project := map[string]string{}
	if report != nil && report.Inventory != nil {
		for _, n := range report.Inventory.Networks {
			project[n.Name] = n.Project
		}
	}
	var names []string
	for name := range in.Networks {
		if name != "bridge" && name != "host" && name != "none" {
			names = append(names, name)
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		pi, pj := c.Project != "" && project[names[i]] == c.Project, c.Project != "" && project[names[j]] == c.Project
		if pi != pj {
			return pi
		}
		return names[i] < names[j]
	})
	if len(names) == 0 {
		return "", ""
	}
	return names[0], in.Networks[names[0]].IPAddress
}

// configArgs reads --config and --adapter from a Caddy command line, in
// either `--flag value` or `--flag=value` form, with one dash or two.
func configArgs(args []string) (config, adapter string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, name := range []string{"config", "adapter"} {
			for _, dash := range []string{"--", "-"} {
				flag := dash + name
				var v string
				var ok bool
				if a == flag && i+1 < len(args) {
					v, ok = args[i+1], true
				} else if after, cut := strings.CutPrefix(a, flag+"="); cut {
					v, ok = after, true
				}
				if ok {
					if name == "config" {
						config = v
					} else {
						adapter = v
					}
				}
			}
		}
	}
	return config, adapter
}

// isCaddyfile reports whether Caddy reads config as a Caddyfile: the
// caddyfile adapter named, or, with none named, a file Caddy itself treats as
// one (a name starting Caddyfile, or ending .caddyfile).
func isCaddyfile(config, adapter string) bool {
	if adapter != "" {
		return adapter == "caddyfile"
	}
	base := path.Base(config)
	return strings.HasPrefix(base, "Caddyfile") || strings.HasSuffix(base, ".caddyfile")
}

func adapterName(adapter string) string {
	if adapter == "" {
		return "default (JSON)"
	}
	return adapter
}

// hostPath maps a path inside the container to the host through the
// container's bind mounts, the deepest mount first, as the kernel resolves
// it.
func hostPath(mounts map[string]string, p string) (string, bool) {
	p = path.Clean(p)
	best := ""
	for dest := range mounts {
		if (p == dest || strings.HasPrefix(p, dest+"/") || dest == "/") && len(dest) > len(best) {
			best = dest
		}
	}
	if best == "" {
		return "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(p, best), "/")
	if rest == "" {
		return mounts[best], true
	}
	return path.Join(mounts[best], rest), true
}

// importDir is a top level import the toolkit can add a file to: a glob in
// its last element only, in a directory bind mounted from the host, with a
// pattern that admits a name of the toolkit's own.
func importDir(mounts map[string]string, base, glob string) (*Import, bool) {
	if !strings.ContainsAny(glob, "*?[") {
		return nil, false
	}
	if !path.IsAbs(glob) {
		glob = path.Join(base, glob)
	}
	dir, pattern := path.Split(glob)
	dir = path.Clean(dir)
	if strings.ContainsAny(dir, "*?[") || strings.ContainsAny(pattern, "?[") || strings.Count(pattern, "*") != 1 {
		return nil, false
	}
	host, ok := hostPath(mounts, dir)
	if !ok {
		return nil, false
	}
	return &Import{Glob: glob, HostDir: host, File: pattern}, true
}

// fileName is the toolkit's file in an import directory: the pattern with
// its one wildcard replaced, so the import matches it.
func (i *Import) fileName(token string) (string, bool) {
	name := strings.Replace(i.File, "*", "paisans-"+token, 1)
	ok, err := path.Match(i.File, name)
	return name, ok && err == nil && !strings.Contains(name, "/")
}

func orRoot(dir string) string {
	if dir == "" {
		return "/"
	}
	return dir
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
