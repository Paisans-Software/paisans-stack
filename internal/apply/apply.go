package apply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Stack   string
	content string
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
	// Reason is quoted back to the operator, so that "why is it recreating"
	// never needs guessing.
	Reason string
}

// Plan is everything one site's apply would do.
type Plan struct {
	Site      string
	Transport string
	Changes   []Change
	Actions   []Action
	// GatewayReload is set when this site serves the public entry point and a
	// routing file changed.
	GatewayReload bool
	// ACMEModule is the Caddy DNS module this deployment's gateway must have,
	// as `caddy list-modules` prints it. Empty when this site runs no gateway.
	ACMEModule string
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

// Build decides what one site's apply would do, without doing any of it.
//
// The order matters and is the whole design. Every file is compared against
// both the rendered content and the manifest the last apply left behind, so a
// conflict is found before a single byte is written. An apply that wrote files
// as it discovered them could leave a stack half updated and then refuse.
func Build(site string, plan *render.Plan, acmeModule string, t Transport) (*Plan, error) {
	out := &Plan{Site: site, Transport: t.Describe()}

	recorded, err := readManifest(t)
	if err != nil {
		return nil, err
	}

	prefix := site + "/"
	stacks := map[string]bool{}
	envChanged := map[string]bool{}
	for _, file := range plan.Files {
		if !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		rel := strings.TrimPrefix(file.Path, prefix)
		if rel == render.ManifestName {
			continue
		}
		remote := remoteRoot + rel

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

		out.Changes = append(out.Changes, change)
		if change.Kind == Create || change.Kind == Update {
			if change.Stack != "" {
				stacks[change.Stack] = true
				if isEnvironment(rel) {
					envChanged[change.Stack] = true
				}
			}
			if isRouting(rel) {
				out.GatewayReload = true
			}
		}
	}

	for _, stack := range sortedKeys(stacks) {
		action := Action{Stack: stack}
		if envChanged[stack] {
			action.Recreate = true
			action.Reason = "an environment or compose file changed, and Compose passes environment at start, so a running container cannot be told about a new value"
		} else {
			action.Reason = "only bind mounted configuration changed, so the container keeps its identity"
		}
		out.Actions = append(out.Actions, action)
	}

	sort.Slice(out.Changes, func(i, j int) bool { return out.Changes[i].Path < out.Changes[j].Path })

	if out.GatewayReload {
		out.ACMEModule = acmeModule
	}

	return out, nil
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
			"%s: %d file(s) were edited on the host and would be overwritten:\n  %s\nRendered files are build artifacts and nothing edits them in place, so a difference here is a change somebody made on the machine. Copy what is wanted into the configuration, or delete the file on the host, then apply again",
			plan.Site, len(conflicts), strings.Join(names, "\n  "))
	}

	writes := plan.Writes()
	for _, change := range writes {
		if err := t.WriteFile(change.Path, change.content, change.Mode); err != nil {
			return err
		}
	}

	// The gateway's configuration is assembled from per app snippets, so a
	// wrong snippet is a wrong file for every hostname at once. Validate before
	// reloading, and refuse to reload what does not validate: the cost of a
	// mistake should be an error message on the workstation, not the public
	// address of every application.
	if plan.GatewayReload && plan.ACMEModule != "" {
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
		command := fmt.Sprintf(
			"docker compose -f /srv/infra/compose.yaml run --rm --no-deps --entrypoint caddy caddy list-modules | grep -qxF %s",
			shellQuote(plan.ACMEModule))
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf(
				"%s: the gateway's Caddy has no %s module, so it cannot serve this configuration and was not reloaded. The image it runs was built without that provider:\n%s",
				plan.Site, plan.ACMEModule, out)
		}
	}

	if plan.GatewayReload {
		if out, err := t.Run("docker compose -f /srv/infra/compose.yaml exec -T caddy caddy validate --config /etc/caddy/Caddyfile"); err != nil {
			return fmt.Errorf("%s: the assembled gateway configuration does not validate, so it was not reloaded:\n%s", plan.Site, out)
		}
		if _, err := t.Run("docker compose -f /srv/infra/compose.yaml exec -T caddy caddy reload --config /etc/caddy/Caddyfile"); err != nil {
			return fmt.Errorf("%s: reloading the gateway: %w", plan.Site, err)
		}
	}

	for _, action := range plan.Actions {
		command := fmt.Sprintf("docker compose -f /srv/%s/compose.yaml restart", action.Stack)
		if action.Recreate {
			command = fmt.Sprintf("docker compose -f /srv/%s/compose.yaml up -d", action.Stack)
		}
		if _, err := t.Run(command); err != nil {
			return err
		}
	}

	return writeManifest(plan, t)
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
func isEnvironment(rel string) bool {
	base := rel[strings.LastIndex(rel, "/")+1:]
	return base == ".env" || base == "compose.yaml"
}

// isRouting reports whether a file is part of the assembled gateway
// configuration.
func isRouting(rel string) bool {
	return strings.Contains(rel, "/caddy/")
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
