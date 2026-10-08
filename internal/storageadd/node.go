package storageadd

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// Paths on a Garage site, as infra-compose.yaml.tmpl mounts them, under the
// deployment's root.
func garageToml(d deployment.Deployment) string { return "/" + apply.GarageConfig(d) }

func metaDir(d deployment.Deployment) string { return d.Path("infra", "garage", "meta") }

// layoutFile is where Garage keeps its stored layout
// (src/rpc/layout/manager.rs:43-44). The reset sets it aside rather than
// deleting it, so a reset that went wrong can be undone by hand.
func layoutFile(d deployment.Deployment) string { return metaDir(d) + "/cluster_layout" }

// countsFile holds each bucket's object count from before a reset, on the
// anchor's host, until the provision gate has compared it.
func countsFile(d deployment.Deployment) string {
	return d.Path("infra", "garage", "replication-change.counts")
}

func stopGarage(d deployment.Deployment) string  { return d.ComposeCmd("infra") + " stop garage" }
func startGarage(d deployment.Deployment) string { return d.ComposeCmd("infra") + " up -d garage" }

// node is what one Garage site's node said when Build read it.
type node struct {
	dep     deployment.Deployment
	site    string
	address string
	// id is the full node ID `node id -q` prints, without its address. Empty
	// when the node did not answer.
	id string
	// answerErr is why `node id -q` failed, empty when it answered.
	answerErr string
	// deployed is whether the site has a garage.toml, and factor is the
	// replication_factor in it.
	deployed bool
	factor   int
	// version is the layout version this node reports, and hasRole whether
	// its own ID is a row in that layout.
	version int
	hasRole bool
	// setAside is whether an earlier reset already moved this node's stored
	// layout aside, and hasLayout whether one is in place now.
	setAside  bool
	hasLayout bool
}

func (n *node) short() string {
	if len(n.id) > 16 {
		return n.id[:16]
	}
	return n.id
}

func (n *node) answers() bool { return n.id != "" }

func (p *Plan) readNode(site string) (*node, error) {
	t := p.transports[site]
	n := &node{dep: p.dep(), site: site, address: p.cfg.Sites[site].Address}

	toml, found, err := t.ReadFile(garageToml(p.dep()))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", site, err)
	}
	if found {
		n.deployed = true
		n.factor, _ = apply.GarageReplication(toml)
	}

	// A missing directory is an answer, an empty listing; any failure is
	// not one. Ignoring a failed listing read as "no stored layout" and
	// planned no set aside for a node that has one.
	listing, err := t.Run(fmt.Sprintf("if [ -d %[1]s ]; then ls -1 %[1]s; fi", metaDir(p.dep())))
	if err != nil {
		return nil, fmt.Errorf("%s: could not read host state (the listing of %s): %w", site, metaDir(p.dep()), err)
	}
	for _, name := range strings.Fields(listing) {
		switch {
		case name == "cluster_layout":
			n.hasLayout = true
		case strings.HasPrefix(name, "cluster_layout.rf"):
			n.setAside = true
		}
	}

	if !n.deployed {
		return n, nil
	}
	if err := n.refresh(t); err != nil {
		// A node that could not be asked has not answered "no". Recording
		// it as silent would let a counts file left by an earlier run
		// resume a reset, which stops every node, on a blip of ssh.
		if errors.Is(err, apply.ErrUnreachable) {
			return nil, fmt.Errorf("%s: %w", site, err)
		}
		n.answerErr = err.Error()
	}
	return n, nil
}

// refresh reads the node's ID and its view of the layout.
func (n *node) refresh(t apply.Transport) error {
	out, err := t.Run(gcmd(n.dep, "node id -q"))
	if err != nil {
		n.id = ""
		if errors.Is(err, apply.ErrUnreachable) {
			return fmt.Errorf("could not read host state: %w", err)
		}
		return fmt.Errorf("`garage node id` failed: %s", lastLines(out, 3))
	}
	// The combined output can carry Garage's own log lines, so the ID is
	// the line shaped like one rather than the first or the last.
	id := ""
	for _, line := range strings.Split(ansi.ReplaceAllString(out, ""), "\n") {
		candidate := strings.TrimSpace(line)
		if at := strings.Index(candidate, "@"); at >= 0 {
			candidate = candidate[:at]
		}
		if nodeIDShape.MatchString(candidate) {
			id = candidate
		}
	}
	if id == "" {
		n.id = ""
		return fmt.Errorf("`garage node id` printed no node ID: %s", lastLines(out, 3))
	}
	n.id = id
	layout, err := t.Run(gcmd(n.dep, "layout show"))
	if err != nil {
		return fmt.Errorf("`garage layout show` failed: %s", lastLines(layout, 3))
	}
	l, err := parseLayout(layout)
	if err != nil {
		return err
	}
	n.version = l.version
	_, n.hasRole = l.rows[n.short()]
	return nil
}

var nodeIDShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// layout is what `garage layout show` says about the current layout.
type layout struct {
	version int
	// rows maps a node's short ID to its zone.
	rows map[string]string
	// capacity maps a node's short ID to its capacity in bytes, as shown:
	// decimal units to one decimal place, so only approximately.
	capacity map[string]int64
}

// shownSize matches a capacity as `layout show` prints it, Eg: 100.0 GB.
var shownSize = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)$`)

// capacityTolerance is how far a shown capacity may sit from the configured
// one before the node is assigned again. One decimal place hides at most 5%
// on a value just above a unit boundary (Eg: 1.05 GB shown as 1.0 GB) but
// under 2% on anything from 5 units up, which covers every capacity a node
// here will declare; a smaller change than that is not worth a rebalance.
const capacityTolerance = 0.02

// sameCapacity reports whether a shown capacity matches the configured one
// within capacityTolerance.
func sameCapacity(shown, want int64) bool {
	if want <= 0 {
		return true
	}
	diff := float64(shown-want) / float64(want)
	return diff <= capacityTolerance && diff >= -capacityTolerance
}

// ansi matches the colour codes Garage's log lines carry.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

var shortID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// parseLayout reads `garage layout show` as dxflrs/garage:v1.0.1 prints it:
// a "==== CURRENT CLUSTER LAYOUT ====" table of "ID Tags Zone Capacity
// Usable" rows, the short ID first, then "Current cluster layout version: N".
// Only rows between the banner and the version line count; the RPC client's
// "Connection established to <ID>" log line before the banner names a node
// whether or not it has a role.
func parseLayout(out string) (layout, error) {
	l := layout{rows: map[string]string{}, capacity: map[string]int64{}}
	text := ansi.ReplaceAllString(out, "")
	const banner = "==== CURRENT CLUSTER LAYOUT ===="
	const marker = "Current cluster layout version:"
	start := strings.Index(text, banner)
	end := strings.Index(text, marker)
	if end < 0 {
		return l, fmt.Errorf("could not find %q in `garage layout show`: %s", marker, lastLines(text, 3))
	}
	rest := strings.TrimSpace(text[end+len(marker):])
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	v, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return l, fmt.Errorf("parsing the layout version %q: %w", rest, err)
	}
	l.version = v
	if start < 0 || start > end {
		return l, nil
	}
	for _, line := range strings.Split(text[start:end], "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !shortID.MatchString(fields[0]) {
			continue
		}
		rest := fields[1:]
		if rest[0] == "[]" && len(rest) > 1 {
			rest = rest[1:]
		}
		l.rows[fields[0]] = rest[0]
		// Capacity follows the zone as a number and a decimal unit.
		if len(rest) >= 3 && shownSize.MatchString(rest[1]) {
			if n, err := config.ParseSize(rest[1] + strings.TrimSuffix(rest[2], "B")); err == nil {
				l.capacity[fields[0]] = n
			}
		}
	}
	return l, nil
}

// healthy reads the short IDs under `garage status`'s HEALTHY NODES heading,
// and whether a FAILED NODES heading follows.
func parseStatus(out string) (healthy map[string]bool, failed bool) {
	healthy = map[string]bool{}
	text := ansi.ReplaceAllString(out, "")
	inHealthy := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "==== HEALTHY NODES ===="):
			inHealthy = true
			continue
		case strings.Contains(line, "==== FAILED NODES ===="):
			inHealthy = false
			failed = true
			continue
		}
		if !inHealthy {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && shortID.MatchString(fields[0]) {
			healthy[fields[0]] = true
		}
	}
	return healthy, failed
}

// stableLayout is `garage layout history`'s line for a cluster with one live
// layout version: every node has synced the newest, and no metadata is
// moving (src/garage/cli/layout.rs:401-406).
const stableLayout = "stable state with a single live layout version"

var (
	resyncQueue  = regexp.MustCompile(`resync queue length:\s*(\d+)`)
	resyncErrors = regexp.MustCompile(`blocks with resync errors:\s*(\d+)`)
	objectsLine  = regexp.MustCompile(`(?m)^Objects:\s*(\d+)\s*$`)
)

// parseResync reads `garage stats`'s block manager lines
// (src/garage/admin/mod.rs:184-240).
func parseResync(out string) (queue, errs int, err error) {
	text := ansi.ReplaceAllString(out, "")
	q := resyncQueue.FindStringSubmatch(text)
	e := resyncErrors.FindStringSubmatch(text)
	if q == nil || e == nil {
		return 0, 0, fmt.Errorf("`garage stats` printed no resync queue length: %s", lastLines(text, 3))
	}
	queue, _ = strconv.Atoi(q[1])
	errs, _ = strconv.Atoi(e[1])
	return queue, errs, nil
}

// parseObjects reads the "Objects: N" line of `garage bucket info`.
func parseObjects(out string) (int, error) {
	m := objectsLine.FindStringSubmatch(ansi.ReplaceAllString(out, ""))
	if m == nil {
		return 0, fmt.Errorf("`garage bucket info` printed no object count: %s", lastLines(out, 3))
	}
	return strconv.Atoi(m[1])
}
