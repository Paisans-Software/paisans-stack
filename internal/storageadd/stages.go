package storageadd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// differs reports whether a node's garage.toml names another factor than the
// configuration, which is what makes it need the reset.
func (p *Plan) differs(n *node) bool {
	return n.deployed && n.factor != p.replication()
}

// asideName is where a reset moves a node's stored layout: named for the
// factor it was built with, so that a later reset at another factor neither
// mistakes it for its own nor overwrites it.
func asideName(factor int) string {
	return fmt.Sprintf("%s.rf%d", layoutFile, factor)
}

// buildNodes is stage 1: what every node said, and whether the join can go
// ahead on it.
func (p *Plan) buildNodes() *Stage {
	st := &Stage{
		Name: "nodes",
		Gate: "every Garage site has a garage.toml and its node answers, every deployed replication factor matches the configuration or --change-replication was given, and no site without a role is listed ahead of one with a role",
	}
	for _, n := range p.nodes {
		var text string
		switch {
		case !n.deployed:
			text = "no garage.toml yet"
		case !n.answers():
			text = fmt.Sprintf("garage.toml at replication %d; the node does not answer: %s", n.factor, n.answerErr)
		default:
			role := "no role"
			if n.hasRole {
				role = "has a role"
			}
			text = fmt.Sprintf("node %s, garage.toml at replication %d, layout version %d, %s", n.short(), n.factor, n.version, role)
		}
		st.Steps = append(st.Steps, Step{Site: n.site, Verb: "read", Text: text})
	}
	st.gate = func() error {
		var problems []string
		for _, n := range p.nodes {
			if !n.deployed {
				problems = append(problems, fmt.Sprintf("%s has no %s. Run `paisans apply --site %s` first: it writes the file and starts Garage there", n.site, garageToml, n.site))
				continue
			}
			// A node the reset stopped is started again by it, so only an
			// unexplained silence is a problem.
			if !n.answers() && !p.reset {
				problems = append(problems, fmt.Sprintf("%s's Garage does not answer: %s", n.site, n.answerErr))
			}
		}
		if p.needsChange && !p.opts.ChangeReplication {
			var have []string
			for _, n := range p.nodes {
				if p.differs(n) {
					have = append(have, fmt.Sprintf("%s at %d", n.site, n.factor))
				}
			}
			problems = append(problems, fmt.Sprintf(
				"the configuration says replication %d and Garage runs at another (%s). Garage v1.0.1 cannot change it in place: a node refuses to start against a stored layout built at another factor. The only documented way is to stop every node, set each stored layout aside and lay the cluster out again, which Garage calls unsupported, so it runs only when asked: re-run with --change-replication. Media is unavailable for about a minute while it runs",
				p.replication(), strings.Join(have, ", ")))
		}
		if !p.reset {
			for i, n := range p.nodes {
				if n.hasRole {
					continue
				}
				for _, later := range p.nodes[i+1:] {
					if later.hasRole {
						problems = append(problems, fmt.Sprintf(
							"%s has no role yet but is listed ahead of %s, which does. The first listed site serves media and takes every app's writes, and a node with no role answers every bucket as missing. Move %s to the end of storage.garage.sites, run storage add, and move it up once it has passed",
							n.site, later.site, n.site))
						break
					}
				}
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%s", strings.Join(problems, "\n  "))
		}
		return nil
	}
	return st
}

// syncState reads whether the given nodes have finished moving data: one live
// layout version, then an empty block resync queue on every one of them for
// syncSamples reads in a row. It returns a *Waiting when they have not.
//
// The queue is read only once the layout is stable: a block is queued for
// resync when its reference reaches a node (src/block/manager.rs:453-476), so
// an empty queue before the tables have synced proves nothing.
func (p *Plan) syncState(st *Stage, sites []string) error {
	for _, site := range sites {
		out, err := p.transports[site].Run(gcmd("layout history"))
		if err != nil {
			return fmt.Errorf("%s: `garage layout history` failed: %s", site, lastLines(out, 3))
		}
		if !strings.Contains(ansi.ReplaceAllString(out, ""), stableLayout) {
			return &Waiting{Stage: st, Detail: fmt.Sprintf("%s reports more than one live layout version, so metadata is still moving", site)}
		}
	}
	for i := 0; i < syncSamples; i++ {
		if i > 0 {
			sleep(sampleGap)
		}
		var busy []string
		failing := false
		for _, site := range sites {
			out, err := p.transports[site].Run(gcmd("stats"))
			if err != nil {
				return fmt.Errorf("%s: `garage stats` failed: %s", site, lastLines(out, 3))
			}
			queue, errs, err := parseResync(out)
			if err != nil {
				return fmt.Errorf("%s: %w", site, err)
			}
			if errs > 0 {
				failing = true
			}
			if queue > 0 || errs > 0 {
				busy = append(busy, fmt.Sprintf("%s resync queue %d, errors %d", site, queue, errs))
			}
		}
		if len(busy) > 0 {
			detail := "blocks are still moving: " + strings.Join(busy, "; ")
			if failing {
				detail += ". Resync errors are retried; `garage block list-errors` on that site names them if they persist"
			}
			return &Waiting{Stage: st, Detail: detail}
		}
	}
	return nil
}

// buildSettle is stage 2, before a reset: every node already in the layout
// has caught up. At consistency dangerous an upload is confirmed once one copy
// exists, so a reset must not start while one may exist on one node only.
func (p *Plan) buildSettle() *Stage {
	st := &Stage{
		Name:  "settle",
		Gate:  "on every node with a role, `garage layout history` reports a single live layout version and `garage stats` an empty resync queue with no errors, three reads in a row",
		Waits: true,
	}
	var sites []string
	for _, n := range p.nodes {
		if n.hasRole && n.answers() {
			sites = append(sites, n.site)
		}
	}
	st.gate = func() error {
		if len(sites) == 0 {
			return nil
		}
		return p.syncState(st, sites)
	}
	return st
}

// buildReset is stage 3: Garage's documented procedure for another
// replication factor (doc/book/reference-manual/configuration.md,
// "replication_factor"), which it calls unsupported. Every node at the old
// factor is stopped before any is changed: a node that meets a peer at a
// higher factor exits (src/rpc/system.rs:583-587).
func (p *Plan) buildReset() *Stage {
	st := &Stage{
		Name: "reset",
		Gate: "every Garage node answers again, at the configured replication factor",
	}
	_, countsRecorded, _ := p.transports[p.anchor].ReadFile(countsFile)
	if !countsRecorded {
		st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "count", Text: "record every bucket's object count in " + countsFile + ", for the provision gate to compare"})
	}
	var changing []*node
	for _, n := range p.nodes {
		if p.differs(n) {
			changing = append(changing, n)
		}
	}
	for _, n := range changing {
		st.Steps = append(st.Steps, Step{Site: n.site, Verb: "stop", Text: "Garage: " + stopGarage})
	}
	for _, n := range changing {
		if n.hasLayout {
			st.Steps = append(st.Steps, Step{Site: n.site, Verb: "set aside", Text: fmt.Sprintf("the stored layout, built at replication %d: mv -n %s %s", n.factor, layoutFile, asideName(n.factor))})
		}
	}
	for _, n := range changing {
		st.Steps = append(st.Steps, Step{Site: n.site, Verb: "write", Text: fmt.Sprintf("%s at replication %d (was %d)", garageToml, p.replication(), n.factor)})
	}
	for _, n := range p.nodes {
		st.Steps = append(st.Steps, Step{Site: n.site, Verb: "start", Text: "Garage: " + startGarage})
	}
	st.run = func() error {
		if !countsRecorded {
			if err := p.recordCounts(); err != nil {
				return err
			}
		}
		for _, n := range changing {
			p.say("  %-9s Garage on %s\n", "stop", n.site)
			if out, err := p.transports[n.site].Run(stopGarage); err != nil {
				return fmt.Errorf("%s: stopping Garage: %s", n.site, lastLines(out, 5))
			}
		}
		for _, n := range changing {
			t := p.transports[n.site]
			cmd := fmt.Sprintf("if [ -e %[1]s ]; then mv -n %[1]s %[2]s; fi; test ! -e %[1]s", layoutFile, asideName(n.factor))
			if out, err := t.Run(cmd); err != nil {
				return fmt.Errorf("%s: setting the stored layout aside left %s in place, so Garage would refuse to start at the new factor. %s already exists there from an earlier attempt; move one of them by hand only once you know which is which: %s", n.site, layoutFile, asideName(n.factor), lastLines(out, 3))
			}
		}
		for _, n := range changing {
			t := p.transports[n.site]
			sp, err := apply.Build(n.site, p.rendered, acme.Module(p.cfg.ACME.Provider), t, apply.Scope(apply.GarageConfig), apply.ReplicationChange())
			if err != nil {
				return err
			}
			if c := sp.Conflicts(); len(c) > 0 {
				return fmt.Errorf("%s: %s differs from what the last apply recorded, so somebody edited it on the host. Garage there is stopped. Restore the file, or copy what is wanted into the configuration, and run storage add again", n.site, c[0].Path)
			}
			if err := apply.Execute(sp, t); err != nil {
				return err
			}
		}
		for _, n := range p.nodes {
			p.say("  %-9s Garage on %s\n", "start", n.site)
			if out, err := p.transports[n.site].Run(startGarage); err != nil {
				return fmt.Errorf("%s: starting Garage: %s", n.site, lastLines(out, 5))
			}
		}
		return nil
	}
	st.gate = func() error {
		return poll(attempts(startWait, startPoll), startPoll, func() error {
			for _, n := range p.nodes {
				t := p.transports[n.site]
				toml, _, err := t.ReadFile(garageToml)
				if err != nil {
					return err
				}
				if f, _ := apply.GarageReplication(toml); f != p.replication() {
					return fmt.Errorf("%s's garage.toml is at replication %d", n.site, f)
				}
				if err := n.refresh(t); err != nil {
					return fmt.Errorf("%s: %w", n.site, err)
				}
			}
			return nil
		})
	}
	return st
}

// buckets is every bucket the configuration's apps store objects in, sorted.
func (p *Plan) buckets() []string {
	var out []string
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if kinds.UsesObjectStorage(app.Kind) {
			out = append(out, garage.BucketName(app, name))
		}
	}
	sort.Strings(out)
	return out
}

// recordCounts writes each bucket's object count to the anchor's host before
// the reset stops anything. It is on the host, not the operator's machine, so
// that a run resumed from anywhere compares against the same numbers.
func (p *Plan) recordCounts() error {
	t := p.transports[p.anchor]
	var b strings.Builder
	fmt.Fprintf(&b, "# Object counts before storage add changed the replication factor to %d.\n# Removed by storage add once its provision gate has compared them.\n", p.replication())
	for _, bucket := range p.buckets() {
		out, err := t.Run(gcmd("bucket info " + bucket))
		if err != nil {
			if strings.Contains(out, "Bucket not found") {
				continue
			}
			return fmt.Errorf("%s: counting objects in %s: %s", p.anchor, bucket, lastLines(out, 3))
		}
		n, err := parseObjects(out)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", p.anchor, bucket, err)
		}
		fmt.Fprintf(&b, "%s %d\n", bucket, n)
	}
	return t.WriteFile(countsFile, b.String(), 0o600)
}

func parseCounts(content string) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			out[fields[0]] = n
		}
	}
	return out
}

// buildConnect is stage 4: the anchor dials every node it does not see.
// Garage keeps the peers a node has met in meta/peer_list and redials them
// after a restart (src/rpc/system.rs:274, :645-650), so a connect is needed
// once per node, and nothing renders bootstrap_peers.
func (p *Plan) buildConnect() *Stage {
	st := &Stage{
		Name: "connect",
		Gate: "`garage status` on every node lists every configured node as healthy",
	}
	seen := map[string]bool{}
	if a := p.node(p.anchor); a.answers() && !p.reset {
		if out, err := p.transports[p.anchor].Run(gcmd("status")); err == nil {
			seen, _ = parseStatus(out)
		}
	}
	for _, n := range p.nodes {
		if n.site == p.anchor || (n.answers() && seen[n.short()]) {
			continue
		}
		st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "connect", Text: fmt.Sprintf("%s: garage node connect <its node ID>@%s:3901", n.site, n.address)})
	}
	st.run = func() error {
		anchor := p.transports[p.anchor]
		out, err := anchor.Run(gcmd("status"))
		if err != nil {
			return fmt.Errorf("%s: `garage status` failed: %s", p.anchor, lastLines(out, 3))
		}
		seen, _ := parseStatus(out)
		for _, n := range p.nodes {
			if n.site == p.anchor {
				continue
			}
			if err := n.refresh(p.transports[n.site]); err != nil {
				return fmt.Errorf("%s: %w", n.site, err)
			}
			if seen[n.short()] {
				continue
			}
			p.say("  %-9s %s to %s\n", "connect", n.site, p.anchor)
			target := fmt.Sprintf("%s@%s:3901", n.id, n.address)
			if out, err := anchor.Run(gcmd("node connect " + target)); err != nil {
				return fmt.Errorf("%s: connecting %s: %s", p.anchor, n.site, lastLines(out, 3))
			}
		}
		return nil
	}
	st.gate = func() error {
		return poll(attempts(connectWait, connectPoll), connectPoll, func() error {
			for _, n := range p.nodes {
				if err := n.refresh(p.transports[n.site]); err != nil {
					return fmt.Errorf("%s: %w", n.site, err)
				}
			}
			for _, n := range p.nodes {
				out, err := p.transports[n.site].Run(gcmd("status"))
				if err != nil {
					return fmt.Errorf("%s: `garage status` failed: %s", n.site, lastLines(out, 3))
				}
				healthy, _ := parseStatus(out)
				for _, other := range p.nodes {
					if !healthy[other.short()] {
						return fmt.Errorf("%s does not list %s (%s) as healthy", n.site, other.site, other.short())
					}
				}
			}
			return nil
		})
	}
	return st
}

// buildLayout is stage 5: every node gets a role in the zone named after its
// site, in one layout version. One version rather than one per node, because
// Garage refuses a layout with fewer storage nodes than the replication
// factor (src/rpc/layout/version.rs:325-332), and zones named after sites,
// because only distinct zones spread a partition's copies across sites
// (version.rs:157-172, :314-319).
func (p *Plan) buildLayout() *Stage {
	st := &Stage{
		Name: "layout",
		Gate: "`garage layout show` on every node reports the same version, with a role for every Garage site in the zone named after it, at its configured capacity",
	}
	garageCfg := p.cfg.Storage.Garage
	version := 0
	var shown layout
	if a := p.node(p.anchor); a.answers() && !p.reset {
		version = a.version
		if out, err := p.transports[p.anchor].Run(gcmd("layout show")); err == nil {
			shown, _ = parseLayout(out)
		}
	}
	changes := 0
	for _, n := range p.nodes {
		want := garageCfg.CapacityFor(n.site)
		switch {
		case p.reset || !n.hasRole:
			st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "assign", Text: fmt.Sprintf("%s: garage layout assign -z %s -c %s <its node ID>", n.site, n.site, want)})
			changes++
		case !p.capacityMatches(shown, n):
			st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "resize", Text: fmt.Sprintf("%s: garage layout assign -c %s <its node ID>, from %s; Garage rebalances to match", n.site, want, humanBytes(shown.capacity[n.short()]))})
			changes++
		}
	}
	if changes > 0 {
		st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "apply", Text: fmt.Sprintf("garage layout apply --version %d", version+1)})
	}
	st.run = func() error {
		anchor := p.transports[p.anchor]
		out, err := anchor.Run(gcmd("layout show"))
		if err != nil {
			return fmt.Errorf("%s: `garage layout show` failed: %s", p.anchor, lastLines(out, 3))
		}
		current, err := parseLayout(out)
		if err != nil {
			return fmt.Errorf("%s: %w", p.anchor, err)
		}
		assigned := 0
		for _, n := range p.nodes {
			if err := n.refresh(p.transports[n.site]); err != nil {
				return fmt.Errorf("%s: %w", n.site, err)
			}
			if zone, ok := current.rows[n.short()]; ok && zone == n.site && p.capacityMatches(current, n) {
				continue
			}
			capacity := garageCfg.CapacityFor(n.site)
			p.say("  %-9s %s a role in zone %s at %s\n", "assign", n.site, n.site, capacity)
			cmd := fmt.Sprintf("layout assign -z %s -c %s %s", n.site, capacity, n.id)
			if out, err := anchor.Run(gcmd(cmd)); err != nil {
				return fmt.Errorf("%s: assigning %s: %s", p.anchor, n.site, lastLines(out, 3))
			}
			assigned++
		}
		if assigned == 0 {
			return nil
		}
		next := current.version + 1
		p.say("  %-9s the layout at version %d\n", "apply", next)
		if out, err := anchor.Run(gcmd(fmt.Sprintf("layout apply --version %d", next))); err != nil {
			return fmt.Errorf("%s: applying the layout at version %d: %s", p.anchor, next, lastLines(out, 5))
		}
		return nil
	}
	st.gate = func() error {
		return poll(attempts(layoutWait, layoutPoll), layoutPoll, func() error {
			versions := map[int][]string{}
			for _, n := range p.nodes {
				out, err := p.transports[n.site].Run(gcmd("layout show"))
				if err != nil {
					return fmt.Errorf("%s: `garage layout show` failed: %s", n.site, lastLines(out, 3))
				}
				l, err := parseLayout(out)
				if err != nil {
					return fmt.Errorf("%s: %w", n.site, err)
				}
				versions[l.version] = append(versions[l.version], n.site)
				for _, other := range p.nodes {
					zone, ok := l.rows[other.short()]
					if !ok {
						return fmt.Errorf("%s's layout (version %d) has no role for %s", n.site, l.version, other.site)
					}
					if zone != other.site {
						return fmt.Errorf("%s's layout puts %s in zone %s, not %s, so its copies may not land on another site", n.site, other.site, zone, other.site)
					}
					if !p.capacityMatches(l, other) {
						return fmt.Errorf("%s's layout gives %s %s, not %s", n.site, other.site, humanBytes(l.capacity[other.short()]), p.cfg.Storage.Garage.CapacityFor(other.site))
					}
				}
			}
			if len(versions) > 1 {
				return fmt.Errorf("the nodes report different layout versions: %v", versions)
			}
			return nil
		})
	}
	return st
}

// buildSync is stage 6: Garage moves the data to match the new layout. It
// takes as long as the slowest site's upload allows, so it waits.
func (p *Plan) buildSync() *Stage {
	st := &Stage{
		Name:  "sync",
		Gate:  "on every node, `garage layout history` reports a single live layout version and `garage stats` an empty resync queue with no errors, three reads in a row",
		Waits: true,
	}
	var sites []string
	for _, n := range p.nodes {
		sites = append(sites, n.site)
	}
	st.gate = func() error { return p.syncState(st, sites) }
	return st
}

// buildProvision is stage 7: each app's key, bucket, grant and website access,
// which Garage keeps in tables replicated to every node with a role
// (src/model/garage.rs:168-188), planned by storage init's own planner against
// the anchor. On a cluster that was already provisioned it finds everything
// present.
func (p *Plan) buildProvision() (*Stage, error) {
	st := &Stage{
		Name: "provision",
		Gate: "storage init's plan against the anchor has nothing left, and after a reset no bucket has fewer objects than it had before",
	}
	t := p.transports[p.anchor]
	ready := !p.reset
	for _, n := range p.nodes {
		if !n.hasRole || !n.answers() {
			ready = false
		}
	}
	if ready {
		plan, err := garage.Build(p.anchor, p.cfg, p.secrets, t)
		if err != nil {
			return nil, err
		}
		for _, step := range plan.Steps {
			st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "create", Text: step.Describe})
		}
	} else {
		st.Steps = append(st.Steps, Step{Site: p.anchor, Verb: "plan", Text: "keys, buckets, grants and website access, once every node has a role: storage init's planner, run against this site"})
	}
	st.run = func() error {
		plan, err := garage.Build(p.anchor, p.cfg, p.secrets, t)
		if err != nil {
			return err
		}
		return garage.Execute(plan, t)
	}
	st.gate = func() error {
		plan, err := garage.Build(p.anchor, p.cfg, p.secrets, t)
		if err != nil {
			return err
		}
		if len(plan.Steps) > 0 {
			var left []string
			for _, s := range plan.Steps {
				left = append(left, s.Describe)
			}
			return fmt.Errorf("still missing on %s: %s", p.anchor, strings.Join(left, "; "))
		}
		recorded, found, err := t.ReadFile(countsFile)
		if err != nil || !found {
			return err
		}
		before := parseCounts(recorded)
		for _, bucket := range p.buckets() {
			want, ok := before[bucket]
			if !ok {
				continue
			}
			out, err := t.Run(gcmd("bucket info " + bucket))
			if err != nil {
				return fmt.Errorf("%s: reading %s: %s", p.anchor, bucket, lastLines(out, 3))
			}
			have, err := parseObjects(out)
			if err != nil {
				return err
			}
			if have < want {
				return &Waiting{Stage: st, Detail: fmt.Sprintf(
					"%s holds %d objects and held %d before the reset. Garage's object counters converge through table sync, every ten minutes; if the count stays short, objects are missing, and each node's previous layout is still in %s",
					bucket, have, want, layoutFile+".rf<N>")}
			}
		}
		if out, err := t.Run("rm -f " + countsFile); err != nil {
			return fmt.Errorf("%s: removing %s: %s", p.anchor, countsFile, lastLines(out, 3))
		}
		return nil
	}
	return st, nil
}

// capacityMatches reports whether a layout gives node its configured
// capacity, within capacityTolerance. A layout that shows no capacity for it
// is not a match.
func (p *Plan) capacityMatches(l layout, n *node) bool {
	shown, ok := l.capacity[n.short()]
	if !ok {
		return false
	}
	want, err := config.ParseSize(p.cfg.Storage.Garage.CapacityFor(n.site))
	if err != nil {
		return true
	}
	return sameCapacity(shown, want)
}

func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
