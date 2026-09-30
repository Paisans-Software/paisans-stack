// Package garage plans and executes the Garage object storage provisioning
// this toolkit's other packages assume already exists: a cluster layout that
// gives each node a role, one S3 key per application that stores objects, and
// one bucket per such application, granted to that key.
//
// This is not part of apply. apply renders files and reconciles them against a
// manifest already on the host; every check it makes is "does this file match
// what was rendered". Garage's layout, keys and buckets are not files at all.
// They are state inside a running service that no manifest describes, reached
// only by asking Garage itself, so a different kind of check is needed: run a
// read command, look at what it says, and plan only what is missing.
package garage

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Transport is the one place this package talks to a machine. It is defined
// here, rather than imported from the apply package, so that this package does
// not depend on the applier: a planner that reasons about a running service's
// internal state has no business knowing how compose files get written.
// apply.SSHTransport already has both of these methods and satisfies this
// interface without changes.
type Transport interface {
	// Run executes a command and returns its combined output. A non zero exit
	// is an error: a caller that wants to tolerate one says so by inspecting
	// the error, never by ignoring it.
	Run(command string) (string, error)
	// Describe names the destination, for messages.
	Describe() string
}

// garageCmd is the prefix every command in this package runs through: the
// Garage binary inside its own container, on the infra stack this toolkit
// renders. It matches how apply reaches into the same stack for other
// commands, so an operator reading a transcript sees one shape throughout.
const garageCmd = "docker compose -f /srv/infra/compose.yaml exec -T garage /garage"

// Step is one command this plan still needs to run, in order.
type Step struct {
	// Describe is one line, shown to the operator before the command runs.
	Describe string
	// Command is the garage command, exactly as it will run.
	Command string
}

// Plan is what Build found missing on one site's node, in the order it must
// run, plus what was already there.
type Plan struct {
	Site string
	// Steps is only what is missing, in the order it must run.
	Steps []Step
	// Present is what was already there, for the report. An operator reading
	// a plan with zero steps should see why, rather than a plan that looks
	// like it found nothing to check.
	Present []string
}

// layoutLine matches Garage's "Current cluster layout version: N" line, which
// is present in `garage layout show` output whether or not the cluster has any
// role assigned yet.
func layoutVersion(out string) (int, error) {
	const marker = "Current cluster layout version:"
	idx := strings.Index(out, marker)
	if idx < 0 {
		return 0, fmt.Errorf("could not find %q in layout output:\n%s", marker, out)
	}
	rest := strings.TrimSpace(out[idx+len(marker):])
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	rest = strings.TrimSpace(rest)
	version, err := strconv.Atoi(rest)
	if err != nil {
		return 0, fmt.Errorf("parsing layout version %q: %w", rest, err)
	}
	return version, nil
}

// layoutTable narrows `layout show`'s combined output down to the actual
// layout table, between the "==== CURRENT CLUSTER LAYOUT ====" banner and the
// version line. Established by running dxflrs/garage:v1.0.1 against a single,
// truly fresh node: the RPC client that `layout show` runs logs "Connection
// established to <node ID>" for the peer it talks to before it prints the
// table, and on a single-node cluster that peer is this node itself. That log
// line carries this node's own ID whether or not the node has a role yet, so
// matching a node ID against the whole combined output makes every fresh node
// look already provisioned. Falling back to the whole output when the banner
// is missing keeps this from masking a genuinely malformed response.
func layoutTable(out string) string {
	const start = "==== CURRENT CLUSTER LAYOUT ===="
	const end = "Current cluster layout version:"
	startIdx := strings.Index(out, start)
	endIdx := strings.Index(out, end)
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		return out
	}
	return out[startIdx:endIdx]
}

// absent reports whether a check command means "this object does not exist
// yet", as opposed to "the node could not be reached". Garage names both cases
// on stderr and the marker is the only thing that tells them apart: a bare non
// zero exit would make an unreachable host look like an empty cluster, and the
// planner would then cheerfully plan an import against it.
func absent(out string, err error, marker string) (bool, error) {
	if err == nil {
		return false, nil
	}
	if strings.Contains(out, marker) {
		return true, nil
	}
	return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
}

// bucketName is the bucket an app's objects live in: a declared s3_bucket
// setting, or <app>-uploads. This mirrors internal/render's appValues.S3.Bucket
// derivation (see internal/render/appview.go), because the bucket this package
// creates has to be the same one the rendered application is told to use.
func bucketName(app config.App, name string) string {
	if v, ok := app.Settings["s3_bucket"].(string); ok && v != "" {
		return v
	}
	return name + "-uploads"
}

// Build checks what already exists on the target site's node and returns a
// plan of only what is missing, in the order it must run.
//
// The layout check is per node, not per cluster. A deployment with more than
// one Garage site (storage.garage.sites, replication 2 in the fixture) applies
// a layout once and then grows it: the second site's node has never appeared
// in it. Reading only "is the layout version 0" would find version 1 after the
// first site is provisioned and conclude, wrongly, that the second site is
// done, leaving it with no role while reporting success. So this checks
// whether this node's own ID appears in `layout show`'s rows, not whether the
// version is zero.
func Build(site string, cfg *config.Config, secrets *config.Secrets, t Transport) (*Plan, error) {
	if !slices.Contains(cfg.Storage.Garage.Sites, site) {
		return nil, fmt.Errorf("garage: %s holds no Garage role. Sites with one are %s", site, strings.Join(cfg.Storage.Garage.Sites, ", "))
	}

	plan := &Plan{Site: site}

	out, err := t.Run(garageCmd + " layout show")
	if err != nil {
		return nil, fmt.Errorf("checking the layout on %s: %w: %s", t.Describe(), err, strings.TrimSpace(out))
	}
	version, err := layoutVersion(out)
	if err != nil {
		return nil, fmt.Errorf("reading the layout on %s: %w", t.Describe(), err)
	}

	// A fresh node's ID is not yet known to us; the only way to get it is to
	// ask the node itself. `node id -q` reads nothing and changes nothing, so
	// running it during Build (rather than deferring it into a closure run at
	// Execute time) is safe, and it is what lets Build decide, right now,
	// whether this node's ID is already a row in the layout.
	nodeOut, err := t.Run(garageCmd + " node id -q")
	if err != nil {
		return nil, fmt.Errorf("reading the node ID on %s: %w: %s", t.Describe(), err, strings.TrimSpace(nodeOut))
	}
	nodeID := strings.TrimSpace(nodeOut)
	if at := strings.Index(nodeID, "@"); at >= 0 {
		nodeID = nodeID[:at]
	}

	// `garage layout show` prints only the first 16 hex characters of a node
	// ID in its table rows, not the full one `node id -q` returns.
	// Established by running dxflrs/garage:v1.0.1: a full ID is never a
	// substring of that output, so matching on it would never find a row
	// this node already has, and Build would replan a layout assign and
	// apply on every single run rather than only on a fresh node. The short
	// prefix is what the table actually prints, so it is what has to be
	// matched; the full ID is still what is given to `layout assign` below,
	// since that command was run with the full ID and accepted it.
	shortNodeID := nodeID
	if len(shortNodeID) > 16 {
		shortNodeID = shortNodeID[:16]
	}

	if shortNodeID != "" && strings.Contains(layoutTable(out), shortNodeID) {
		plan.Present = append(plan.Present, fmt.Sprintf("layout: %s already has a role (version %d)", site, version))
	} else {
		capacity := cfg.Storage.Garage.Capacity
		plan.Steps = append(plan.Steps, Step{
			Describe: fmt.Sprintf("assign %s a role in the cluster layout", site),
			Command:  fmt.Sprintf("%s layout assign -z %s -c %s %s", garageCmd, site, capacity, nodeID),
		})
		plan.Steps = append(plan.Steps, Step{
			Describe: fmt.Sprintf("apply the layout at version %d", version+1),
			Command:  fmt.Sprintf("%s layout apply --version %d", garageCmd, version+1),
		})
	}

	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if !kinds.UsesObjectStorage(app.Kind) {
			continue
		}
		keyID, _ := secretString(secrets, name, "s3_access_key_id")
		secretKey, _ := secretString(secrets, name, "s3_secret_access_key")
		bucket := bucketName(app, name)

		keyOut, keyErr := t.Run(garageCmd + " key info " + keyID)
		keyAbsent, err := absent(keyOut, keyErr, "0 matching keys")
		if err != nil {
			return nil, fmt.Errorf("checking key %s on %s: %w", keyID, t.Describe(), err)
		}
		if keyAbsent {
			// The secret appears on this command line, which means it is
			// visible in `ps` on the host for the moment the command runs.
			// The alternative, writing it to a temporary file first and
			// having Garage read it from there, trades a moment in the
			// process table for a secret at rest on disk, and apply already
			// writes this same rendered secret to the host in its .env file,
			// so that is not a new exposure this package introduces.
			plan.Steps = append(plan.Steps, Step{
				Describe: fmt.Sprintf("import the S3 key for %s", name),
				Command:  fmt.Sprintf("%s key import %s %s --yes -n %s", garageCmd, keyID, secretKey, name),
			})
		} else {
			plan.Present = append(plan.Present, fmt.Sprintf("key: %s already has an S3 key", name))
		}

		bucketOut, bucketErr := t.Run(garageCmd + " bucket info " + bucket)
		bucketAbsent, err := absent(bucketOut, bucketErr, "Bucket not found")
		if err != nil {
			return nil, fmt.Errorf("checking bucket %s on %s: %w", bucket, t.Describe(), err)
		}
		if bucketAbsent {
			plan.Steps = append(plan.Steps, Step{
				Describe: fmt.Sprintf("create the bucket for %s", name),
				Command:  fmt.Sprintf("%s bucket create %s", garageCmd, bucket),
			})
		} else {
			plan.Present = append(plan.Present, fmt.Sprintf("bucket: %s already exists", bucket))
		}

		// bucket allow is idempotent, Garage accepts it whether or not the
		// grant already exists, so it is always planned rather than checked
		// first.
		plan.Steps = append(plan.Steps, Step{
			Describe: fmt.Sprintf("grant %s's key read/write/owner on its bucket", name),
			Command:  fmt.Sprintf("%s bucket allow --read --write --owner %s --key %s", garageCmd, bucket, keyID),
		})
	}

	return plan, nil
}

// secretString reads one string secret for an app, empty when unset. It
// mirrors internal/render/appview.go's Secret helper, which is unexported
// there.
func secretString(secrets *config.Secrets, app, key string) (string, bool) {
	m, ok := secrets.Apps[app]
	if !ok {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok
}

// Execute runs each step's command in order and stops at the first failure,
// naming the step that failed so an operator knows exactly where a half
// finished run left the node.
func Execute(plan *Plan, t Transport) error {
	for _, step := range plan.Steps {
		if _, err := t.Run(step.Command); err != nil {
			return fmt.Errorf("%s: %w", step.Describe, err)
		}
	}
	return nil
}
