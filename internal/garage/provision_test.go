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
func TestAFreshNodeIsLaidOutBeforeAnyKeyIsImported(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "==== CURRENT CLUSTER LAYOUT ====\nNo nodes currently have a role in the cluster.\n\nCurrent cluster layout version: 0\n"},
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
func TestAProvisionedNodePlansNothingButTheGrant(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "==== CURRENT CLUSTER LAYOUT ====\nID  Tags  Zone  Capacity\nabc  []  home-a  100.0 GB\n\nCurrent cluster layout version: 1\n"},
		// This node's own ID is a row in the layout already (it is the "abc"
		// above), which is what makes this node fully provisioned rather than
		// merely a cluster that has some layout.
		"node id -q":  {out: "abc@127.0.0.1:3901\n"},
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
