package apply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// Change is what one file needs, decided before anything is written.
type Change struct {
	// Path is where the file lands on the host, absolute.
	Path string
	// Kind says what will happen to it.
	Kind ChangeKind
	// Mode is the mode the rendered file wants.
	Mode uint32
	// Stack is the directory under /srv this file belongs to, empty for files
	// outside one.
	Stack string
	// Overwritten marks a conflict the operator named with --overwrite: a file
	// that differs from the last record and is replaced anyway, because they
	// said so for that one path.
	Overwritten bool
	content     string
}

// ChangeKind is what an apply will do to one file.
type ChangeKind int

const (
	// Create is a file the host does not have.
	Create ChangeKind = iota
	// Update is a file that differs from what is rendered, and matches what the
	// last apply recorded, so replacing it loses nothing.
	Update
	// Unchanged is a file already byte identical to what is rendered.
	Unchanged
	// Conflict is a file that differs from what the last apply recorded.
	// Somebody edited it on the host. Nothing is written and nothing is
	// restarted until that is resolved.
	Conflict
)

func (k ChangeKind) String() string {
	switch k {
	case Create:
		return "create"
	case Update:
		return "update"
	case Unchanged:
		return "unchanged"
	default:
		return "conflict"
	}
}

// Action is what has to happen after files are written for a change to be
// live. The narrower one is always preferred: a recreate is an outage, however
// brief, and a restart is not.
type Action struct {
	Stack string
	// Recreate means `docker compose up -d`, which replaces the container.
	// Only an environment or a compose change needs it, because Compose passes
	// environment at start and a running container cannot be told about a new
	// value.
	Recreate bool
	// Force means `docker compose up -d --force-recreate`, which replaces
	// every container of the stack even when Compose sees nothing changed. It
	// is set for a stack a stopped apply still owes and for one the operator
	// named with --recreate, never for an ordinary change: a plain `up -d`
	// over a container that an earlier `up -d` left half built only starts
	// it, network and all missing, because its configuration matches.
	Force bool
	// Reason is quoted back to the operator, so that "why is it recreating"
	// never needs guessing.
	Reason string
}

// Command is what the action runs on the host.
func (a Action) Command() string {
	switch {
	case a.Force:
		return fmt.Sprintf("docker compose -f /srv/%s/compose.yaml up -d --force-recreate", a.Stack)
	case a.Recreate:
		return fmt.Sprintf("docker compose -f /srv/%s/compose.yaml up -d", a.Stack)
	default:
		return fmt.Sprintf("docker compose -f /srv/%s/compose.yaml restart", a.Stack)
	}
}

// Plan is everything one site's apply would do.
type Plan struct {
	Site      string
	Transport string
	Changes   []Change
	Actions   []Action
	// GatewayChanging is set when this site runs the gateway and its Caddy is
	// about to change or be reloaded. That is the question every gate on the
	// gateway is really asking, and it has two answers rather than one:
	//
	//   - a routing file changed, so the running Caddy is told to reload
	//   - srv/infra/compose.yaml changed, so the container is replaced, which
	//     is how the image itself moves
	//
	// The second is the flagship workflow: a pull request bumps the Caddy
	// digest and merging it changes exactly that one file on the host. It is
	// an environment path rather than a routing one, so it yields an ordinary
	// `up -d` in the Actions loop, and `up -d` exits 0 as soon as the
	// container starts. A Caddy that cannot load its configuration dies a
	// moment later and the apply has already reported success. Checking only
	// before a reload would leave exactly that path unchecked.
	GatewayChanging bool
	// GatewayReload is set when this site serves the public entry point and a
	// routing file changed. It is the narrower of the two: a new image is
	// picked up by the recreate, not by a reload.
	GatewayReload bool
	// ACMEModule is the Caddy DNS module this deployment's gateway must have,
	// as `caddy list-modules` prints it. Empty when this site runs no gateway.
	ACMEModule string
	// WireGuard is what wg0 needs before any stack moves. Every service binds
	// the site's mesh address, so a stack started before the interface exists
	// fails to bind, and a container that cannot bind is restarted in a loop
	// by Docker rather than reported to the apply.
	WireGuard WireGuardStep
	// Bootstrap is the per app database work, nil when this site does none.
	// It runs after the infrastructure stack and before any app stack, and a
	// failure stops the apply there. See WithDatabases.
	Bootstrap *Bootstrap
	// Disk is the free space check on Docker's data root, nil when no stack
	// action will pull an image. Execute refuses before writing anything when
	// it is short. See DiskCheck.
	Disk *DiskCheck
	// Prunes are the images on the host that this apply's stacks supersede,
	// as Build saw them. Execute reads the host again after each stack is
	// healthy, and removes only what no container uses by then.
	Prunes []Prune
	// KeepImages skips pruning for this run, as --keep-images does.
	KeepImages bool
	// images is what each stack of this site renders, from its compose file.
	images map[string][]string
	// Progress receives what Execute decided along the way that is not an
	// error, such as a replica leaving the database work to the leader. Nil
	// discards it.
	Progress io.Writer
}

func (p *Plan) say(format string, args ...any) {
	if p.Progress != nil {
		fmt.Fprintf(p.Progress, format, args...)
	}
}

// WireGuardStep is the one thing an apply does to wg0.
type WireGuardStep int

const (
	// WireGuardNone means the interface is up and its file did not change.
	WireGuardNone WireGuardStep = iota
	// WireGuardStart enables the unit and starts it: a first apply, or an
	// interface found down.
	WireGuardStart
	// WireGuardSync hands the running interface its new peers without taking
	// it down, which is the ordinary update: a site joined or left.
	WireGuardSync
	// WireGuardRestart takes the interface down and up again. Only a change to
	// a line that wg-quick itself applies needs it, because `wg syncconf`
	// never sees those lines.
	WireGuardRestart
)

// Command is what the step runs on the host, empty for WireGuardNone.
func (w WireGuardStep) Command() string {
	switch w {
	case WireGuardStart:
		return "systemctl enable --now " + wireguardUnit
	case WireGuardSync:
		// Through a temporary file rather than a pipe. /bin/sh has no
		// pipefail, so a `wg-quick strip` that failed would hand syncconf an
		// empty configuration, and syncconf removes every peer it is not
		// given: one failed command would cut this site off the mesh.
		return "set -e; f=$(mktemp); trap 'rm -f \"$f\"' EXIT; wg-quick strip wg0 > \"$f\"; wg syncconf wg0 \"$f\""
	case WireGuardRestart:
		return "systemctl restart " + wireguardUnit
	default:
		return ""
	}
}

// Describe says what the step does, for a plan.
func (w WireGuardStep) Describe() string {
	switch w {
	case WireGuardStart:
		return "start wg0 and enable it at boot, before any stack moves"
	case WireGuardSync:
		return "give wg0 its new peers in place, without taking the mesh down"
	case WireGuardRestart:
		return "restart wg0, because a line only wg-quick applies changed"
	default:
		return ""
	}
}

// Conflicts returns the files somebody edited on the host.
func (p Plan) Conflicts() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind == Conflict {
			out = append(out, c)
		}
	}
	return out
}

// Writes returns the files this plan would write.
func (p Plan) Writes() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind == Create || c.Kind == Update {
			out = append(out, c)
		}
	}
	return out
}

// remoteRoot is where a site's tree lands. The rendered tree is already shaped
// like the host, so a path under it is the path on the host.
const remoteRoot = "/"

// manifestPath is where the last apply's record lives on the host. It is what
// separates "this file changed because we changed it" from "somebody edited
// this on the host", and without it every apply would be a blind overwrite.
const manifestPath = "/srv/.paisans-manifest.json"

// gatewayCaddyfile and gatewayCompose are the two rendered files that say a
// site runs the gateway and that its Caddy is about to be replaced, as paths
// relative to a site's root in the rendered tree.
//
// gatewayCompose is the whole infrastructure stack's compose file rather than
// a Caddy specific one: the gateway shares it with etcd and Patroni, so a
// change to it may or may not be the Caddy image. The check is cheap and
// wrongly running it costs one container start, while wrongly skipping it
// costs the public address of every application.
const (
	gatewayCaddyfile = "srv/infra/caddy/Caddyfile"
	gatewayCompose   = "srv/infra/compose.yaml"
)

// wireguardConfig is the mesh interface's file, relative to a site's root, and
// wireguardUnit the systemd unit wg-quick ships to bring it up from it.
const (
	wireguardConfig = "etc/wireguard/wg0.conf"
	wireguardUnit   = "wg-quick@wg0"
)

// wireguardProbe asks whether wg0 exists. It reads the kernel rather than the
// unit, because an interface somebody brought up with a bare `wg-quick up` is
// up for every service binding to it, and `systemctl enable --now` on top of
// it would fail on an interface that already exists. It prints rather than
// exits non zero, so that "down" cannot be confused with ssh failing.
const wireguardProbe = "if ip link show wg0 >/dev/null 2>&1; then echo up; else echo down; fi"

// Option adjusts what Build plans, on the operator's word.
type Option func(*options)

type options struct {
	overwrite  []string
	recreate   []string
	minFree    int64
	minFreeSet bool
	keepImages bool
}

// KeepImages leaves superseded images on the host for this run, as
// --keep-images does, for an operator who wants the old image to roll back to.
func KeepImages() Option {
	return func(o *options) { o.keepImages = true }
}

// MinFree sets the free space an apply needs on Docker's data root before it
// pulls an image, as --min-free does. The default is DefaultMinFree.
func MinFree(bytes int64) Option {
	return func(o *options) { o.minFree, o.minFreeSet = bytes, true }
}

// Overwrite names conflicting files that may be replaced anyway, one path
// each, as --overwrite does.
func Overwrite(paths ...string) Option {
	return func(o *options) { o.overwrite = append(o.overwrite, paths...) }
}

// Recreate names stacks to force-recreate although nothing about them
// changed, as --recreate does.
func Recreate(stacks ...string) Option {
	return func(o *options) { o.recreate = append(o.recreate, stacks...) }
}

// Build decides what one site's apply would do, without doing any of it.
//
// The order matters and is the whole design. Every file is compared against
// both the rendered content and the manifest the last apply left behind, so a
// conflict is found before a single byte is written. An apply that wrote files
// as it discovered them could leave a stack half updated and then refuse.
func Build(site string, plan *render.Plan, acmeModule string, t Transport, opts ...Option) (*Plan, error) {
	out := &Plan{Site: site, Transport: t.Describe()}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	overwrite := o.overwrite

	// Paths the operator has said may be replaced although they conflict.
	// Each must name a file that really is a conflict: a path that is not one
	// is refused below rather than ignored, so a typo cannot pass for consent.
	named := map[string]bool{}
	for _, path := range overwrite {
		named[path] = true
	}
	used := map[string]bool{}

	recorded, err := readManifest(t)
	if err != nil {
		return nil, err
	}

	prefix := site + "/"
	stacks := map[string]bool{}
	// Every stack this site renders, changed or not, which is what a
	// --recreate may name.
	rendered := map[string]bool{}
	envChanged := map[string]bool{}
	// Whether this site runs the gateway at all, and which of the two ways its
	// Caddy is about to change. render only emits a Caddyfile for a site
	// holding the gateway role, so its presence in the rendered tree is the
	// signal, regardless of whether it changed: an image only apply changes no
	// routing file at all.
	var isGateway, routingChanged, gatewayComposeChanged bool
	// The mesh file's fate, and what was on the host before, which decides
	// whether its change can be applied in place.
	var wireguard *Change
	var wireguardBefore string
	for _, file := range plan.Files {
		if !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		rel := strings.TrimPrefix(file.Path, prefix)
		if rel == render.ManifestName {
			continue
		}
		remote := remoteRoot + rel

		if rel == gatewayCaddyfile {
			isGateway = true
		}

		change := Change{
			Path:    remote,
			Mode:    file.Mode,
			Stack:   stackOf(rel),
			content: file.Content,
		}

		current, found, err := t.ReadFile(remote)
		if err != nil {
			return nil, err
		}
		switch {
		case !found:
			change.Kind = Create
		case current == file.Content:
			change.Kind = Unchanged
		case recorded[rel] == "":
			// Present on the host, different from what we render, and no
			// record of us having written it. That is somebody else's file,
			// and overwriting it is exactly what this gate exists to stop.
			change.Kind = Conflict
		case recorded[rel] != sum(current):
			change.Kind = Conflict
		default:
			change.Kind = Update
		}
		if change.Kind == Conflict && named[remote] {
			change.Kind = Update
			change.Overwritten = true
			used[remote] = true
		}

		if rel == wireguardConfig {
			copied := change
			wireguard = &copied
			wireguardBefore = current
		}

		if change.Stack != "" {
			rendered[change.Stack] = true
		}
		out.Changes = append(out.Changes, change)
		if (change.Kind == Create || change.Kind == Update) && !isRecord(rel) {
			if change.Stack != "" {
				stacks[change.Stack] = true
				if isEnvironment(rel) {
					envChanged[change.Stack] = true
				}
			}
			if isRouting(rel) {
				routingChanged = true
			}
			// The compose file moves the image; an environment file under
			// infra moves what the replaced container starts with. Both
			// replace the gateway, so both need the gates.
			if rel == gatewayCompose || (change.Stack == infraStack && isEnvironment(rel)) {
				gatewayComposeChanged = true
			}
		}
	}

	for _, path := range overwrite {
		if !used[path] {
			return nil, fmt.Errorf("%s: --overwrite %s names no conflicting file. Only a file this site renders, and which differs from what the last apply recorded, can be overwritten", site, path)
		}
	}

	resumed, err := readPending(t)
	if err != nil {
		return nil, err
	}
	// A stack the record still owes is force-recreated, whether or not its
	// files changed again since. The record says its last action did not
	// finish, and an `up -d` that stopped part way can leave a container
	// created and never attached to its network; a plain `up -d` or a restart
	// would start that container as it is.
	owed := map[string]bool{}
	for _, action := range resumed.Actions {
		owed[action.Stack] = true
		stacks[action.Stack] = true
	}

	forced := map[string]bool{}
	for _, stack := range o.recreate {
		if !rendered[stack] {
			return nil, fmt.Errorf("%s: --recreate %s names no stack this site renders. Its stacks are %s", site, stack, strings.Join(sortedKeys(rendered), ", "))
		}
		forced[stack] = true
		stacks[stack] = true
	}

	for _, stack := range stackOrder(stacks) {
		action := Action{Stack: stack}
		if owed[stack] {
			action.Recreate, action.Force = true, true
			action.Reason = "a previous apply stopped before this stack's action finished, so its containers may be half built and are replaced outright"
		} else if forced[stack] {
			action.Recreate, action.Force = true, true
			action.Reason = "named with --recreate, so every container is replaced whether or not anything changed"
		} else if envChanged[stack] {
			action.Recreate = true
			action.Reason = "an environment or compose file changed, and Compose passes environment at start, so a running container cannot be told about a new value"
		} else {
			action.Reason = "only bind mounted configuration changed, so the container keeps its identity"
		}
		out.Actions = append(out.Actions, action)
	}

	sort.Slice(out.Changes, func(i, j int) bool { return out.Changes[i].Path < out.Changes[j].Path })

	need := DefaultMinFree
	if o.minFreeSet {
		need = o.minFree
	}
	out.KeepImages = o.keepImages
	if err := out.probeImages(need, t); err != nil {
		return nil, fmt.Errorf("%s: %w", site, err)
	}

	out.GatewayReload = isGateway && (routingChanged || resumed.GatewayReload)
	out.GatewayChanging = isGateway && (routingChanged || gatewayComposeChanged || resumed.GatewayChanging)
	if out.GatewayChanging {
		out.ACMEModule = acmeModule
	}

	if wireguard != nil {
		step, err := wireguardStep(*wireguard, wireguardBefore, t)
		if err != nil {
			return nil, err
		}
		out.WireGuard = step
	}

	return out, nil
}

// wireguardStep decides what wg0 needs. Reading the host here is a probe and
// changes nothing, so a dry run can show it.
func wireguardStep(change Change, before string, t Transport) (WireGuardStep, error) {
	switch change.Kind {
	case Create:
		return WireGuardStart, nil
	case Conflict:
		// Execute refuses before any step runs.
		return WireGuardNone, nil
	}
	out, err := t.Run(wireguardProbe)
	if err != nil {
		return WireGuardNone, fmt.Errorf("asking whether wg0 is up: %w", err)
	}
	if strings.TrimSpace(out) != "up" {
		return WireGuardStart, nil
	}
	if change.Kind == Unchanged {
		return WireGuardNone, nil
	}
	if wgQuickLines(before) != wgQuickLines(change.content) {
		return WireGuardRestart, nil
	}
	return WireGuardSync, nil
}

// wgQuickOnly are the [Interface] keys wg-quick applies itself and `wg-quick
// strip` removes, so `wg syncconf` never sees a change to one
// (wireguard-tools, src/wg-quick/linux.bash, parse_options).
var wgQuickOnly = map[string]bool{
	"address": true, "dns": true, "mtu": true, "table": true, "saveconfig": true,
	"preup": true, "postup": true, "predown": true, "postdown": true,
}

// wgQuickLines returns the wg-quick only lines of a configuration, in order,
// as one comparable string.
func wgQuickLines(content string) string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if wgQuickOnly[strings.ToLower(strings.TrimSpace(key))] {
			out = append(out, strings.ToLower(strings.TrimSpace(key))+"="+strings.TrimSpace(value))
		}
	}
	return strings.Join(out, "\n")
}

// Execute carries out a plan. It refuses outright if anything conflicts,
// because a partial apply across a stack is worse than no apply.
func Execute(plan *Plan, t Transport) error {
	if conflicts := plan.Conflicts(); len(conflicts) > 0 {
		var names []string
		for _, c := range conflicts {
			names = append(names, c.Path)
		}
		return fmt.Errorf(
			"%s: %d file(s) were edited on the host and would be overwritten:\n  %s\nRendered files are build artifacts and nothing edits them in place, so a difference here is a change somebody made on the machine. Copy what is wanted into the configuration and apply again, or, once you have looked at a file and want the rendered one, name it with --overwrite <path>",
			plan.Site, len(conflicts), strings.Join(names, "\n  "))
	}

	// A pull that fills the disk fails part way through a stack's action,
	// with the old containers stopped and the new image half written, and it
	// takes the database and every log down with it. Refusing here, before the
	// first write, leaves the host exactly as it was.
	if plan.Disk.Short() {
		return diskRefusal(plan)
	}

	// Record what this apply owes before writing anything. Files that land
	// before a gate stops the apply already match the render, so without
	// this record the next apply would see nothing to do, and an app stack
	// held back by a failed gate would stay down until something unrelated
	// changed it.
	owes := len(plan.Actions) > 0 || plan.GatewayChanging
	if owes {
		if err := writePending(plan, plan.Actions, t); err != nil {
			return err
		}
	}

	writes := plan.Writes()
	for _, change := range writes {
		if err := t.WriteFile(change.Path, change.content, change.Mode); err != nil {
			return err
		}
	}

	// Record the files as soon as they are on the host, not only at the end.
	// They are this apply's files whether or not a later gate or action
	// succeeds, and a manifest written only on success turned every file of a
	// failed first apply into "somebody else's": the next apply, carrying a
	// fix to one of them, refused it as a host edit. Owed actions are tracked
	// separately, in the pending record above.
	if len(writes) > 0 {
		if err := writeManifest(plan, t); err != nil {
			return err
		}
	}

	// The mesh comes up before anything that binds to it. It runs before the
	// gateway gates as well, which need no mesh themselves, so that the one
	// rule is simple: no container is started or checked on a site whose
	// interface is down.
	if command := plan.WireGuard.Command(); command != "" {
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf("%s: bringing up wg0, so nothing that binds the mesh address was started:\n%s", plan.Site, out)
		}
	}

	// The gateway's configuration is assembled from per app snippets, so a
	// wrong snippet is a wrong file for every hostname at once. Check the
	// binary and the configuration before the gateway changes at all, and
	// refuse rather than proceed: the cost of a mistake should be an error
	// message on the workstation, not the public address of every application.
	//
	// These gates run on GatewayChanging rather than on GatewayReload, and
	// both run before the Actions loop below, because the loop is where an
	// image change lands. `docker compose up -d` returns as soon as the
	// container starts, so a Caddy that cannot load its configuration is
	// reported as a successful apply and is discovered by whoever visits the
	// site.
	if plan.GatewayChanging && plan.ACMEModule != "" {
		// Ask the binary rather than trusting the image's name. A DNS provider
		// is compiled into Caddy, so a wrong image or a provider no module
		// answers to both produce a gateway that cannot load its own
		// configuration, and neither is visible in a reference string. This
		// runs before validate on purpose: a binary without the module also
		// fails to validate, but the missing module error says what to fix and
		// a parse error does not.
		// The identifier is shell quoted and matched as a fixed string. It is
		// no longer a compile time literal: acme.Module derives one from
		// acme.provider for any provider the toolkit publishes no image for,
		// so the operator's configuration reaches this command line. Single
		// quoting keeps it an argument rather than shell syntax, and -F keeps
		// it a literal rather than a pattern whose dots match anything.
		// Pull first, on its own. The module check below runs the image, and a
		// run whose pull fails exits non zero exactly like a binary without
		// the module: on the first real gateway a private image's
		// "unauthorized" was reported as a missing DNS provider.
		if out, err := t.Run("docker compose -f /srv/infra/compose.yaml pull caddy"); err != nil {
			return fmt.Errorf(
				"%s: the gateway's Caddy image could not be pulled, so nothing was changed. If the registry answered unauthorized or denied, the image is private: make it public, or log the host in to that registry:\n%s",
				plan.Site, out)
		}
		command := fmt.Sprintf(
			"docker compose -f /srv/infra/compose.yaml run --rm --no-deps --entrypoint caddy caddy list-modules | grep -qxF %s",
			shellQuote(plan.ACMEModule))
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf(
				"%s: the gateway's Caddy has no %s module, so it cannot serve this configuration and was not reloaded. The image it runs was built without that provider:\n%s",
				plan.Site, plan.ACMEModule, out)
		}
	}

	if plan.GatewayChanging {
		// `run --rm --no-deps`, the same shape as the module check, rather than
		// `exec`. Nothing has started the infrastructure stack at this point:
		// the Actions loop below is what does that, and on a first apply to a
		// fresh host there is no container to exec into at all, so validating
		// through exec made `apply` unable to complete a first install of a
		// gateway site. It also made recovery impossible after a gateway died,
		// since every later apply would refuse at this step. `run` starts a
		// throwaway container with the service's own image and bind mounts,
		// which is exactly what validation needs and needs nothing running.
		if out, err := t.Run("docker compose -f /srv/infra/compose.yaml run --rm --no-deps --entrypoint caddy caddy validate --config /etc/caddy/Caddyfile"); err != nil {
			return fmt.Errorf("%s: the assembled gateway configuration does not validate, so the gateway was not changed:\n%s", plan.Site, out)
		}
	}

	if plan.GatewayReload {
		// Reload only a Caddy that is running. A stopped or absent gateway is
		// started by the Actions loop below instead, and it reads the same
		// configuration this apply just validated, so nothing is skipped by
		// not reloading it.
		running, err := t.Run("docker compose -f /srv/infra/compose.yaml ps --status running --quiet caddy")
		if err != nil {
			return fmt.Errorf("%s: asking whether the gateway is running: %w", plan.Site, err)
		}
		if strings.TrimSpace(running) != "" {
			if _, err := t.Run("docker compose -f /srv/infra/compose.yaml exec -T caddy caddy reload --config /etc/caddy/Caddyfile"); err != nil {
				return fmt.Errorf("%s: reloading the gateway: %w", plan.Site, err)
			}
		}
	}

	// App stacks start only after their databases exist. An app started
	// first connects, fails to authenticate as a role nobody created, and is
	// restarted in a loop by Docker while the apply reports success.
	bootstrapped := plan.Bootstrap == nil
	for i, action := range plan.Actions {
		if action.Stack != infraStack && !bootstrapped {
			if err := runBootstrap(plan, t); err != nil {
				return err
			}
			bootstrapped = true
		}
		if _, err := t.Run(action.Command()); err != nil {
			return err
		}
		// `up -d` and `restart` return once the containers start, which says
		// nothing about whether they stay up or pass their own checks. The
		// stack stays in the pending record until this passes, so a failure
		// here is resumed, and force-recreated, by the next apply.
		if err := waitHealthy(plan, action.Stack, t); err != nil {
			return err
		}
		// Only now, with the stack healthy on its new image, is the old one
		// safe to lose. Before the gate it is the image a rollback would use.
		if !plan.KeepImages {
			pruneStack(plan, action.Stack, t)
		}
		// The stack is done, so it leaves the record. Left in, a later stack
		// failing would have the next apply force-recreate this one too, an
		// outage for a stack that was fine. The last stack stays until the
		// end, so that a bootstrap failing after it still leaves the record
		// owing something and the next apply comes back to the bootstrap.
		if i < len(plan.Actions)-1 {
			if err := writePending(plan, plan.Actions[i+1:], t); err != nil {
				return err
			}
		}
	}

	if !bootstrapped {
		if err := runBootstrap(plan, t); err != nil {
			return err
		}
	}

	if err := writeManifest(plan, t); err != nil {
		return err
	}
	if owes {
		if _, err := t.Run("rm -f " + shellQuote(pendingPath)); err != nil {
			return fmt.Errorf("%s: everything was applied, but the record of owed actions could not be removed, so the next apply will repeat them: %w", plan.Site, err)
		}
	}
	return nil
}

// pendingPath records what an apply has written but not yet acted on. It
// exists only between the start of an Execute and its successful end.
const pendingPath = "/srv/.paisans-pending.json"

// pending is what one apply owes the host. It holds no file content and no
// credential, only stack names and which gates to run.
type pending struct {
	Version         int             `json:"version"`
	Actions         []pendingAction `json:"actions"`
	GatewayChanging bool            `json:"gateway_changing,omitempty"`
	GatewayReload   bool            `json:"gateway_reload,omitempty"`
}

type pendingAction struct {
	Stack    string `json:"stack"`
	Recreate bool   `json:"recreate,omitempty"`
}

func readPending(t Transport) (pending, error) {
	content, found, err := t.ReadFile(pendingPath)
	if err != nil || !found {
		return pending{}, err
	}
	var p pending
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return pending{}, fmt.Errorf("%s is not readable: %w\nIt records actions a stopped apply still owes. Delete it and every stack will be acted on only when its files next change", pendingPath, err)
	}
	return p, nil
}

func writePending(plan *Plan, actions []Action, t Transport) error {
	p := pending{Version: 1, GatewayChanging: plan.GatewayChanging, GatewayReload: plan.GatewayReload}
	for _, action := range actions {
		p.Actions = append(p.Actions, pendingAction{Stack: action.Stack, Recreate: action.Recreate})
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return t.WriteFile(pendingPath, string(data)+"\n", 0o600)
}

// infraStack is the site's own infrastructure: etcd, Patroni, HAProxy, Garage
// and the gateway. Every app stack depends on it.
const infraStack = "infra"

// stackOrder puts the infrastructure stack first and the rest in sorted
// order. Sorting alone put "blog" and "docs" ahead of "infra", which started
// applications before the database and proxy they connect to.
func stackOrder(stacks map[string]bool) []string {
	var out []string
	if stacks[infraStack] {
		out = append(out, infraStack)
	}
	for _, stack := range sortedKeys(stacks) {
		if stack != infraStack {
			out = append(out, stack)
		}
	}
	return out
}

// readManifest returns what the last apply recorded, keyed by path relative to
// the site root. A host with no manifest is a first apply, which is ordinary.
func readManifest(t Transport) (map[string]string, error) {
	content, found, err := t.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]string{}, nil
	}
	var m render.Manifest
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return nil, fmt.Errorf("%s is not readable as a manifest: %w\nIt records what the last apply wrote. Delete it to treat every file on this host as somebody else's, which is the safe reading", manifestPath, err)
	}
	out := make(map[string]string, len(m.Files))
	for _, file := range m.Files {
		out[file.Path] = file.SHA256
	}
	return out, nil
}

// writeManifest records what is now on the host, so the next apply can tell its
// own writes from somebody's edit.
func writeManifest(plan *Plan, t Transport) error {
	var files []render.ManifestFile
	for _, change := range plan.Changes {
		if change.Kind == Conflict {
			continue
		}
		files = append(files, render.ManifestFile{
			Path:   strings.TrimPrefix(change.Path, remoteRoot),
			SHA256: sum(change.content),
			Mode:   fmt.Sprintf("%04o", change.Mode),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	data, err := json.MarshalIndent(render.Manifest{Version: 1, Files: files}, "", "  ")
	if err != nil {
		return err
	}
	return t.WriteFile(manifestPath, string(data)+"\n", 0o600)
}

// stackOf returns the stack directory a rendered path belongs to, empty when it
// belongs to none. `srv/talk/.env` is talk; `etc/wireguard/wg0.conf` is not a
// stack at all.
func stackOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 3 || parts[0] != "srv" {
		return ""
	}
	return parts[1]
}

// isEnvironment reports whether a change to this file needs the container
// replaced rather than restarted.
//
// Any `*.env` counts, not only a file named `.env`: patroni.env and
// caddy/caddy.env reach their containers through `env_file`, which Compose
// reads when it creates a container, so a restart keeps the old values. Every
// env_file the templates render ends in .env, which is what makes the suffix
// a sufficient test; parsing each compose file for its env_file list was the
// alternative, and it answers the same question with a YAML parser in the
// path of every apply.
func isEnvironment(rel string) bool {
	base := rel[strings.LastIndex(rel, "/")+1:]
	return strings.HasSuffix(base, ".env") || base == "compose.yaml"
}

// isRouting reports whether a file is part of the assembled gateway
// configuration. An environment file beside the Caddyfile is not: a reload
// rereads the Caddyfile and never the container's environment, so treating
// caddy.env as routing reloaded a Caddy that still held the old DNS token.
func isRouting(rel string) bool {
	return strings.Contains(rel, "/caddy/") && !isEnvironment(rel)
}

// isRecord reports whether a file is a record the toolkit keeps for itself
// beside a stack, which no container mounts or reads. Writing one changes
// nothing that runs, so it is never a reason to restart or recreate the
// stack: on a deployment applied before the etcd record existed, treating its
// first write as a change restarted etcd and Patroni for a file neither reads.
func isRecord(rel string) bool {
	return rel == render.EtcdInitialPath
}

func sum(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
