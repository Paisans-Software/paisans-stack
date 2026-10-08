package hostprep

import (
	"fmt"
	"strings"
)

// ubuntu is the profile for Ubuntu releases. Its shell lives in
// profiles/ubuntu-<version>/; a second release that differs only in its
// templates is a second directory and a second register call.
type ubuntu struct {
	version string
}

func init() {
	register(ubuntu{version: "24.04"})
}

func (u ubuntu) ID() string      { return "ubuntu" }
func (u ubuntu) Version() string { return u.version }

func (u ubuntu) tmpl(name string) string { return "profiles/ubuntu-" + u.version + "/" + name }

// dockerPackages are the ones Docker's install page names, step 2 of "Install
// using the apt repository": https://docs.docker.com/engine/install/ubuntu/
// (fetched 2026-10-06).
var dockerPackages = []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"}

// dockerConflicts are the packages the same page says to remove first,
// containerd and runc included because Docker Engine ships its own.
var dockerConflicts = []string{"docker.io", "docker-compose", "docker-compose-v2", "docker-doc", "docker-buildx", "podman-docker", "containerd", "runc"}

// basePackages are needed whatever else is installed: wg and wg-quick for
// the mesh, and the firewall this profile drives.
var basePackages = []string{"wireguard-tools", "ufw"}

const (
	dockerKeyring = "/etc/apt/keyrings/docker.asc"
	dockerSources = "/etc/apt/sources.list.d/docker.sources"
)

type packageFacts struct {
	composeWorks bool
	installed    map[string]bool
	keyring      bool
	arch         string
	snapDocker   bool
}

func (u ubuntu) probePackages(t Transport) (packageFacts, error) {
	all := append(append(append([]string{}, dockerPackages...), dockerConflicts...), basePackages...)
	script, err := snippet(u.tmpl("packages-probe.sh.tmpl"), map[string]any{"Packages": all})
	if err != nil {
		return packageFacts{}, err
	}
	out, err := t.Run(script)
	if err != nil {
		return packageFacts{}, err
	}
	facts := packageFacts{installed: map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		key, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "compose":
			facts.composeWorks = rest == "present"
		case "pkg":
			name, status, _ := strings.Cut(rest, " ")
			// dpkg's "install ok installed". "deinstall ok config-files" is a
			// removed package whose configuration was kept, which is absent.
			facts.installed[name] = strings.HasSuffix(strings.TrimSpace(status), "ok installed")
		case "keyring":
			facts.keyring = rest == "present"
		case "arch":
			facts.arch = strings.TrimSpace(rest)
		case "snap":
			facts.snapDocker = strings.TrimSpace(rest) == "docker"
		}
	}
	return facts, nil
}

// Packages installs Docker from Docker's own repository rather than Ubuntu's
// docker.io. The compose plugin this toolkit runs (`docker compose`, v2) is
// what Docker's repository ships and supports; the toolkit is tested against
// Docker's packages, and one source for every host keeps two sites from
// running different engines because they were prepared a month apart.
func (u ubuntu) Packages(t Transport, host OSRelease) (Section, error) {
	var out Section
	facts, err := u.probePackages(t)
	if err != nil {
		return out, err
	}

	// Refused, not removed, for the docker.io reason below, and whether or
	// not compose works with it: every host runs Docker from Docker's own
	// repository, which is what the toolkit is tested against, and a snap
	// beside those packages would be a second engine.
	if facts.snapDocker {
		return out, fmt.Errorf("docker: Docker is installed as a snap, and every host here runs Docker Engine from Docker's own apt repository. Remove it yourself (snap remove docker), after checking nothing running depends on it, and prepare again")
	}

	var install []string
	if facts.composeWorks {
		out.Present = append(out.Present, "docker: `docker compose` already works")
	} else {
		// Refused, not removed. Removing docker.io on a host that is already
		// running containers from it takes those containers down, and that is
		// a decision for whoever started them.
		var conflicts []string
		for _, p := range dockerConflicts {
			if facts.installed[p] {
				conflicts = append(conflicts, p)
			}
		}
		if len(conflicts) > 0 {
			return out, fmt.Errorf("docker: %s from Ubuntu's archive conflict with Docker's own packages, and `docker compose` does not work with them. Remove them yourself (apt-get remove %s), after checking nothing running depends on them, and prepare again",
				strings.Join(conflicts, ", "), strings.Join(conflicts, " "))
		}

		if facts.keyring {
			out.Present = append(out.Present, "docker: apt signing key at "+dockerKeyring)
		} else {
			script, err := snippet(u.tmpl("docker-keyring.sh.tmpl"), nil)
			if err != nil {
				return out, err
			}
			out.Steps = append(out.Steps, Step{Describe: "docker: add Docker's apt signing key at " + dockerKeyring, Command: script})
		}

		codename := host.Fields["UBUNTU_CODENAME"]
		if codename == "" {
			codename = host.Fields["VERSION_CODENAME"]
		}
		if codename == "" || facts.arch == "" {
			return out, fmt.Errorf("docker: could not read the release codename (%q) or the dpkg architecture (%q), and Docker's apt source needs both", codename, facts.arch)
		}
		sources, err := snippet(u.tmpl("docker.sources.tmpl"), map[string]string{"Codename": codename, "Arch": facts.arch})
		if err != nil {
			return out, err
		}
		current, found, err := t.ReadFile(dockerSources)
		if err != nil {
			return out, err
		}
		if found && current == sources {
			out.Present = append(out.Present, "docker: apt repository at "+dockerSources)
		} else {
			out.Steps = append(out.Steps, Step{
				Describe: fmt.Sprintf("docker: add Docker's apt repository for %s/%s at %s", codename, facts.arch, dockerSources),
				File:     &File{Path: dockerSources, Content: sources, Mode: 0o644},
			})
		}
		for _, p := range dockerPackages {
			if !facts.installed[p] {
				install = append(install, p)
			}
		}
	}

	for _, p := range basePackages {
		if facts.installed[p] {
			out.Present = append(out.Present, "package: "+p+" installed")
		} else {
			install = append(install, p)
		}
	}
	if len(install) > 0 {
		script, err := snippet(u.tmpl("apt-install.sh.tmpl"), map[string]any{"Packages": install})
		if err != nil {
			return out, err
		}
		out.Steps = append(out.Steps, Step{Describe: "packages: install " + strings.Join(install, " "), Command: script})
	}
	return out, nil
}

// unitState reads `systemctl is-enabled` and `is-active` for one unit.
func (u ubuntu) unitState(t Transport, unit string) (enabled, active bool, err error) {
	script, err := snippet(u.tmpl("services-probe.sh.tmpl"), map[string]string{"Unit": unit})
	if err != nil {
		return false, false, err
	}
	out, err := t.Run(script)
	if err != nil {
		return false, false, err
	}
	for _, line := range strings.Split(out, "\n") {
		key, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "enabled":
			enabled = rest == "enabled"
		case "active":
			active = rest == "active"
		}
	}
	return enabled, active, nil
}

// dockerAfterWG0 is the drop-in that orders Docker after the mesh interface.
const dockerAfterWG0 = "/etc/systemd/system/docker.service.d/paisans-after-wg0.conf"

// Services makes Docker start at boot and now, and after wg0 at boot. Docker's
// packages enable it on install, so on a freshly installed host enabling it is
// a no-op that is planned anyway: the probe runs before the install and cannot
// know that.
//
// The ordering is a drop-in rather than an edit to Docker's own unit, which a
// package upgrade replaces. Writing it needs only a daemon-reload: it takes
// effect at the next boot, which is the only moment it matters, so Docker and
// every container on the host keep running.
func (u ubuntu) Services(t Transport) (Section, error) {
	var out Section
	dropIn, err := snippet(u.tmpl("docker-after-wg0.conf.tmpl"), nil)
	if err != nil {
		return out, err
	}
	current, found, err := t.ReadFile(dockerAfterWG0)
	if err != nil {
		return out, err
	}
	if found && current == dropIn {
		out.Present = append(out.Present, "service: docker starts after wg0 at boot")
	} else {
		out.Steps = append(out.Steps, Step{
			Describe: "service: start docker after wg0 at boot, so containers can bind the mesh address (" + dockerAfterWG0 + ")",
			File:     &File{Path: dockerAfterWG0, Content: dropIn, Mode: 0o644},
			Command:  "systemctl daemon-reload",
		})
	}
	enabled, active, err := u.unitState(t, "docker")
	if err != nil {
		return out, err
	}
	if enabled && active {
		out.Present = append(out.Present, "service: docker enabled and running")
		return out, nil
	}
	out.Steps = append(out.Steps, Step{Describe: "service: enable and start docker", Command: "systemctl enable --now docker"})
	return out, nil
}

// watchdogUnit is the unit that loads the watchdog module at boot.
const watchdogUnit = "paisans-watchdog.service"

// WatchdogModule loads the module now with modprobe and at boot with a unit,
// not a modules-load.d entry. Ubuntu's kernel package ships a modprobe
// blacklist for the watchdog drivers, softdog among them, and
// systemd-modules-load honours it (Launchpad bug 1535840; systemd's
// module_load_and_warn probes with KMOD_PROBE_APPLY_BLACKLIST), so a
// modules-load.d file would be skipped at every boot while looking correct.
// modprobe named on a command line does not apply the blacklist.
func (u ubuntu) WatchdogModule(t Transport, module string, loaded bool) (Section, error) {
	var out Section
	if !loaded {
		out.Steps = append(out.Steps, Step{Describe: "watchdog: load " + module + " now", Command: "modprobe " + module})
	}
	unit, err := snippet(u.tmpl("watchdog-module.service.tmpl"), map[string]string{"Module": module})
	if err != nil {
		return out, err
	}
	path := "/etc/systemd/system/" + watchdogUnit
	current, found, err := t.ReadFile(path)
	if err != nil {
		return out, err
	}
	enabled, _, err := u.unitState(t, watchdogUnit)
	if err != nil {
		return out, err
	}
	switch {
	case !found || current != unit:
		out.Steps = append(out.Steps, Step{
			Describe: fmt.Sprintf("watchdog: load %s at every boot (%s)", module, watchdogUnit),
			File:     &File{Path: path, Content: unit, Mode: 0o644},
			Command:  "systemctl daemon-reload && systemctl enable " + watchdogUnit,
		})
	case !enabled:
		out.Steps = append(out.Steps, Step{
			Describe: fmt.Sprintf("watchdog: enable %s so %s loads at every boot", watchdogUnit, module),
			Command:  "systemctl enable " + watchdogUnit,
		})
	default:
		out.Present = append(out.Present, fmt.Sprintf("watchdog: %s loads %s at every boot", watchdogUnit, module))
	}
	return out, nil
}

// Firewall drives ufw. The order of the steps is the safety property: every
// allow and adoption, SSH first, then the default policy, then enabling, so
// there is no moment at which the firewall is on and SSH is not allowed; and
// removals last, so a rule is only taken away once everything it might have
// stood in for is in place. `--force` is what stops `ufw enable` asking
// whether to disrupt existing connections, a prompt that would hang a
// non-interactive run. Which rules are added, adopted, removed or left alone
// is planRules, in ufw.go. On a shared host (hostWide false) only the rules
// are planned: the default policy and enabling belong to the host, not to
// the deployment.
func (u ubuntu) Firewall(t Transport, rules []Rule, hostWide bool) (Section, error) {
	var out Section
	script, err := snippet(u.tmpl("firewall-probe.sh.tmpl"), nil)
	if err != nil {
		return out, err
	}
	probe, err := t.Run(script)
	if err != nil {
		return out, err
	}
	var present, active bool
	var added []addedRule
	for _, line := range strings.Split(probe, "\n") {
		key, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "ufw":
			present = rest == "present"
		case "status":
			active = rest == "active"
		case "rule":
			if a, ok := parseAdded(rest); ok {
				added = append(added, a)
			}
		}
	}

	planned, removals, err := planRules(rules, added)
	if err != nil {
		return out, err
	}
	out.add(planned)

	if !hostWide {
		out.Present = append(out.Present, "firewall: default policy and enabled state left alone, since the host is shared")
		out.Steps = append(out.Steps, removals...)
		return out, nil
	}

	input, output := "", ""
	if present {
		defaults, found, err := t.ReadFile("/etc/default/ufw")
		if err != nil {
			return out, err
		}
		if found {
			fields := ParseOSRelease(defaults).Fields // the same KEY="value" shape
			input, output = fields["DEFAULT_INPUT_POLICY"], fields["DEFAULT_OUTPUT_POLICY"]
		}
	}
	if input == "DROP" {
		out.Present = append(out.Present, "firewall: incoming denied by default")
	} else {
		out.Steps = append(out.Steps, Step{Describe: "firewall: deny incoming by default", Command: "ufw default deny incoming"})
	}
	if output == "ACCEPT" {
		out.Present = append(out.Present, "firewall: outgoing allowed by default")
	} else {
		out.Steps = append(out.Steps, Step{Describe: "firewall: allow outgoing by default", Command: "ufw default allow outgoing"})
	}
	if active {
		out.Present = append(out.Present, "firewall: ufw active")
	} else {
		out.Steps = append(out.Steps, Step{Describe: "firewall: enable ufw", Command: "ufw --force enable"})
	}
	out.Steps = append(out.Steps, removals...)
	return out, nil
}
