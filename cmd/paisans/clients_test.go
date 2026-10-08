package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// The recorder is the one way a client's credentials reach the secrets file,
// for `oidc client create` and for apply. Written with no recipient, an
// encrypted file would come back as plaintext.
func TestSecretsRecorderRefusesAnEncryptedFileWithoutARecipient(t *testing.T) {
	path := tempSecrets(t)
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets.Encrypted = true
	rec := &secretsRecorder{app: "talk", path: path, secrets: secrets}
	err = rec.Record("c-1", "not-a-real-secret-0001")
	if err == nil || !strings.Contains(err.Error(), "names a recipient") {
		t.Fatalf("got %v", err)
	}
	if rec.wrote {
		t.Error("the recorder says it wrote")
	}
}

// passLog is a sitePass that renders the site as apply would, records what
// each pass held back and whether app rendered with its recorded client ID
// at that moment, and executes nothing.
type passLog struct {
	events []string
	// done is what each plan call was told the earlier passes did.
	done [][]*apply.Plan
	// plans is every plan returned, in order.
	plans []*apply.Plan
}

func (l *passLog) pass(t *testing.T, c *clientStep, app string) sitePass {
	t.Helper()
	return sitePass{
		plan: func(hold []string, done []*apply.Plan) (*apply.Plan, error) {
			l.done = append(l.done, done)
			rendered, err := render.Build(c.cfg, c.secrets)
			if err != nil {
				t.Fatal(err)
			}
			id := c.secrets.OIDCClients[app].ClientID
			has := false
			for _, f := range rendered.Files {
				if f.Path == c.site+"/srv/"+app+"/.env" && id != "" && strings.Contains(f.Content, id) {
					has = true
				}
			}
			l.events = append(l.events, fmt.Sprintf("plan hold=%s client=%t", strings.Join(hold, ","), has))
			p := &apply.Plan{Site: c.site}
			l.plans = append(l.plans, p)
			return p, nil
		},
		execute: func(*apply.Plan) error {
			l.events = append(l.events, "execute")
			return nil
		},
	}
}

// secretsWithout is a writable copy of the fixture secrets with the named
// apps' clients removed, as before anything created them.
func secretsWithout(t *testing.T, apps ...string) string {
	t.Helper()
	path := tempSecrets(t)
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range apps {
		delete(secrets.OIDCClients, app)
	}
	if err := config.WriteSecrets(path, secrets, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

// stepFor is apply's identity step for site, over the fixture configuration
// and the secrets at path.
func stepFor(t *testing.T, site, path string, only ...string) *clientStep {
	t.Helper()
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newClientStep(cfg, site, "", path, secrets, only)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatalf("%s has no identity step", site)
	}
	return c
}

// captureOutput runs fn and returns what it wrote to stdout and to stderr.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	stdout = captureStdout(t, func() {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stderr
		os.Stderr = w
		done := make(chan string, 1)
		go func() {
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			done <- buf.String()
		}()
		fn()
		w.Close()
		os.Stderr = saved
		stderr = <-done
	})
	return stdout, stderr
}

// On a site running Pocket ID, the first pass holds back every other app,
// Pocket ID answers, the client is created and recorded, and only then is
// the site planned again, with the app rendering the recorded client.
func TestApplyCreatesAndRecordsTheClientBeforeTheAppRenders(t *testing.T) {
	fake := withIDPFake(t)
	path := secretsWithout(t, "talk")
	c := stepFor(t, "home-a", path)
	var log passLog
	var err error
	stdout, stderr := captureOutput(t, func() {
		_, err = executeWithClients(c, log.pass(t, c, "talk"))
	})
	if err != nil {
		t.Fatalf("%v\n%s%s", err, stdout, stderr)
	}
	want := []string{"plan hold=blog,docs,gate,talk client=false", "execute", "plan hold= client=true", "execute"}
	if strings.Join(log.events, "\n") != strings.Join(want, "\n") {
		t.Errorf("passes:\n%s\nwant:\n%s", strings.Join(log.events, "\n"), strings.Join(want, "\n"))
	}
	if !fake.mutated {
		t.Error("Pocket ID was sent nothing")
	}
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	got := secrets.OIDCClients["talk"]
	if got.ClientID != "c-1" || len(got.ClientSecret) < 16 {
		t.Fatalf("recorded id %q and a %d character secret", got.ClientID, len(got.ClientSecret))
	}
	if len(fake.prefixes) != 1 || !strings.HasPrefix(got.ClientSecret, fake.prefixes[0]) {
		t.Error("Pocket ID was not sent the recorded secret")
	}
	if strings.Contains(stdout, got.ClientSecret) || strings.Contains(stderr, got.ClientSecret) {
		t.Error("the secret was printed")
	}
	if !strings.Contains(stdout, "recorded oidc_clients.talk.client_id") {
		t.Errorf("output:\n%s", stdout)
	}
	if err := c.result(); err != nil {
		t.Error(err)
	}
}

// A dry run probes, prints the client plan, and sends Pocket ID nothing.
func TestApplyDryRunSendsNoMutation(t *testing.T) {
	fake := withIDPFake(t)
	path := secretsWithout(t, "talk")
	before, _ := os.ReadFile(path)
	c := stepFor(t, "home-a", path)
	stdout, _ := captureOutput(t, func() { c.ensure(false, c.pocketIDHere()) })
	if !strings.Contains(stdout, "create client talk: POST /api/oidc/clients") {
		t.Errorf("the dry run does not show the client:\n%s", stdout)
	}
	if fake.mutated {
		t.Error("a dry run mutated Pocket ID")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a dry run wrote the secrets file")
	}
	if c.steps == 0 || len(c.heldApps()) != 0 {
		t.Errorf("planned %d step(s), held %v", c.steps, c.heldApps())
	}
}

// A Pocket ID that cannot be reached holds back an app with no client, and
// lets one with a recorded client go ahead on it. Neither fails the apply.
func TestApplyHoldsBackOnlyAppsWithoutAClientWhenPocketIDIsUnreachable(t *testing.T) {
	var asked []string
	lookWith(t, map[string]string{"home-a.local": "down", "home-b.local": "down"}, &asked)
	for _, tc := range []struct {
		name     string
		recorded bool
		hold     string
		says     string
	}{
		{"no client", false, "plan hold=status ", "skip      status:"},
		{"recorded client", true, "plan hold= ", "unchecked status:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := withIDPFake(t)
			path := secretsWithout(t, "status")
			if tc.recorded {
				secrets, _ := config.LoadSecrets(path)
				_ = secrets.Set("oidc_clients.status.client_id", "fixture-not-a-secret-status-id")
				_ = secrets.Set("oidc_clients.status.client_secret", "fixture-not-a-secret-status-secret")
				if err := config.WriteSecrets(path, secrets, nil); err != nil {
					t.Fatal(err)
				}
			}
			c := stepFor(t, "vm", path)
			var log passLog
			var err error
			stdout, _ := captureOutput(t, func() {
				_, err = executeWithClients(c, log.pass(t, c, "status"))
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(log.events) != 2 || !strings.HasPrefix(log.events[0], tc.hold) {
				t.Errorf("passes %v, want one that starts %q", log.events, tc.hold)
			}
			if !strings.Contains(stdout, tc.says) {
				t.Errorf("output lacks %q:\n%s", tc.says, stdout)
			}
			if len(fake.commands) != 0 {
				t.Error("an unreachable Pocket ID was called")
			}
			var result error
			after, _ := captureOutput(t, func() { result = c.result() })
			if result != nil {
				t.Errorf("an unreachable Pocket ID failed the apply: %v", result)
			}
			if !tc.recorded && !strings.Contains(after, "held back until Pocket ID answers: status") {
				t.Errorf("the end of the apply does not say what was held back:\n%s", after)
			}
		})
	}
}

// An existing client that differs from what the app needs refuses that app:
// the rest of the site is applied, and the apply fails at the end.
func TestApplyRefusesOnlyTheAppWhoseClientDiffers(t *testing.T) {
	fake := withIDPFake(t)
	fake.client = &pocketid.OIDCClient{ID: "fixture-not-a-secret-talk-id", Name: "talk", CallbackURLs: []string{"https://elsewhere.example.org/callback"}, PkceEnabled: true}
	c := stepFor(t, "home-a", tempSecrets(t))
	var log passLog
	var err error
	stdout, _ := captureOutput(t, func() {
		_, err = executeWithClients(c, log.pass(t, c, "talk"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(log.events) != 4 || !strings.HasPrefix(log.events[2], "plan hold=talk ") || log.events[3] != "execute" {
		t.Errorf("passes %v, want the rest of the site applied with talk held back", log.events)
	}
	if fake.mutated {
		t.Error("a refused client was changed")
	}
	if !strings.Contains(stdout, "refuse    talk:") {
		t.Errorf("output:\n%s", stdout)
	}
	if err := c.result(); err == nil || !strings.Contains(err.Error(), "talk") || !strings.Contains(err.Error(), "differs") {
		t.Errorf("result %v, want talk's refusal", err)
	}
}

// The secret is written before Pocket ID is sent it, so a file that cannot
// be written back encrypted is refused before Pocket ID is asked anything.
func TestApplyRefusesAnUnwritableSecretsFileBeforeContactingPocketID(t *testing.T) {
	fake := withIDPFake(t)
	path := secretsWithout(t, "talk")
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets.Encrypted = true
	_, err = newClientStep(cfg, "home-a", "", path, secrets, nil)
	if err == nil || !strings.Contains(err.Error(), "names a recipient") {
		t.Fatalf("got %v", err)
	}
	if len(fake.commands) != 0 {
		t.Error("Pocket ID was contacted")
	}
}

// --only naming no app that signs in through Pocket ID has no identity step.
func TestApplyHasNoIdentityStepWithoutAnAppThatSignsIn(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(fixtureSecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	c, err := newClientStep(cfg, "home-a", "", fixtureSecretsPath(), secrets, []string{"docs"})
	if err != nil || c != nil {
		t.Errorf("got %+v, %v", c, err)
	}
}

// The second pass is told what the first did, so it does not repeat an
// --overwrite or a --recreate the first carried out (apply.After).
func TestApplyTellsTheSecondPassWhatTheFirstDid(t *testing.T) {
	withIDPFake(t)
	c := stepFor(t, "home-a", secretsWithout(t, "talk"))
	var log passLog
	var err error
	captureOutput(t, func() { _, err = executeWithClients(c, log.pass(t, c, "talk")) })
	if err != nil {
		t.Fatal(err)
	}
	if len(log.done) != 2 || len(log.done[0]) != 0 || len(log.done[1]) != 1 || log.done[1][0] != log.plans[0] {
		t.Errorf("the passes were told %v, want nothing and then the first pass", log.done)
	}
}

// A Pocket ID on this site that never answers after the first pass holds
// back the apps without a client, and the rest of the site still applies.
func TestApplyHoldsBackWhenItsOwnPocketIDNeverAnswers(t *testing.T) {
	fake := withIDPFake(t)
	saved := checkOneActive
	checkOneActive = func(*config.Config, string, map[string]apply.Transport, io.Writer) error {
		return fmt.Errorf("pocket-id auth: no site has an active instance within 3m0s")
	}
	t.Cleanup(func() { checkOneActive = saved })
	c := stepFor(t, "home-a", secretsWithout(t, "talk"))
	var log passLog
	var err error
	stdout, _ := captureOutput(t, func() { _, err = executeWithClients(c, log.pass(t, c, "talk")) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plan hold=blog,docs,gate,talk client=false", "execute", "plan hold=talk client=false", "execute"}
	if strings.Join(log.events, "\n") != strings.Join(want, "\n") {
		t.Errorf("passes:\n%s", strings.Join(log.events, "\n"))
	}
	if !strings.Contains(stdout, "skip      talk:") || len(fake.commands) != 0 {
		t.Errorf("Pocket ID was called %d time(s), output:\n%s", len(fake.commands), stdout)
	}
	if err := c.result(); err != nil {
		t.Error(err)
	}
}

// apply creates a client only when nothing is recorded. Recorded credentials
// that Pocket ID does not hold, or holds under another client, are refused
// rather than replaced, and the operator is pointed at the command.
func TestApplyNeverReplacesRecordedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *pocketid.OIDCClient
	}{
		{"no client at Pocket ID", nil},
		{"another client ID", &pocketid.OIDCClient{ID: "c-other", Name: "talk", CallbackURLs: []string{"https://talk.example.org/oauth/oidc/verify"}, PkceEnabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := withIDPFake(t)
			fake.client = tc.client
			path := tempSecrets(t)
			before, _ := os.ReadFile(path)
			c := stepFor(t, "home-a", path)
			var err error
			captureOutput(t, func() { err = c.ensure(true, false) })
			if err != nil {
				t.Fatal(err)
			}
			if fake.mutated {
				t.Error("Pocket ID was changed")
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Error("the recorded credentials were replaced")
			}
			result := c.result()
			if result == nil || !strings.Contains(result.Error(), "paisans oidc client create --app talk") {
				t.Errorf("result %v, want a refusal naming the command", result)
			}
			if got := strings.Join(c.heldApps(), ","); got != "talk" {
				t.Errorf("held %s", got)
			}
		})
	}
}

// A plan that would record into a file that cannot be written back
// encrypted is refused before Pocket ID is sent anything.
func TestApplyRefusesARecordingStepBeforeAnyMutation(t *testing.T) {
	fake := withIDPFake(t)
	c := stepFor(t, "home-a", secretsWithout(t, "talk"))
	c.secrets.Encrypted = true
	var err error
	captureOutput(t, func() { err = c.ensure(true, false) })
	if err == nil || !strings.Contains(err.Error(), "names a recipient") {
		t.Fatalf("got %v", err)
	}
	if fake.mutated {
		t.Error("Pocket ID was changed")
	}
}

// --ssh replaces the applying site's ssh section for every look at it,
// including the one that finds the active Pocket ID.
func TestActiveInstanceReachesTheApplyingSiteThroughSSH(t *testing.T) {
	var reached []string
	saved := standbyLook
	standbyLook = func(tr apply.SSHTransport) apply.Transport {
		reached = append(reached, tr.Describe())
		var asked []string
		return stateHost{"active", &asked, tr.Describe()}
	}
	t.Cleanup(func() { standbyLook = saved })
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = activeInstance(cfg, "auth", "home-a", "operator@jump.example.org")
	if !slices.Contains(reached, "operator@jump.example.org") || slices.Contains(reached, "ubuntu@home-a.local") {
		t.Errorf("reached %v", reached)
	}
}

// A refusal from the site lookup names the command it came from.
func TestAdminSiteNamesItsCaller(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminSite(cfg, "talk", "vm", "apply"); err == nil || !strings.HasPrefix(err.Error(), "apply: ") {
		t.Errorf("got %v", err)
	}
}
