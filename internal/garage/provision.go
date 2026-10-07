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
	"errors"
	"fmt"
	"regexp"
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

// Command is the prefix every command in this package runs through: the
// Garage binary inside its own container, on the infra stack this toolkit
// renders. It matches how apply reaches into the same stack for other
// commands, so an operator reading a transcript sees one shape throughout.
const Command = "docker compose -f /srv/infra/compose.yaml exec -T garage /garage"

// Step is one command this plan still needs to run, in order.
type Step struct {
	// Describe is one line, shown to the operator before the command runs.
	// It never carries a credential, so it is the only field of this struct
	// that is safe to print unconditionally.
	Describe string
	// Command is the garage command, exactly as it will run. It may carry a
	// credential, so nothing prints it, logs it, or puts it in an error.
	Command string
	// Secret, when set, is a value inside Command that must never reach a
	// terminal, a scrollback buffer or a log. Execute strips it from anything
	// a failure produces.
	//
	// It is a value rather than a pre redacted copy of Command because the
	// leak is not confined to the command text. apply.SSHTransport.Run embeds
	// the command in its error, Garage quotes an offending argument back in
	// some of its own messages, and a caller may wrap either. Redacting the
	// value covers all three; redacting a copy of the command covers only the
	// first.
	Secret string
}

// redactionMarker is what a stripped secret is replaced by. It names itself so
// that an operator reading a failure knows a value was removed rather than
// wondering whether the command was malformed.
const redactionMarker = "[redacted]"

// redact removes every occurrence of secret from text. An empty secret redacts
// nothing, which is what makes it safe to call on every step.
func redact(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, redactionMarker)
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

// BucketName is the bucket an app's objects live in: a declared s3_bucket
// setting, or <app>-uploads. It is kinds.BucketName, which internal/render
// also reads, because the bucket this package creates has to be the same one
// the rendered application is told to use. Exported for storage add.
func BucketName(app config.App, name string) string {
	return kinds.BucketName(name, app)
}

// Build checks what already exists on the target site's node and returns a
// plan of only what is missing, in the order it must run.
//
// The layout check is per node, not per cluster: reading only "is the layout
// version 0" would find version 1 on a cluster this node has not joined and
// conclude, wrongly, that it is done. So this checks whether this node's own
// ID appears in `layout show`'s rows. It lays a node out only when it is the
// one Garage site; a node joining a cluster of several is refused and sent to
// `paisans storage add` (internal/storageadd), which joins them all at once.
func Build(site string, cfg *config.Config, secrets *config.Secrets, t Transport) (*Plan, error) {
	if !slices.Contains(cfg.Storage.Garage.Sites, site) {
		return nil, fmt.Errorf("garage: %s holds no Garage role. Sites with one are %s", site, strings.Join(cfg.Storage.Garage.Sites, ", "))
	}

	plan := &Plan{Site: site}

	out, err := t.Run(Command + " layout show")
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
	nodeOut, err := t.Run(Command + " node id -q")
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

	hasRole := shortNodeID != "" && strings.Contains(layoutTable(out), shortNodeID)
	switch {
	case hasRole:
		plan.Present = append(plan.Present, fmt.Sprintf("layout: %s already has a role (version %d)", site, version))
	case len(cfg.Storage.Garage.Sites) > 1:
		// A node joining a cluster is `storage add`'s: it connects the
		// nodes, assigns every role in one layout version, and waits for the
		// data to move. Assigning one node here, as this used to, either
		// fails (`layout apply` refuses fewer nodes than the replication
		// factor) or, at replication 1, starts a second cluster that never
		// meets the first. Nothing after the layout can work without it:
		// every key and bucket command needs a role to reach quorum.
		return nil, fmt.Errorf("garage: %s has no role in the cluster layout, and storage.garage.sites lists %d sites. Joining a node is `paisans storage add`, which lays out every site at once; run it, then storage init has nothing left to do", site, len(cfg.Storage.Garage.Sites))
	default:
		capacity := cfg.Storage.Garage.Capacity
		plan.Steps = append(plan.Steps, Step{
			Describe: fmt.Sprintf("assign %s a role in the cluster layout", site),
			Command:  fmt.Sprintf("%s layout assign -z %s -c %s %s", Command, site, capacity, nodeID),
		})
		plan.Steps = append(plan.Steps, Step{
			Describe: fmt.Sprintf("apply the layout at version %d", version+1),
			Command:  fmt.Sprintf("%s layout apply --version %d", Command, version+1),
		})
	}

	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if !kinds.UsesObjectStorage(app.Kind) {
			continue
		}
		keyID, _ := SecretString(secrets, name, "s3_access_key_id")
		secretKey, _ := SecretString(secrets, name, "s3_secret_access_key")
		bucket := BucketName(app, name)

		missing, err := keyAbsent(t, keyID)
		if err != nil {
			return nil, err
		}
		if missing {
			plan.Steps = append(plan.Steps, ImportStep(name, keyID, secretKey))
		} else {
			plan.Present = append(plan.Present, fmt.Sprintf("key: %s already has an S3 key", name))
		}

		state, err := ReadBucket(t, bucket)
		if err != nil {
			return nil, err
		}
		bucketAbsent := state.Absent
		if bucketAbsent {
			plan.Steps = append(plan.Steps, Step{
				Describe: fmt.Sprintf("create the bucket for %s", name),
				Command:  fmt.Sprintf("%s bucket create %s", Command, bucket),
			})
		} else {
			plan.Present = append(plan.Present, fmt.Sprintf("bucket: %s already exists", bucket))
		}

		// Grant and website access are read off `bucket info` and planned
		// only when missing. They used to be planned on every run, because
		// no real `bucket info` output had been seen to parse; it has now
		// (dxflrs/garage:v1.0.1, `bucket info talk-uploads` on a provisioned
		// node), and it prints `Website access: true` and an `Authorized
		// keys:` section with one `RWO  <key ID>  <key name>` row per key.
		// Both commands are sets, so replanning them was harmless, but a
		// plan that always has steps never says "nothing to do". Output that
		// does not parse plans both, and says so: running a set twice costs
		// nothing, while skipping a grant that is missing leaves the app
		// unable to write.
		info := state.info
		unread := ""
		if !bucketAbsent && !info.parsed {
			unread = ", planned because `bucket info` could not be read"
		}

		if info.parsed && info.grants(keyID) {
			plan.Present = append(plan.Present, fmt.Sprintf("grant: %s's key already has read/write/owner on %s", name, bucket))
		} else {
			step := GrantStep(name, bucket, keyID)
			step.Describe += unread
			plan.Steps = append(plan.Steps, step)
		}

		if kinds.ServesObjectsPublicly(app.Kind) {
			// Website access is what lets a request with no credential read
			// this bucket. `garage bucket website --allow` is a set rather
			// than a create: established by running it twice against a real
			// dxflrs/garage:v1.0.1 container, which printed the identical
			// "Website access allowed for <bucket>" and exited 0 both times.
			//
			// Nothing here ever revokes it. A bucket that stops being public
			// is a change to the kinds catalogue, which is a code change with
			// a review, and a provisioner that guessed a human meant to
			// withdraw public access would be guessing about the one thing in
			// this package that cannot be undone quietly.
			if info.parsed && info.website {
				plan.Present = append(plan.Present, fmt.Sprintf("website: %s already allows website access", bucket))
			} else {
				plan.Steps = append(plan.Steps, Step{
					Describe: fmt.Sprintf("allow website access on %s's bucket%s", name, unread),
					Command:  fmt.Sprintf("%s bucket website --allow %s", Command, bucket),
				})
			}
		}
	}

	return plan, nil
}

// keyAbsent asks Garage whether a key with this ID exists. Garage matches the
// argument against every key's ID by prefix and against its name exactly
// (src/model/key_table.rs, KeyFilter::MatchesAndNotDeleted, at v1.0.1), so a
// full ID matches only its own key, and a deleted key is absent.
func keyAbsent(t Transport, keyID string) (bool, error) {
	out, err := t.Run(Command + " key info " + keyID)
	absentNow, err := absent(out, err, "0 matching keys")
	if err != nil {
		return false, fmt.Errorf("checking key %s on %s: %w", keyID, t.Describe(), err)
	}
	return absentNow, nil
}

// KeyPresent reports whether Garage holds a live key with this ID. Exported
// for storage rotate-key, which checks both the key it retires and the one
// replacing it.
func KeyPresent(t Transport, keyID string) (bool, error) {
	gone, err := keyAbsent(t, keyID)
	return !gone, err
}

// ImportStep imports one app's S3 key under the app's name.
//
// `garage key import` takes the secret as a positional argument and offers no
// other form, so it is visible in `ps` on the host for the moment the command
// runs. That much is unavoidable here, and it is bounded: apply already writes
// this same secret to the host in the app's .env, so a reader of the process
// table learns nothing they could not read off the disk.
//
// The exposure that is NOT bounded, and that an earlier version of this
// comment missed entirely by reasoning only about `ps`, is the operator's own
// machine. A failing import produced an error carrying the command, and that
// error goes to a terminal: into a scrollback buffer, into a terminal
// multiplexer's log, into a CI job's recorded output, and into whatever a
// screen recording caught. None of those are on the host and none are bounded
// by who can already read the .env. Secret on the step is what keeps a failure
// from putting the value in any of them.
//
// Feeding it over stdin the way apply.WriteFile does would not help.
// WriteFile works because `cat > $tmp` never has the content in argv at all;
// here the value has to end up as an argument to the garage process whatever
// route it takes to get there, so stdin would move the same string through a
// shell and leave the `ps` exposure exactly where it was, while fixing only the
// error text that Secret already fixes.
func ImportStep(app, keyID, secretKey string) Step {
	return Step{
		Describe: fmt.Sprintf("import the S3 key %s for %s", keyID, app),
		Command:  fmt.Sprintf("%s key import %s %s --yes -n %s", Command, keyID, secretKey, app),
		Secret:   secretKey,
	}
}

// GrantStep grants one key read, write and owner on an app's bucket. The
// command is a set, so running it on a key that already holds the grant
// changes nothing.
func GrantStep(app, bucket, keyID string) Step {
	return Step{
		Describe: fmt.Sprintf("grant %s's key %s read/write/owner on its bucket", app, keyID),
		Command:  fmt.Sprintf("%s bucket allow --read --write --owner %s --key %s", Command, bucket, keyID),
	}
}

// DeleteKeyStep deletes one key by its full ID. The syntax is
// dxflrs/garage:v1.0.1's: `key delete <key_pattern> --yes`, where the pattern
// is an ID prefix or an exact name, more than one match is refused with "N
// matching keys", and without --yes nothing is deleted
// (src/garage/cli/structs.rs, KeyDeleteOpt; src/garage/admin/key.rs,
// handle_delete_key). Deleting a key also removes its grant on every bucket
// (src/model/helper/locked.rs, delete_key). A deleted key's ID can never be
// imported again (handle_import_key refuses any ID the key table holds, even
// deleted), which is why storage rotate-key only ever deletes the key it is
// retiring, and only after the new one is proven.
func DeleteKeyStep(keyID string) Step {
	return Step{
		Describe: fmt.Sprintf("delete the S3 key %s from Garage", keyID),
		Command:  fmt.Sprintf("%s key delete --yes %s", Command, keyID),
	}
}

// Bucket is what `garage bucket info` said about one bucket, for a caller
// outside this package. Exported for storage rotate-key.
type Bucket struct {
	// Absent is set when Garage has no bucket by this name.
	Absent bool
	info   bucketInfo
}

// Readable reports whether the output parsed. An unreadable answer grants
// nothing, so a caller asking Grants about one plans the grant.
func (b Bucket) Readable() bool { return b.info.parsed }

// Grants reports whether the key with this ID holds read, write and owner.
func (b Bucket) Grants(keyID string) bool { return b.info.parsed && b.info.grants(keyID) }

// ReadBucket asks Garage about one bucket.
func ReadBucket(t Transport, bucket string) (Bucket, error) {
	out, err := t.Run(Command + " bucket info " + bucket)
	gone, err := absent(out, err, "Bucket not found")
	if err != nil {
		return Bucket{}, fmt.Errorf("checking bucket %s on %s: %w", bucket, t.Describe(), err)
	}
	if gone {
		return Bucket{Absent: true}, nil
	}
	return Bucket{info: parseBucketInfo(out)}, nil
}

// bucketInfo is what `garage bucket info` says about website access and
// grants.
type bucketInfo struct {
	// parsed is set only when both the website line and the authorized keys
	// section were found. Anything less is read as "unknown", not "absent".
	parsed  bool
	website bool
	// keys are the authorized keys' rows: permissions, key ID, key name.
	keys [][3]string
}

// grants reports whether the key with this ID holds all of read, write and
// owner.
//
// The ID is the only thing matched. A key's name is a label Garage does not
// keep unique, and `storage rotate-key` imports the new key under the app's
// name while the old one still holds its grant under the same name; matching
// either would read the new key as granted and leave the app with no access
// once the old key is deleted. `bucket info` prints the ID in every row, so
// nothing is lost by ignoring the name.
func (b bucketInfo) grants(keyID string) bool {
	for _, row := range b.keys {
		if row[1] != keyID {
			continue
		}
		if strings.Contains(row[0], "R") && strings.Contains(row[0], "W") && strings.Contains(row[0], "O") {
			return true
		}
	}
	return false
}

// ansi matches the colour codes Garage's log lines carry.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// parseBucketInfo reads `garage bucket info` as dxflrs/garage:v1.0.1 prints
// it. The combined output can open with log lines (the RPC client's
// "Connection established", ANSI coloured), so only a line that itself starts
// with a field name counts, and the authorized keys are the indented rows
// directly under their heading, ending at the first blank or unindented line.
func parseBucketInfo(out string) bucketInfo {
	var info bucketInfo
	var sawWebsite, sawKeys, inKeys bool
	for _, raw := range strings.Split(ansi.ReplaceAllString(out, ""), "\n") {
		line := strings.TrimRight(raw, "\r ")
		if inKeys {
			if strings.TrimSpace(line) == "" || (line[0] != ' ' && line[0] != '\t') {
				inKeys = false
			} else {
				fields := strings.Fields(line)
				var row [3]string
				copy(row[:], fields)
				info.keys = append(info.keys, row)
				continue
			}
		}
		switch {
		case strings.HasPrefix(line, "Website access:"):
			sawWebsite = true
			info.website = strings.TrimSpace(strings.TrimPrefix(line, "Website access:")) == "true"
		case strings.HasPrefix(line, "Authorized keys:"):
			sawKeys, inKeys = true, true
		}
	}
	info.parsed = sawWebsite && sawKeys
	return info
}

// SecretString reads one string secret for an app, empty when unset. It
// mirrors internal/render/appview.go's Secret helper, which is unexported
// there.
func SecretString(secrets *config.Secrets, app, key string) (string, bool) {
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
//
// A failure's error is rebuilt rather than wrapped, and the wrap chain is
// given up deliberately. The transport's own error embeds the command it ran,
// which for a key import is the S3 secret itself, so wrapping it with %w would
// keep the secret reachable through errors.Unwrap and, more to the point,
// print it the moment anything formats the chain. Nothing in this package or
// its callers inspects a transport error with errors.Is or errors.As, so the
// chain buys nothing here and costs a credential in a log.
func Execute(plan *Plan, t Transport) error {
	for _, step := range plan.Steps {
		out, err := t.Run(step.Command)
		if err == nil {
			continue
		}
		detail := redact(err.Error(), step.Secret)
		if trimmed := strings.TrimSpace(redact(out, step.Secret)); trimmed != "" && !strings.Contains(detail, trimmed) {
			detail += ": " + trimmed
		}
		return errors.New(step.Describe + ": " + detail)
	}
	return nil
}
