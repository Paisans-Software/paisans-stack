// Package hostprep takes a blank host to the state `paisans apply` assumes:
// Docker Engine with the compose plugin, the WireGuard tools, a firewall that
// lets the mesh in and little else, and, on a data site, a /dev/watchdog that
// Patroni alone will hold.
//
// It is not part of apply. apply reconciles files it rendered against a
// manifest it wrote; nothing here is a rendered file. Installed packages,
// firewall rules and loaded kernel modules are host state that only the host
// can describe, so this package does what internal/garage does for object
// storage: read what is there, plan only what is missing, and change nothing
// unless told to with --execute.
//
// Everything that differs between operating systems lives behind Profile, and
// each profile's shell lives in its own directory under profiles/. Adding a
// distribution is adding a directory and a registration, not editing Build.
package hostprep

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"

	"github.com/paisans-software/paisans-stack/internal/config"
)

//go:embed profiles snippets
var templates embed.FS

// Transport is the one place this package talks to a machine.
// apply.SSHTransport satisfies it without changes.
type Transport interface {
	// Run executes a command and returns its combined output. A non zero exit
	// is an error.
	Run(command string) (string, error)
	// ReadFile returns a file's contents, reporting a missing file through
	// found rather than as an error.
	ReadFile(path string) (content string, found bool, err error)
	// WriteFile writes content at path over stdin, never on a command line.
	WriteFile(path string, content string, mode uint32) error
	// Describe names the destination, for messages.
	Describe() string
}

// File is content a step writes before it runs its command.
type File struct {
	Path    string
	Content string
	Mode    uint32
}

// Step is one change this plan still needs to make, in order.
type Step struct {
	// Describe is one line, shown to the operator before the step runs.
	Describe string
	// File, when set, is written first. It goes over stdin, so a unit file or
	// an apt source never sits in the process table.
	File *File
	// Command, when set, runs after File is written.
	Command string
	// Label is the word the plan prints before Describe. Empty is "change";
	// the firewall also uses "adopt" and "remove", so a step that deletes
	// something never reads like one that adds it.
	Label string
}

// Section is what one part of the preparation found: the steps still needed
// and what was already there.
type Section struct {
	Steps   []Step
	Present []string
	// Foreign is what was found that this package did not create and will not
	// touch, listed only where it bears on something this package manages.
	Foreign []string
	// Warnings are printed whether or not anything is planned, because a
	// prepared host that runs on softdog is still running on softdog.
	Warnings []string
}

func (s *Section) add(other Section) {
	s.Steps = append(s.Steps, other.Steps...)
	s.Present = append(s.Present, other.Present...)
	s.Foreign = append(s.Foreign, other.Foreign...)
	s.Warnings = append(s.Warnings, other.Warnings...)
}

// Plan is what Build found on one site's host.
type Plan struct {
	Site string
	// Profile names the host profile that was selected, as "<id> <version>".
	Profile string
	Section
}

// OSRelease is the part of /etc/os-release a profile is selected by and reads.
type OSRelease struct {
	ID        string
	VersionID string
	// Fields is every key in the file, for a profile that needs more than ID
	// and VERSION_ID (a codename, say).
	Fields map[string]string
}

// ParseOSRelease reads the KEY=value format of os-release(5). Values may be
// quoted with single or double quotes; comments and blank lines are skipped.
func ParseOSRelease(content string) OSRelease {
	fields := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		fields[key] = value
	}
	return OSRelease{ID: fields["ID"], VersionID: fields["VERSION_ID"], Fields: fields}
}

// Profile is everything about preparing a host that depends on its operating
// system. Build decides what a site needs from its roles and its watchdog
// mode; a profile decides how that is done on its distribution and how to tell
// whether it already has been.
//
// Every method probes read only and returns only what is missing. None of them
// changes the host: changes are Steps, run later by Execute.
type Profile interface {
	// ID and Version are what /etc/os-release must say for this profile to
	// be selected, exactly: ID and VERSION_ID.
	ID() string
	Version() string
	// Packages plans Docker Engine with the compose plugin, the WireGuard
	// tools, and whatever the profile's own firewall needs.
	Packages(t Transport, host OSRelease) (Section, error)
	// Services plans enabling and starting what Packages installed.
	Services(t Transport) (Section, error)
	// WatchdogModule plans loading a kernel module now, unless loaded says
	// it already is, and loading it at every boot.
	WatchdogModule(t Transport, module string, loaded bool) (Section, error)
	// Firewall plans the given inbound rules and, when hostWide, a default
	// deny for everything else and enabling the firewall. The SSH rule must
	// be in place before the firewall is enabled, and enabling must not
	// prompt. Rules it added earlier that are no longer given are removed,
	// last; rules it did not add are never removed or changed. hostWide is
	// false on a shared host, where the default policy and whether the
	// firewall is on decide other people's traffic too.
	Firewall(t Transport, rules []Rule, hostWide bool) (Section, error)
}

var registry []Profile

// register adds a profile. Each profile's file calls it from init, which is the
// whole of adding one.
func register(p Profile) { registry = append(registry, p) }

// Supported lists every registered profile as "<id> <version>", sorted.
func Supported() []string {
	var out []string
	for _, p := range registry {
		out = append(out, p.ID()+" "+p.Version())
	}
	sort.Strings(out)
	return out
}

// Select returns the profile for a host, or the error an operator sees when
// there is none. The wording is the founder's and a test pins it.
func Select(host OSRelease) (Profile, error) {
	for _, p := range registry {
		if p.ID() == host.ID && p.Version() == host.VersionID {
			return p, nil
		}
	}
	return nil, errors.New(UnsupportedMessage(host))
}

// UnsupportedMessage is the refusal for a host no profile matches.
func UnsupportedMessage(host OSRelease) string {
	return fmt.Sprintf("%s %s is not supported. Use one of the following:\n%s",
		host.ID, host.VersionID, strings.Join(Supported(), "\n"))
}

// Option adjusts what Build plans.
type Option func(*options)

type options struct {
	shared bool
}

// Shared plans for a host the host check found shared: something besides
// the deployment runs there. The firewall's default policy and enabled state
// are left alone, since they govern that other thing's traffic as well, and
// only the rules host prepare marks as its own are added and removed. The
// host check has already refused a shared host whose ufw is not active and
// denying incoming by default.
func Shared() Option { return func(o *options) { o.shared = true } }

// Build probes the site's host and returns only what is missing, in the order
// it must run: packages, then services, then the watchdog, then the login
// user's authorized keys, then the firewall, then removing authorized keys
// no longer listed. The firewall follows the packages because the package
// step may be what installs it; key removals are last so that nothing is
// taken away before every listed key is in place.
func Build(site string, cfg *config.Config, t Transport, opts ...Option) (*Plan, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	declared, ok := cfg.Sites[site]
	if !ok {
		return nil, fmt.Errorf("host prepare: no site %q is declared", site)
	}

	content, found, err := t.ReadFile("/etc/os-release")
	if err != nil {
		return nil, fmt.Errorf("reading /etc/os-release on %s: %w", t.Describe(), err)
	}
	if !found {
		return nil, fmt.Errorf("%s has no /etc/os-release, so no host profile can be chosen for it", t.Describe())
	}
	host := ParseOSRelease(content)
	profile, err := Select(host)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Site: site, Profile: profile.ID() + " " + profile.Version()}

	packages, err := profile.Packages(t, host)
	if err != nil {
		return nil, fmt.Errorf("packages on %s: %w", t.Describe(), err)
	}
	plan.add(packages)

	services, err := profile.Services(t)
	if err != nil {
		return nil, fmt.Errorf("services on %s: %w", t.Describe(), err)
	}
	plan.add(services)

	watchdog, err := planWatchdog(t, profile, declared)
	if err != nil {
		return nil, err
	}
	plan.add(watchdog)

	keys, keyRemovals, err := planAuthorizedKeys(t, declared.SSH)
	if err != nil {
		return nil, fmt.Errorf("authorized keys on %s: %w", t.Describe(), err)
	}
	plan.add(keys)

	firewall, err := profile.Firewall(t, append(Rules(declared), ContainerRules(cfg, site)...), !o.shared)
	if err != nil {
		return nil, fmt.Errorf("firewall on %s: %w", t.Describe(), err)
	}
	plan.add(firewall)

	// Last of all, after the firewall's own removals: a key is taken away
	// only once everything else, the listed keys included, is in place.
	plan.Steps = append(plan.Steps, keyRemovals...)

	return plan, nil
}

// Print writes the plan the way every host reaching command here does: the
// steps still to run, then what was already there, then any warning. A plan
// with no steps still lists what it found, so an operator can see what was
// checked rather than a blank that might mean nothing was.
func (p *Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "%s (%s)\n", p.Site, p.Profile)
	for _, step := range p.Steps {
		label := step.Label
		if label == "" {
			label = "change"
		}
		fmt.Fprintf(w, "  %-9s %s\n", label, step.Describe)
	}
	for _, present := range p.Present {
		fmt.Fprintf(w, "  %-9s %s\n", "present", present)
	}
	for _, foreign := range p.Foreign {
		fmt.Fprintf(w, "  %-9s %s\n", "present (not paisans)", foreign)
	}
	for _, warning := range p.Warnings {
		fmt.Fprintf(w, "  %-9s %s\n", "WARNING", warning)
	}
}

// Execute runs each step in order and stops at the first failure, naming the
// step, so an operator knows where a half finished run left the host. Every
// step is planned from a probe, so re-running Build after fixing the cause
// plans only what is still missing.
func Execute(plan *Plan, t Transport) error {
	for _, step := range plan.Steps {
		if step.File != nil {
			if err := t.WriteFile(step.File.Path, step.File.Content, step.File.Mode); err != nil {
				return fmt.Errorf("%s: %w", step.Describe, err)
			}
		}
		if step.Command == "" {
			continue
		}
		if out, err := t.Run(step.Command); err != nil {
			return fmt.Errorf("%s: %w: %s", step.Describe, err, strings.TrimSpace(out))
		}
	}
	return nil
}

// snippet renders one embedded shell template. A missing template or a bad
// one is a bug in this package, so it is returned rather than recovered from.
func snippet(path string, data any) (string, error) {
	tmpl, err := template.ParseFS(templates, path)
	if err != nil {
		return "", fmt.Errorf("loading %s: %w", path, err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("rendering %s: %w", path, err)
	}
	return out.String(), nil
}
