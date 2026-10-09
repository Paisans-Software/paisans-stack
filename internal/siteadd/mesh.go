package siteadd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// handshakeProbe prints each peer's latest handshake and the host's own clock,
// so an age is computed against the clock that wrote the timestamp. `wg show
// <interface> latest-handshakes` prints one line per peer, its public key and
// a Unix time, 0 for a peer that has never completed one (wireguard-tools,
// wg(8), "latest-handshakes").
func handshakeProbe(d deployment.Deployment) string {
	return "wg show " + d.Interface() + ` latest-handshakes; echo "now $(date +%s)"`
}

// buildMesh is stage 2: every site's WireGuard file with the new peer, written as a
// scoped apply so the next whole apply sees it as its own, and handed to the
// running interface without taking it down.
func (p *Plan) buildMesh(rendered *render.Plan) (*Stage, error) {
	st := &Stage{
		Number: 2,
		Name:   "mesh",
		Short:  "mesh handshakes and ping",
		Gate: fmt.Sprintf("a handshake younger than %d seconds between every pair of sites, read from `wg show` on both ends, and %s's mesh address %s answers ping from every site",
			handshakeMaxAge, p.Site, p.cfg.Sites[p.Site].Address),
		OnFailure: "restore the previous " + p.cfg.Deployment().Interface() + ".conf on every existing site and sync it, so the running cluster is untouched; the new site's interface is left, since no existing site has it as a peer any more",
	}
	plans := map[string]*apply.Plan{}
	var order []string
	for _, name := range p.cfg.SiteNames() {
		if name != p.Site {
			order = append(order, name)
		}
	}
	order = append(order, p.Site)

	for _, name := range order {
		mp, err := apply.Build(name, rendered, "", p.transports[name], apply.Scope(p.cfg.Deployment().WireGuardConf()))
		if err != nil {
			return nil, err
		}
		if conflicts := mp.Conflicts(); len(conflicts) > 0 {
			return nil, fmt.Errorf("site add %s: %s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed. Copy what is wanted into the configuration, or restore the file, and run site add again", p.Site, name, conflicts[0].Path)
		}
		plans[name] = mp
		for _, c := range mp.Writes() {
			st.Steps = append(st.Steps, Step{Site: name, Verb: c.Kind.String(), Title: "update mesh on " + name, Text: c.Path})
		}
		if mp.WireGuard != apply.WireGuardNone {
			st.Steps = append(st.Steps, Step{Site: name, Verb: "mesh", Title: "sync mesh on " + name, Text: mp.WireGuard.Describe(p.cfg.Deployment())})
		}
	}

	var applied []string
	st.run = func() error {
		for _, name := range order {
			mp := plans[name]
			if len(mp.Writes()) == 0 && mp.WireGuard == apply.WireGuardNone {
				continue
			}
			applied = append(applied, name)
			p.work("update mesh on " + name)
			if err := apply.Execute(mp, p.transports[name]); err != nil {
				return err
			}
		}
		return nil
	}
	st.gate = p.meshGate
	st.rollback = func() error {
		for _, name := range applied {
			if name == p.Site {
				continue
			}
			if err := apply.Rollback(plans[name], p.transports[name]); err != nil {
				return err
			}
		}
		return nil
	}
	return st, nil
}

// meshGate passes when every site reaches the new one and every pair of sites
// has a recent handshake seen from both ends. Both ends are read because
// each keeps its own record, and the spec asks for the tunnel to be proven
// from each side rather than inferred from one.
func (p *Plan) meshGate() error {
	keys := map[string]string{}
	for _, name := range p.cfg.SiteNames() {
		key, err := render.PublicKey(p.secrets.Sites[name].WireGuardPrivateKey)
		if err != nil {
			return fmt.Errorf("secrets sites.%s.wireguard_private_key: %w", name, err)
		}
		keys[name] = key
	}
	address := p.cfg.Sites[p.Site].Address
	var problems []string
	n := attempts(meshWait, meshPoll)
	for i := 0; i < n; i++ {
		problems = nil
		// Ping first: on a peer that has not handshaken yet, the first packet
		// is what starts one.
		for _, name := range p.cfg.SiteNames() {
			if name == p.Site {
				continue
			}
			if out, err := p.transports[name].Run("ping -c 3 -W 2 " + address); err != nil {
				// A ping that never ran is not a ping that failed: the
				// mesh may be whole, and only ssh to this site is not.
				if errors.Is(err, apply.ErrUnreachable) {
					return fmt.Errorf("%s: could not read host state, so whether it reaches %s is unknown: %w", name, p.Site, err)
				}
				problems = append(problems, fmt.Sprintf("%s cannot reach %s at %s: %s", name, p.Site, address, lastLines(out, 2)))
			}
		}
		views := map[string]map[string]int64{}
		for _, name := range p.cfg.SiteNames() {
			out, err := p.transports[name].Run(handshakeProbe(p.cfg.Deployment()))
			if err != nil {
				if errors.Is(err, apply.ErrUnreachable) {
					return fmt.Errorf("%s: could not read host state, so its handshakes are unknown: %w", name, err)
				}
				problems = append(problems, fmt.Sprintf("%s: `wg show %s` failed: %s", name, p.cfg.Deployment().Interface(), lastLines(out, 2)))
				continue
			}
			views[name] = handshakeAges(out)
		}
		for _, a := range p.cfg.SiteNames() {
			view, ok := views[a]
			if !ok {
				continue
			}
			for _, b := range p.cfg.SiteNames() {
				if a == b {
					continue
				}
				age, ok := view[keys[b]]
				switch {
				case !ok:
					problems = append(problems, fmt.Sprintf("%s has no peer for %s", a, b))
				case age < 0:
					problems = append(problems, fmt.Sprintf("%s has never completed a handshake with %s", a, b))
				case age > handshakeMaxAge:
					problems = append(problems, fmt.Sprintf("%s last completed a handshake with %s %d seconds ago", a, b, age))
				}
			}
		}
		if len(problems) == 0 {
			return nil
		}
		if i < n-1 {
			sleep(meshPoll)
		}
	}
	return fmt.Errorf("the mesh is not whole after %s:\n  %s\nCheck that each endpoint's port is open to UDP and forwarded, and read `wg show %s` on the sites named", meshWait, strings.Join(problems, "\n  "), p.cfg.Deployment().Interface())
}

// handshakeAges reads handshakeProbe's output: each peer's key and the age of
// its latest handshake in seconds, -1 for none yet.
func handshakeAges(out string) map[string]int64 {
	var now int64
	stamps := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if fields[0] == "now" {
			now = value
			continue
		}
		stamps[fields[0]] = value
	}
	ages := map[string]int64{}
	for key, stamp := range stamps {
		if stamp == 0 {
			ages[key] = -1
			continue
		}
		ages[key] = now - stamp
	}
	return ages
}
