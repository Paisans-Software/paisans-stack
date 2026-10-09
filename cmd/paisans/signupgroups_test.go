package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// The fixture's Pocket ID, auth, declares signup_default_groups [members,
// newcomers], and the fixture secrets record both IDs as the fake holds them.
// The fake has no talk client while the fixture records one, so the client
// step refuses talk throughout; these tests read the group step's own verdict,
// groupsErr, rather than the apply's.

// authEnv renders the fixture with the step's secrets and returns auth's
// .env on home-a.
func authEnv(t *testing.T, c *clientStep) string {
	t.Helper()
	plan, err := render.Build(c.cfg, c.secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Files {
		if f.Path == "home-a/srv/paisans/f2a9/auth/.env" {
			return f.Content
		}
	}
	t.Fatal("no .env for auth on home-a")
	return ""
}

func recordedGroups(t *testing.T, path string) map[string]string {
	t.Helper()
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.PocketIDGroups
}

// Groups that exist under the recorded IDs plan nothing, send nothing and
// write nothing, so a re-run of a finished apply is quiet.
func TestSignupGroupsAlreadyRecordedPlanNothing(t *testing.T) {
	fake := withIDPFake(t)
	path := tempSecrets(t)
	before, _ := os.ReadFile(path)
	c := stepFor(t, "home-a", path)
	stdout, _ := captureOutput(t, func() {
		if err := c.ensure(true, false); err != nil {
			t.Fatal(err)
		}
	})
	if fake.mutated || c.steps != 0 {
		t.Errorf("mutated %t, %d step(s):\n%s", fake.mutated, c.steps, stdout)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("the secrets file was written")
	}
	if !strings.Contains(stdout, "present group newcomers (id fixture-group-newcomers-id), recorded as pocket_id_groups.newcomers") {
		t.Errorf("output:\n%s", stdout)
	}
	if c.groupsErr != nil {
		t.Error(c.groupsErr)
	}
}

// A dry run shows a missing group's create and its recording, and changes
// nothing.
func TestSignupGroupsDryRunShowsTheCreate(t *testing.T) {
	fake := withIDPFake(t)
	fake.groups = fake.groups[:2]
	path := tempSecrets(t)
	before, _ := os.ReadFile(path)
	c := stepFor(t, "home-a", path)
	stdout, _ := captureOutput(t, func() { _ = c.ensure(false, false) })
	want := `create group newcomers: POST /api/user-groups {"friendlyName":"newcomers","name":"newcomers"}, record its ID as pocket_id_groups.newcomers, then render auth's .env again and recreate it. The recorded ID fixture-group-newcomers-id is not a group on this instance`
	if !strings.Contains(stdout, want) {
		t.Errorf("output:\n%s", stdout)
	}
	if fake.mutated || c.steps == 0 {
		t.Errorf("mutated %t, %d step(s)", fake.mutated, c.steps)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a dry run wrote the secrets file")
	}
}

// A group missing from Pocket ID, as on a fresh database where the recorded
// ID is stale, is created, its new ID recorded and rendered, and a second run
// then plans nothing.
func TestSignupGroupsMissingAreCreatedRecordedAndRendered(t *testing.T) {
	fake := withIDPFake(t)
	fake.groups = fake.groups[:2]
	path := tempSecrets(t)
	c := stepFor(t, "home-a", path)
	if !strings.Contains(authEnv(t, c), "fixture-group-newcomers-id") {
		t.Fatal("the fixture does not render the stale ID to begin with")
	}
	stdout, _ := captureOutput(t, func() {
		if err := c.ensure(true, false); err != nil {
			t.Fatal(err)
		}
	})
	if c.groupsErr != nil {
		t.Fatalf("%v\n%s", c.groupsErr, stdout)
	}
	if !slices.Equal(fake.created, []string{"newcomers"}) {
		t.Errorf("created %v", fake.created)
	}
	if got := recordedGroups(t, path)["newcomers"]; got != "g-newcomers" {
		t.Errorf("recorded %q", got)
	}
	env := authEnv(t, c)
	if !strings.Contains(env, `SIGNUP_DEFAULT_USER_GROUP_IDS="[\"fixture-group-members-id\",\"g-newcomers\"]"`) {
		t.Errorf("the .env does not carry the new ID:\n%s", env)
	}

	fake.mutated = false
	before, _ := os.ReadFile(path)
	captureOutput(t, func() { _ = c.ensure(false, false) })
	if c.steps != 0 || fake.mutated {
		t.Errorf("a re-run planned %d step(s)", c.steps)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a re-run wrote the secrets file")
	}
}

// A group that exists under another ID than the recorded one, or with none
// recorded, is recorded as it is and not created again.
func TestSignupGroupsWithAnotherIDAreRecordedNotCreated(t *testing.T) {
	fake := withIDPFake(t)
	fake.groups[0].ID = "members-elsewhere"
	path := tempSecrets(t)
	secrets, _ := config.LoadSecrets(path)
	delete(secrets.PocketIDGroups, "newcomers")
	if err := config.WriteSecrets(path, secrets, nil); err != nil {
		t.Fatal(err)
	}
	// The fake's clients admit members by its ID; keep them consistent.
	for _, other := range fake.others {
		other.AllowedUserGroups = fake.groups[:2]
	}
	c := stepFor(t, "home-a", path)
	stdout, _ := captureOutput(t, func() {
		if err := c.ensure(true, false); err != nil {
			t.Fatal(err)
		}
	})
	if c.groupsErr != nil {
		t.Fatalf("%v\n%s", c.groupsErr, stdout)
	}
	if len(fake.created) != 0 {
		t.Errorf("created %v", fake.created)
	}
	got := recordedGroups(t, path)
	if got["members"] != "members-elsewhere" || got["newcomers"] != "fixture-group-newcomers-id" {
		t.Errorf("recorded %v", got)
	}
	for _, line := range []string{
		"record pocket_id_groups.members members-elsewhere in place of fixture-group-members-id",
		"record pocket_id_groups.newcomers fixture-group-newcomers-id, the ID of group newcomers",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("no line %q in:\n%s", line, stdout)
		}
	}
}

// A group a client step in the same dry run creates is not planned twice.
func TestSignupGroupsCreatedByAClientAreNotPlannedTwice(t *testing.T) {
	fake := withIDPFake(t)
	fake.groups = fake.groups[1:] // no members
	for _, other := range fake.others {
		other.AllowedUserGroups = []pocketid.Group{fake.groups[0]}
	}
	path := secretsWithout(t, "talk")
	c := stepFor(t, "home-a", path)
	stdout, _ := captureOutput(t, func() { _ = c.ensure(false, false) })
	_, groups, _ := strings.Cut(stdout, "signup default groups of auth")
	if strings.Contains(groups, "POST /api/user-groups") {
		t.Errorf("the signup group step plans a create of its own:\n%s", stdout)
	}
	if !strings.Contains(stdout, "record pocket_id_groups.members once a client step above has created group members") {
		t.Errorf("output:\n%s", stdout)
	}
}

// A Pocket ID that cannot be asked holds nothing back: recorded IDs are
// rendered as they are, and the apply does not fail for it.
func TestSignupGroupsWhenPocketIDCannotBeAsked(t *testing.T) {
	var asked []string
	lookWith(t, map[string]string{"home-a.local": "down", "home-b.local": "down"}, &asked)
	fake := withIDPFake(t)
	path := tempSecrets(t)
	c := stepFor(t, "home-a", path)
	stdout, _ := captureOutput(t, func() { _ = c.ensure(true, false) })
	if len(fake.commands) != 0 {
		t.Errorf("Pocket ID was called %d time(s)", len(fake.commands))
	}
	if !strings.Contains(stdout, "signup group members: Pocket ID not asked, recorded ID used") || !strings.Contains(stdout, "Its recorded ID fixture-group-members-id is rendered as it is") {
		t.Errorf("output:\n%s", stdout)
	}
	if err := c.result(); err != nil {
		t.Error(err)
	}
}

// A site that does not run Pocket ID has no signup groups to resolve, and
// --only without the Pocket ID app leaves them alone.
func TestSignupGroupsOnlyWherePocketIDRuns(t *testing.T) {
	withIDPFake(t)
	if c := stepFor(t, "watch", tempSecrets(t)); len(c.signupGroups) != 0 {
		t.Errorf("watch resolves %v", c.signupGroups)
	}
	if c := stepFor(t, "home-a", tempSecrets(t), "talk"); len(c.signupGroups) != 0 {
		t.Errorf("--only talk resolves %v", c.signupGroups)
	}
	if c := stepFor(t, "home-a", tempSecrets(t)); !slices.Equal(c.signupGroups, []string{"members", "newcomers"}) {
		t.Errorf("home-a resolves %v", c.signupGroups)
	}
}

// The two steps of an apply on a site running Pocket ID: the first pass
// starts it with the IDs recorded so far, the group step resolves the rest,
// and the second pass renders its .env again with them, which is what
// recreates it.
func TestApplyRendersPocketIDAgainWithTheResolvedGroups(t *testing.T) {
	fake := withIDPFake(t)
	fake.groups = fake.groups[:2]
	path := secretsWithout(t, "talk")
	c := stepFor(t, "home-a", path)
	var log passLog
	var envs []string
	pass := log.pass(t, c, "talk")
	plan := pass.plan
	pass.plan = func(hold []string, done []*apply.Plan) (*apply.Plan, error) {
		envs = append(envs, authEnv(t, c))
		return plan(hold, done)
	}
	var err error
	stdout, _ := captureOutput(t, func() { _, err = executeWithClients(c, pass) })
	if err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if err := c.result(); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(envs) != 2 {
		t.Fatalf("%d passes", len(envs))
	}
	if !strings.Contains(envs[0], "fixture-group-newcomers-id") || strings.Contains(envs[0], "g-newcomers") {
		t.Errorf("the first pass did not start Pocket ID with what was recorded:\n%s", envs[0])
	}
	if !strings.Contains(envs[1], `\"g-newcomers\"`) {
		t.Errorf("the second pass does not render the resolved ID:\n%s", envs[1])
	}
}
