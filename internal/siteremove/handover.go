package siteremove

import (
	"fmt"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// What follows hands a leaving gateway's Caddy to the host's owner, when
// something that is not this deployment's relies on it (README, "The gateway
// host's own sites live in /srv/caddy.d"). The handed over Caddy lives in
// handoverDir, with no deployment label, and from the moment markerPath is
// written paisans never touches that directory again.

const (
	handoverDir = "/srv/caddy"
	markerPath  = handoverDir + "/HANDED-OVER"
	// handoverProject is the compose project of the handed over Caddy.
	handoverProject = "caddy"
)

// handoverSnippets are the snippets the deployment's Caddyfile defines that
// a file in /srv/caddy.d may import, carried into the handed over Caddyfile
// so those files still validate.
var handoverSnippets = []string{"upstream_unavailable", "upstream_failover", "upstream_single"}

// handover is the plan for one gateway's Caddy.
type handover struct {
	// done is a marker recording this deployment: the hand over finished on
	// an earlier run.
	done bool
	// users are what relies on the Caddy, for the plan.
	users     []string
	image     string
	compose   string
	caddyfile string
	// env is whether this deployment's Caddy has a DNS provider token on the
	// host, copied there for the owner.
	env bool
}

func (p *Plan) caddyDir() string { return p.dep().Path("infra", "caddy") }

func (p *Plan) caddyEnv() string { return p.dep().Path("infra", "caddy", "caddy.env") }

func handoverCompose() string { return "docker compose -f " + handoverDir + "/compose.yaml" }

// caddyImage is the image the deployment's Caddy runs, as render chooses it.
func (p *Plan) caddyImage() (string, error) {
	if p.cfg.ACME.Image != "" {
		return p.cfg.ACME.Image, nil
	}
	image, ok := acme.Image(p.cfg.ACME.Provider)
	if !ok {
		return "", fmt.Errorf("acme.provider %q has no image and acme.image declares none", p.cfg.ACME.Provider)
	}
	return image, nil
}

// snippetBlocks copies the named snippets out of a rendered Caddyfile: each
// from its `(name) {` line to the `}` that closes it at the start of a line.
func snippetBlocks(caddyfile string, names []string) (string, error) {
	lines := strings.Split(caddyfile, "\n")
	var out []string
	for _, name := range names {
		start := -1
		for i, l := range lines {
			if l == "("+name+") {" {
				start = i
			}
		}
		if start < 0 {
			return "", fmt.Errorf("the rendered Caddyfile defines no snippet %s", name)
		}
		end := -1
		for i := start + 1; i < len(lines); i++ {
			if lines[i] == "}" {
				end = i
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("the rendered Caddyfile's snippet %s has no closing brace", name)
		}
		out = append(out, strings.Join(lines[start:end+1], "\n"))
	}
	return strings.Join(out, "\n\n") + "\n", nil
}

func (p *Plan) handoverHeader() string {
	return fmt.Sprintf(`# Written by paisans site remove when deployment %s (%s) left this
# host, whose gateway served the site blocks in %s. This Caddy carries on
# serving them. It belongs to the host's owner: paisans never reads or
# changes this directory again.
`, p.cfg.ID, p.cfg.Community.Domain, render.HostSitesDir)
}

// handoverFiles renders the handed over Caddy's compose file and Caddyfile.
func (p *Plan) handoverFiles(h *handover) error {
	var env string
	directive := ""
	if h.env {
		env = "    env_file:\n      - caddy.env\n"
		directive = acme.Directive(p.cfg.ACME.Provider) + "\n"
	}
	h.compose = p.handoverHeader() + fmt.Sprintf(`name: %s

services:
  caddy:
    image: %s
    restart: unless-stopped
    network_mode: host
%s    volumes:
      - %s/Caddyfile:/etc/caddy/Caddyfile:ro
      - %s:%s:ro
      - %s/data:/data
      - %s/config:/config
`, handoverProject, h.image, env, handoverDir, render.HostSitesDir, render.HostSitesMount, handoverDir, handoverDir)

	rendered := ""
	for _, f := range p.full.Files {
		if f.Path == p.Site+"/"+p.dep().RelPath("infra", "caddy", "Caddyfile") {
			rendered = f.Content
		}
	}
	snippets, err := snippetBlocks(rendered, handoverSnippets)
	if err != nil {
		return err
	}
	h.caddyfile = p.handoverHeader() + fmt.Sprintf(`{
	email admin@%s
%s}

# Snippets a site block below may import, as the deployment's Caddyfile
# defined them.
%s
import %s/*.caddy
`, p.cfg.Community.Domain, directive, snippets, render.HostSitesMount)
	return nil
}

// handoverProbe lists what handoverDir holds and which of the deployment's
// Caddy directories are still where they were, and whether its token file
// exists, without reading the token.
func (p *Plan) handoverProbe() string {
	return fmt.Sprintf(`if [ -d %[1]s ]; then ls -A %[1]s | sed 's/^/dst /'; fi; for x in data config; do [ -d %[2]s/$x ] && echo "src $x"; done; [ -f %[3]s ] && echo env; true`,
		quote(handoverDir), quote(p.caddyDir()), quote(p.caddyEnv()))
}

// probeHandover plans the hand over, or refuses one that would take over
// something already there.
func (p *Plan) probeHandover(t apply.Transport, inv *hostcheck.Inventory, report ownership.Report) (*handover, error) {
	h := &handover{users: report.ForeignLines()}
	marker, found, err := t.ReadFile(markerPath)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: reading %s: %w", p.Site, markerPath, err)
	}
	if found {
		if !strings.Contains(marker, "deployment: "+p.cfg.ID+"\n") {
			return nil, fmt.Errorf("site remove %s: %s records a hand over by another deployment, so this gateway's Caddy cannot be handed over there. Nothing was changed", p.Site, markerPath)
		}
		h.done = true
		p.Kept = append(p.Kept, p.handoverKept(h)...)
		return h, nil
	}
	for _, c := range inv.Containers {
		if c.Project == handoverProject && c.Deployment == "" {
			return nil, fmt.Errorf("site remove %s: container %s already belongs to a compose project named %s, which the handed over Caddy would take. Nothing was changed", p.Site, c.Name, handoverProject)
		}
	}
	if h.image, err = p.caddyImage(); err != nil {
		return nil, err
	}
	out, err := t.Run(p.handoverProbe())
	if err != nil {
		return nil, fmt.Errorf("site remove %s: reading %s: %w: %s", p.Site, handoverDir, err, lastLines(out, 2))
	}
	dst, src := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		kind, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case "dst":
			dst[name] = true
		case "src":
			src[name] = true
		case "env":
			h.env = true
		}
	}
	if err := p.handoverFiles(h); err != nil {
		return nil, err
	}
	for name := range dst {
		switch name {
		case "compose.yaml", "Caddyfile":
			have, _, err := t.ReadFile(handoverDir + "/" + name)
			if err != nil {
				return nil, err
			}
			want := h.compose
			if name == "Caddyfile" {
				want = h.caddyfile
			}
			if have != want {
				return nil, fmt.Errorf("site remove %s: %s/%s is there and is not what the hand over writes, so it is somebody else's. Nothing was changed. Move it aside, or hand Caddy over yourself", p.Site, handoverDir, name)
			}
		case "caddy.env":
		case "data", "config":
			if src[name] {
				return nil, fmt.Errorf("site remove %s: both %s/%s and %s/%s exist, so which holds the certificates is not clear. Nothing was changed", p.Site, handoverDir, name, p.caddyDir(), name)
			}
		default:
			return nil, fmt.Errorf("site remove %s: %s holds %s, which the hand over does not write, so the directory is somebody else's. Nothing was changed", p.Site, handoverDir, name)
		}
	}
	p.Kept = append(p.Kept, p.handoverKept(h)...)
	return h, nil
}

func (p *Plan) handoverKept(h *handover) []string {
	return handoverKept(p.Site, handoverDir, h.env)
}

// handOver runs the hand over: write, validate in a one-off container, stop
// this deployment's Caddy, move its certificates, start the new one, gate,
// and record it. A failed gate puts everything back and starts this
// deployment's Caddy again.
func (p *Plan) handOver(t apply.Transport, h *handover) error {
	run := func(what, command string) error {
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf("%s: hand over: %s: %w: %s", p.Site, what, err, lastLines(out, 5))
		}
		return nil
	}
	p.work("hand over Caddy").Detail("Caddy to %s", handoverDir)
	if err := t.WriteFile(handoverDir+"/compose.yaml", h.compose, 0o644); err != nil {
		return err
	}
	if err := t.WriteFile(handoverDir+"/Caddyfile", h.caddyfile, 0o644); err != nil {
		return err
	}
	envFlag := ""
	if h.env {
		if err := run("copying the token", fmt.Sprintf("install -m 600 %s %s/caddy.env", quote(p.caddyEnv()), handoverDir)); err != nil {
			return err
		}
		envFlag = " --env-file " + handoverDir + "/caddy.env"
	}
	validate := fmt.Sprintf("docker run --rm --network none%s -v %s/Caddyfile:/etc/caddy/Caddyfile:ro -v %s:%s:ro %s caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile",
		envFlag, handoverDir, render.HostSitesDir, render.HostSitesMount, quote(h.image))
	if err := run("validating the handed over configuration before anything stops, so nothing was stopped", validate); err != nil {
		return err
	}
	infra := p.dep().Compose("infra")
	if err := run("stopping this deployment's Caddy", fmt.Sprintf("if [ -f %s ]; then %s stop caddy; fi", quote(infra), p.dep().ComposeCmd("infra"))); err != nil {
		return err
	}
	if err := run("moving the certificates", fmt.Sprintf(`set -e; for x in data config; do if [ -d %[1]s/$x ] && [ ! -e %[2]s/$x ]; then mv %[1]s/$x %[2]s/$x; fi; done`, quote(p.caddyDir()), handoverDir)); err != nil {
		return p.handoverBack(t, err)
	}
	if err := run("starting the handed over Caddy", handoverCompose()+" up -d"); err != nil {
		return p.handoverBack(t, err)
	}
	if err := poll(handoverWait, handoverPoll, func() error { return p.handoverRunning(t) }); err != nil {
		return p.handoverBack(t, fmt.Errorf("the handed over Caddy did not come up within %s: %w", handoverWait, err))
	}
	marker := fmt.Sprintf("deployment: %s\ndomain: %s\nsite: %s\nhanded_over_at: %s\n", p.cfg.ID, p.cfg.Community.Domain, p.Site, now().UTC().Format(time.RFC3339))
	if err := t.WriteFile(markerPath, marker, 0o644); err != nil {
		return err
	}
	return nil
}

// handoverRunning is the hand over's gate: the container runs and its
// configuration validates inside it.
func (p *Plan) handoverRunning(t apply.Transport) error {
	out, err := t.Run(handoverCompose() + " ps --status running --quiet caddy")
	if err != nil {
		return fmt.Errorf("%w: %s", err, lastLines(out, 2))
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("its container is not running")
	}
	if out, err := t.Run(handoverCompose() + " exec -T caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile"); err != nil {
		return fmt.Errorf("caddy validate: %w: %s", err, lastLines(out, 3))
	}
	return nil
}

// handoverBack undoes a hand over whose gate failed: the new Caddy down, the
// certificates back, this deployment's Caddy started again.
func (p *Plan) handoverBack(t apply.Transport, cause error) error {
	back := fmt.Sprintf(`%[3]s down >/dev/null 2>&1 || true; for x in data config; do if [ -d %[2]s/$x ] && [ ! -e %[1]s/$x ]; then mv %[2]s/$x %[1]s/$x; fi; done; %[4]s up -d caddy`,
		quote(p.caddyDir()), handoverDir, handoverCompose(), p.dep().ComposeCmd("infra"))
	if out, err := t.Run(back); err != nil {
		return fmt.Errorf("%s: %w\nPutting this deployment's Caddy back failed too: %v: %s", p.Site, cause, err, lastLines(out, 3))
	}
	return fmt.Errorf("%s: %w\nThis deployment's Caddy was started again with its certificates, and %s was left without a marker, so the next run tries again", p.Site, cause, handoverDir)
}

// now is the clock the marker records. Tests replace it.
var now = time.Now
