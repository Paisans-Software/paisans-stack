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
	"sort"
	"strings"
	"text/template"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/ui"
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
	// Title is the step's short name, an imperative verb and its object, as
	// the operator sees it by default. Describe is its full sentence, shown
	// with --verbose.
	Title string
	// Describe is one line saying what the step does and why.
	Describe string
	// File, when set, is written first. It goes over stdin, so a unit file or
	// an apt source never sits in the process table.
	File *File
	// Command, when set, runs after File is written.
	Command string
	// Label is the kind of change, so one can be told apart without reading
	// Describe: empty for an ordinary one, or "adopt", "add", "rename",
	// "share", "release" or "remove". It is not printed; Title already says
	// what the step does.
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
	// Warnings are reported whether or not anything is planned, because a
	// prepared host that runs on softdog is still running on softdog.
	Warnings []Warning
}

// Warning is a problem that leaves the host working but not as declared, or
// something left for the operator to do, such as deleting an old SSH allow.
// Hint is the line an operator sees by default; Detail says why and what to
// do, under --verbose.
type Warning struct {
	Hint   string
	Detail string
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
	// Its own files carry the deployment's token, so that two deployments on
	// one host each keep their own.
	Services(t Transport, d deployment.Deployment) (Section, error)
	// WatchdogModule plans loading a kernel module now, unless loaded says
	// it already is, and loading it at every boot.
	WatchdogModule(t Transport, d deployment.Deployment, module string, loaded bool) (Section, error)
	// Firewall plans the given inbound rules and, when hostWide, a default
	// deny for everything else and enabling the firewall. The SSH rule must
	// be in place before the firewall is enabled, and enabling must not
	// prompt. Rules it added earlier that are no longer given are removed,
	// last; rules it did not add are never removed or changed. What it added
	// is told apart by the owner tag of deployment d in each rule's comment,
	// so a rule another deployment added is as foreign as an operator's.
	// hostWide is false on a shared host, where the default policy and
	// whether the firewall is on decide other people's traffic too.
	Firewall(t Transport, d deployment.Deployment, rules []Rule, hostWide bool) (Section, error)
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

	d := cfg.Deployment()
	services, err := profile.Services(t, d)
	if err != nil {
		return nil, fmt.Errorf("services on %s: %w", t.Describe(), err)
	}
	plan.add(services)

	watchdog, err := planWatchdog(t, d, profile, declared)
	if err != nil {
		return nil, err
	}
	plan.add(watchdog)

	keys, keyRemovals, err := planAuthorizedKeys(t, d, declared.SSH)
	if err != nil {
		return nil, fmt.Errorf("authorized keys on %s: %w", t.Describe(), err)
	}
	plan.add(keys)

	firewall, err := profile.Firewall(t, d, append(Rules(d, declared), ContainerRules(cfg, site)...), !o.shared)
	if err != nil {
		return nil, fmt.Errorf("firewall on %s: %w", t.Describe(), err)
	}
	plan.add(firewall)

	// Last of all, after the firewall's own removals: a key is taken away
	// only once everything else, the listed keys included, is in place.
	plan.Steps = append(plan.Steps, keyRemovals...)

	return plan, nil
}

// Show reports the plan under the section the caller has opened for the
// site, marked the way apply's dry run marks its steps: each step still to
// run as pending under its title, with its sentence as a detail, then any
// warning. Under --verbose it first names the host profile and, after the
// steps, marks what was already there as done, what another hand put there
// as done and not paisans, so a plan with no steps still shows what was
// checked rather than a blank that might mean nothing was.
func (p *Plan) Show(r ui.Reporter) {
	r.Detail("host profile %s", p.Profile)
	for _, step := range p.Steps {
		s := r.Step(step.Title)
		s.Detail("%s", step.Describe)
		s.End(ui.Pending, "")
	}
	if r.Verbose() {
		for _, present := range p.Present {
			r.Step(present).End(ui.OK, "")
		}
		for _, foreign := range p.Foreign {
			r.Step(foreign).End(ui.OK, "not paisans")
		}
	}
	p.Warn(r)
}

// Warn reports the plan's warnings alone. They are reported whether or not
// anything is planned, and on --execute where the plan itself is not shown,
// because a prepared host that runs on softdog is still running on softdog.
func (p *Plan) Warn(r ui.Reporter) {
	for _, w := range p.Warnings {
		r.Warn(w.Hint, w.Detail)
	}
}

// Execute runs each step in order and stops at the first failure, naming the
// step, so an operator knows where a half finished run left the host. Every
// step is planned from a probe, so re-running Build after fixing the cause
// plans only what is still missing.
func Execute(plan *Plan, t Transport, r ui.Reporter) error {
	for _, step := range plan.Steps {
		s := r.Step(step.Title)
		s.Detail("%s", step.Describe)
		if step.File != nil {
			if err := t.WriteFile(step.File.Path, step.File.Content, step.File.Mode); err != nil {
				s.Fail(err)
				return fmt.Errorf("%s: %w", step.Describe, err)
			}
		}
		if step.Command != "" {
			if out, err := t.Run(step.Command); err != nil {
				s.Fail(err)
				return fmt.Errorf("%s: %w: %s", step.Describe, err, strings.TrimSpace(out))
			}
		}
		s.Done("")
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
