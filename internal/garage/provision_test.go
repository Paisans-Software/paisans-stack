package garage_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/garage"
)

// fakeTransport answers canned output per command prefix, so the whole planner
// is testable without a host or a container.
type fakeTransport struct {
	responses map[string]response // keyed by a substring of the command
	ran       []string
}

type response struct {
	out string
	err error
}

func (f *fakeTransport) Run(command string) (string, error) {
	f.ran = append(f.ran, command)
	for key, r := range f.responses {
		if strings.Contains(command, key) {
			return r.out, r.err
		}
	}
	return "", nil
}

func (f *fakeTransport) Describe() string { return "fake" }

func fixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture configuration: %v", err)
	}
	return cfg
}

func fixtureSecrets(t *testing.T) *config.Secrets {
	t.Helper()
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture secrets: %v", err)
	}
	return secrets
}

func indexOfContaining(commands []string, substr string) int {
	for i, c := range commands {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// A fresh node needs the layout first. Before one is applied Garage answers
// every key and bucket command with "Could not reach quorum of 1", so a plan
// that ordered them the other way would fail on a real host and pass here
// unless the order is asserted.
//
// The layout show output carries the RPC client's own connection preamble,
// "Connection established to <node ID>", ahead of the table, exactly as
// dxflrs/garage:v1.0.1 actually prints it. That line names this node's own
// short ID whether or not it has a role yet, because on a single node
// cluster the client always connects to itself to run the command. A check
// that matched the node ID against the whole combined output, preamble
// included, would read that line as proof the node already has a role and
// plan no layout at all, on a cluster that in fact has none. This fixture is
// what catches that: the table itself still says "No nodes currently have a
// role", and the plan must still contain a layout assign and apply.
func TestAFreshNodeIsLaidOutBeforeAnyKeyIsImported(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Connection established to 51494feb5444d466\n==== CURRENT CLUSTER LAYOUT ====\nNo nodes currently have a role in the cluster.\n\nCurrent cluster layout version: 0\n"},
		"node id -q":  {out: "51494feb5444d466aaaabbbbccccddddeeeeffff00001111222233334444abcd@127.0.0.1:3901\n"},
		"key info":    {out: "Error: 0 matching keys", err: errors.New("exit status 1")},
		"bucket info": {out: "Error: Bucket not found / several matching buckets: talk-uploads", err: errors.New("exit status 1")},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	var commands []string
	for _, s := range plan.Steps {
		commands = append(commands, s.Command)
	}
	joined := strings.Join(commands, "\n")
	assign := indexOfContaining(commands, "layout assign")
	apply := indexOfContaining(commands, "layout apply")
	importKey := indexOfContaining(commands, "key import")
	if assign < 0 || apply < 0 || importKey < 0 {
		t.Fatalf("a fresh node needs all three, got:\n%s", joined)
	}
	if !(assign < apply && apply < importKey) {
		t.Errorf("layout must be applied before any key is imported, got:\n%s", joined)
	}
	if !strings.Contains(joined, "layout apply --version 1") {
		t.Errorf("a layout at version 0 is applied as version 1, got:\n%s", joined)
	}
	if !strings.Contains(joined, "-c 100G") {
		t.Errorf("capacity comes from the configuration and Garage requires a unit suffix, got:\n%s", joined)
	}
}

// key import and bucket create both fail when the object exists, so a second
// run must not plan them. bucket allow is idempotent and is always planned.
//
// `node id -q` returns the full 64 character node ID, but `layout show`'s
// table prints only its first 16 characters, exactly as dxflrs/garage:v1.0.1
// actually does. A check that compared the full ID against the table would
// never find this row, no matter how long the cluster had been provisioned,
// and would replan the layout on every single run. This fixture is what
// catches that: node id -q returns the full ID below, the table row carries
// only its first 16 characters, and the two have to be recognised as the
// same node for this test to see nothing planned. The layout show output
// also carries the connection preamble real Garage prints ahead of the
// table, so this fixture exercises both the truncation and the preamble at
// once, the way a real run would.
func TestAProvisionedNodePlansNothingButTheGrant(t *testing.T) {
	const fullNodeID = "bce014be16f81a9033b33dbb8ba6cc4c528353dd045d73aed6ef0c807389129a"
	const shortNodeID = "bce014be16f81a90" // fullNodeID's first 16 characters
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Connection established to " + shortNodeID + "\n==== CURRENT CLUSTER LAYOUT ====\nID                Tags  Zone    Capacity\n" + shortNodeID + "        []    home-a  100.0 GB\n\nCurrent cluster layout version: 1\n"},
		"node id -q":  {out: fullNodeID + "@127.0.0.1:3901\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Bucket: cfc236316d4a81858f84f84c287f5a0d\nSize: 0 B\nObjects: 0\n"},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	for _, s := range plan.Steps {
		if strings.Contains(s.Command, "key import") || strings.Contains(s.Command, "bucket create") || strings.Contains(s.Command, "layout") {
			t.Errorf("nothing was missing, so this should not have been planned: %s", s.Command)
		}
	}
	if len(plan.Present) == 0 {
		t.Error("a fully provisioned node should report what it found rather than looking like it did nothing")
	}
}

// The half provisioned node is what a failed first run leaves behind, and it
// is the case a check-then-act planner exists for.
func TestAHalfProvisionedNodePlansOnlyWhatIsMissing(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Current cluster layout version: 1\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Error: Bucket not found / several matching buckets: talk-uploads", err: errors.New("exit status 1")},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	joined := ""
	for _, s := range plan.Steps {
		joined += s.Command + "\n"
	}
	if strings.Contains(joined, "key import") {
		t.Errorf("the key was already there and importing it again fails, got:\n%s", joined)
	}
	if !strings.Contains(joined, "bucket create") {
		t.Errorf("the bucket was missing and should be planned, got:\n%s", joined)
	}
}

// A transport failure is not an absent object. Treating every non zero exit as
// "not there yet" would plan an import against a node that is simply
// unreachable, and then run it.
func TestAnUnreachableNodeIsAnErrorRatherThanAnEmptyPlan(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"": {out: "ssh: connect to host home-a port 22: Connection refused", err: errors.New("exit status 255")},
	}}

	if _, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport); err == nil {
		t.Fatal("expected a connection failure to be reported rather than treated as an unprovisioned node")
	}
}

// This is the ruling that overrides the brief's literal wording: the fixture
// deployment declares two Garage sites, home-a and home-b, at replication 2.
// Layout detection has to be per node, not per cluster. A layout already at
// version 1 (home-a's role was assigned by an earlier run) still leaves
// home-b without a role, and home-b's node ID is absent from the "layout
// show" rows. A planner that only checked the layout version against 0 would
// see version 1, plan nothing, and leave home-b serving nothing while
// reporting success.
func TestASecondSiteIsAssignedEvenWhenTheLayoutIsAlreadyAtVersionOne(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "==== CURRENT CLUSTER LAYOUT ====\nID        Tags  Zone    Capacity\n51494feb  []    home-a  100.0 GB\n\nCurrent cluster layout version: 1\n"},
		"node id -q":  {out: "aabbccdd5444d466aaaabbbbccccddddeeeeffff00001111222233334444abcd@127.0.0.1:3901\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Bucket: cfc236316d4a81858f84f84c287f5a0d\nSize: 0 B\nObjects: 0\n"},
	}}

	plan, err := garage.Build("home-b", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	var commands []string
	for _, s := range plan.Steps {
		commands = append(commands, s.Command)
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "layout assign") {
		t.Fatalf("home-b has no role in a layout that only lists home-a's node, so it must be assigned, got:\n%s", joined)
	}
	if !strings.Contains(joined, "aabbccdd") {
		t.Errorf("the assign step must carry home-b's own node ID, got:\n%s", joined)
	}
	if !strings.Contains(joined, "-z home-b") {
		t.Errorf("the assign step must carry home-b's zone, got:\n%s", joined)
	}
	if !strings.Contains(joined, "layout apply --version 2") {
		t.Errorf("a layout already at version 1 is applied as version 2, got:\n%s", joined)
	}
}

// vm, in the fixture, is a declared site holding roles gateway and witness
// and is not in storage.garage.sites. Building a plan for it must be refused
// before any transport call, not answered with a plan that assigns it a
// cluster layout role: vm is the etcd witness, and a layout assign against it
// would hand the wrong machine a role the deployment never gave it.
func TestASiteWithNoGarageRoleIsRefused(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"": {out: "should never be reached", err: errors.New("a transport call was made for a non Garage site")},
	}}

	_, err := garage.Build("vm", fixtureConfig(t), fixtureSecrets(t), transport)
	if err == nil {
		t.Fatal("expected a site absent from storage.garage.sites to be refused")
	}
	if !strings.Contains(err.Error(), "vm") {
		t.Errorf("the refusal should name the site, got: %v", err)
	}
	if !strings.Contains(err.Error(), "home-a") || !strings.Contains(err.Error(), "home-b") {
		t.Errorf("the refusal should list the sites that do hold a Garage role, got: %v", err)
	}
	if len(transport.ran) != 0 {
		t.Errorf("the refusal must happen before any transport call, but ran: %v", transport.ran)
	}
}

// Website access is what makes an object readable with no credential, and it
// is per bucket. Mbin's bucket needs it. Outline's must never have it, because
// its bucket holds the attachments of documents only members can read.
func TestOnlyAPublicBucketIsAllowedWebsiteAccess(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Connection established to 51494feb5444d466\n==== CURRENT CLUSTER LAYOUT ====\nID  Tags  Zone  Capacity\n51494feb5444d466  []  home-a  100.0 GB\n\nCurrent cluster layout version: 1\n"},
		"node id -q":  {out: "51494feb5444d466aaaabbbbccccddddeeeeffff00001111222233334444abcd@10.44.0.1:3901\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Bucket: cfc236316d4a81858f84f84c287f5a0d\nSize: 0 B\nObjects: 0\n"},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	var website []string
	for _, s := range plan.Steps {
		if strings.Contains(s.Command, "bucket website") {
			website = append(website, s.Command)
		}
	}
	joined := strings.Join(website, "\n")
	if !strings.Contains(joined, "bucket website --allow talk-uploads") {
		t.Errorf("mbin's bucket needs website access or its media stays unreadable, got:\n%s", joined)
	}
	if strings.Contains(joined, "docs-uploads") {
		t.Errorf("outline's bucket must never be world readable, got:\n%s", joined)
	}
}

// Garage cannot allow website access on a bucket that does not exist, so the
// website step for a given bucket must come after that bucket's create step.
func TestWebsiteAccessIsPlannedAfterTheBucketExists(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Connection established to 51494feb5444d466\n==== CURRENT CLUSTER LAYOUT ====\nNo nodes currently have a role in the cluster.\n\nCurrent cluster layout version: 0\n"},
		"node id -q":  {out: "51494feb5444d466aaaabbbbccccddddeeeeffff00001111222233334444abcd@127.0.0.1:3901\n"},
		"key info":    {out: "Error: 0 matching keys", err: errors.New("exit status 1")},
		"bucket info": {out: "Error: Bucket not found / several matching buckets: talk-uploads", err: errors.New("exit status 1")},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	commands := commandsOf(plan)
	create := indexOfContaining(commands, "bucket create talk-uploads")
	website := indexOfContaining(commands, "bucket website --allow talk-uploads")
	if create < 0 || website < 0 {
		t.Fatalf("expected both a bucket create and a website allow step for talk-uploads, got:\n%s", strings.Join(commands, "\n"))
	}
	if !(create < website) {
		t.Errorf("website access must be planned after the bucket exists, got create at %d and website at %d:\n%s", create, website, strings.Join(commands, "\n"))
	}
}

// Execute stops at the first failing step and names it, rather than carrying
// on and leaving a node in a state nobody can describe.
//
// This was deferred once as coverage for its own sake. Writing it is what
// surfaced the finding below: the step that fails first in a real run is the
// key import, and its error carried the S3 secret.
func TestExecuteStopsAtTheFirstFailingStepAndNamesIt(t *testing.T) {
	plan := &garage.Plan{
		Site: "home-a",
		Steps: []garage.Step{
			{Describe: "first step", Command: "docker compose -f /srv/infra/compose.yaml exec -T garage /garage layout assign"},
			{Describe: "second step", Command: "docker compose -f /srv/infra/compose.yaml exec -T garage /garage layout apply"},
			{Describe: "third step", Command: "docker compose -f /srv/infra/compose.yaml exec -T garage /garage bucket create"},
		},
	}
	transport := &fakeTransport{responses: map[string]response{
		"layout apply": {out: "Error: could not apply the layout", err: errors.New("exit status 1")},
	}}

	err := garage.Execute(plan, transport)
	if err == nil {
		t.Fatal("a failing step must be an error, not a silent partial run")
	}
	if !strings.Contains(err.Error(), "second step") {
		t.Errorf("the error must name the step that failed, so an operator knows where the run stopped. Got: %v", err)
	}
	if len(transport.ran) != 2 {
		t.Errorf("Execute must stop at the first failure and run 2 commands, not %d: %v", len(transport.ran), transport.ran)
	}
	if indexOfContaining(transport.ran, "bucket create") >= 0 {
		t.Error("the step after the failing one must not run")
	}
}

// A failing `key import` must not put the S3 secret in the error.
//
// The transport's own error embeds the command it ran, which is how
// apply.SSHTransport reports a failure, and that command carries the secret as
// a positional argument because `garage key import` takes no other form. The
// error goes straight to the operator's terminal, so without this it lands in
// a scrollback buffer, a multiplexer's log and a CI job's recorded output.
// Garage also quotes an offending argument back in some of its messages, so
// the command output is checked too rather than only the command text.
func TestAFailingKeyImportDoesNotPutTheSecretInItsError(t *testing.T) {
	cfg := fixtureConfig(t)
	secrets := fixtureSecrets(t)
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "==== CURRENT CLUSTER LAYOUT ====\nno nodes\nCurrent cluster layout version: 0\n"},
		"node id -q":  {out: "0123456789abcdef0123456789abcdef@10.44.0.1:3901\n"},
		"key info":    {out: "Key not found: 0 matching keys", err: errors.New("exit status 1")},
		"bucket info": {out: "Bucket not found", err: errors.New("exit status 1")},
	}}

	plan, err := garage.Build("home-a", cfg, secrets, transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	i := indexOfContaining(commandsOf(plan), "key import")
	if i < 0 {
		t.Fatal("a fresh node should plan a key import; without one this test proves nothing")
	}
	step := plan.Steps[i]
	if step.Secret == "" {
		t.Fatal("the key import step must carry its secret so Execute can strip it from a failure")
	}
	if !strings.Contains(step.Command, step.Secret) {
		t.Fatal("the secret this step declares must be the one in its command, or redaction removes the wrong string")
	}

	// A transport that fails the way apply.SSHTransport does: the error text
	// carries the whole command, and Garage echoes the argument it rejected.
	failing := &failingImportTransport{inner: transport}
	execErr := garage.Execute(plan, failing)
	if execErr == nil {
		t.Fatal("a failing key import must be an error")
	}
	if strings.Contains(execErr.Error(), step.Secret) {
		t.Errorf("a failing key import put the S3 secret in its error, where it lands in a scrollback buffer and a log. Got: %v", execErr)
	}
	if !strings.Contains(execErr.Error(), "import the S3 key") {
		t.Errorf("redacting must not cost the operator the name of the step that failed. Got: %v", execErr)
	}
}

// failingImportTransport answers every command from an inner transport except
// `key import`, which fails the way apply.SSHTransport does: with an error
// that embeds the command, and with output that quotes the rejected argument.
type failingImportTransport struct {
	inner *fakeTransport
}

func (f *failingImportTransport) Describe() string { return "failing" }

func (f *failingImportTransport) Run(command string) (string, error) {
	if !strings.Contains(command, "key import") {
		return f.inner.Run(command)
	}
	out := "Error: could not import key " + command + "\n"
	return out, errors.New("host: " + command + ": exit status 1\n" + out)
}

func commandsOf(plan *garage.Plan) []string {
	var out []string
	for _, s := range plan.Steps {
		out = append(out, s.Command)
	}
	return out
}
