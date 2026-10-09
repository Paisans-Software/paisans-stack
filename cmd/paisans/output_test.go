package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/appremove"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// Default output is for reading at a glance: plain when piped, and no line
// long enough to be an explanation. Run over the dry runs and executions the
// command tests already drive against their fakes.
func TestDefaultOutputIsShortAndPlain(t *testing.T) {
	runs := fixtureRuns(t)
	var names []string
	for name := range runs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out := runs[name]
		if testing.Verbose() {
			t.Logf("%s:\n%s", name, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s: printed nothing, so the run was not driven", name)
		}
		if strings.ContainsAny(out, "\x1b\r") {
			t.Errorf("%s: escapes in piped output", name)
		}
		for _, line := range strings.Split(out, "\n") {
			// A failure and a refusal's explanation say why in full: the
			// operator needs the reason to act, and a second run for it
			// would be worse than a long line.
			if len(line) > 100 && !strings.HasPrefix(strings.TrimSpace(line), "FAIL") && !strings.HasPrefix(line, "       ") {
				t.Errorf("%s: long line in default output (%d): %q", name, len(line), line)
			}
		}
	}
}

// runDefault runs fn with the default reporter (not verbose) writing to a
// buffer, and returns what that reporter and any direct print to stdout
// showed. A direct print is measured too, so a command that skips the
// reporter cannot hide a long line.
func runDefault(t *testing.T, name string, fn func() error) string {
	t.Helper()
	var b strings.Builder
	reporterOverride = ui.NewPlain(&b, false)
	defer func() { reporterOverride = nil; apply.SetRetryLog(nil) }()
	var err error
	direct := captureStdout(t, func() { err = fn() })
	if err != nil {
		// A refusal is output too, and the error itself is printed whole by
		// main; the lines measured here are the reporter's.
		t.Logf("%s ended with: %v", name, err)
	}
	return b.String() + direct
}

// quietSSH puts an ssh on PATH that connects and answers as a clean, empty
// host, so a command that reaches one reports what it would do there.
func quietSSH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// The remote command travels base64 encoded (apply.RemoteCommand), so the
	// script decodes it to answer the few probes whose silence would be read
	// as a failure: who it runs as, which OS, what volumes there are, and
	// that a file is absent.
	script := `#!/bin/sh
for last; do :; done
b=${last#*printf %s }
b=${b%% |*}
cmd=$(printf %s "$b" | base64 -d 2>/dev/null)
case "$cmd" in
*"for n in "*) l=${cmd#*for n in }; l=${l%%; do*}; for n in $l; do echo "absent $(printf %s "$n" | tr -d "'")"; done;;
*"for r in "*) l=${cmd#*for r in }; l=${l%%; do*}
	for r in $l; do
		r=$(printf %s "$r" | tr -d "'")
		case "$cmd" in *".Config.Volumes"*) echo "volumes $r null";; *) echo "present sha256:0123456789abcdef $r";; esac
	done;;
*"getent passwd"*) echo "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash";;
*"id -u") echo 0;;
*"print-architecture"*) echo "arch amd64";;
*"/etc/os-release"*) printf 'ID=ubuntu\nVERSION_ID="24.04"\nVERSION_CODENAME=noble\n';;
*"docker volume ls"*) echo end;;
*__PAISANS_NO_ETCD__*) echo __PAISANS_NO_ETCD__;;
*__PAISANS_ABSENT__*) echo __PAISANS_ABSENT__;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runningHost is a host whose stacks come up: `ps` finds a running container,
// so the gate after each stack passes and an execution carries on.
type runningHost struct{ failingHost }

func (h runningHost) Run(command string) (string, error) {
	if strings.HasSuffix(command, " ps --all --format json") {
		return `{"Service":"app","Name":"app-1","State":"running","Health":""}` + "\n", nil
	}
	if strings.Contains(command, "ip -j ") {
		return "[]\n", nil
	}
	if strings.HasSuffix(command, "id -u") {
		return "0\n", nil
	}
	if strings.Contains(command, "__PAISANS_NO_ETCD__") {
		return "__PAISANS_NO_ETCD__\n", nil
	}
	if strings.Contains(command, ":8008/cluster") {
		return `{"members":[{"name":"home-a","role":"leader","state":"running"}]}`, nil
	}
	return h.failingHost.Run(command)
}

// fixtureRuns drives the commands against their fakes, by default verbosity,
// and returns what each showed. Commands run from a directory of their own
// with short file names, as an operator runs them, so a long temporary path
// is not mistaken for a long sentence.
func fixtureRuns(t *testing.T) map[string]string {
	t.Helper()
	runs := map[string]string{}
	fixture, secretsFixture := mustAbs(t, fixtureConfig()), mustAbs(t, fixtureSecretsPath())

	// init reads the example declaration relative to the package, so it
	// runs first, in a subtest whose directory change ends with it.
	t.Run("init", func(t *testing.T) {
		path, _ := initWorld(t, nil, fakeSites(), make([]byte, 64))
		t.Chdir(filepath.Dir(path))
		runs["init"] = runDefault(t, "init", func() error { return runInit([]string{"--config", "paisans.yaml", "--sudo=false"}) })
	})

	work := t.TempDir()
	t.Chdir(work)
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	secretsBody, err := os.ReadFile(secretsFixture)
	if err != nil {
		t.Fatal(err)
	}
	write("paisans.yaml", string(body))
	write("secrets.yaml", string(secretsBody))
	cfg, secrets := "paisans.yaml", "secrets.yaml"

	// With a public address declared, dns init gets as far as the provider
	// check, which is the last place it can refuse offline.
	withAddress := strings.Replace(string(body), "    endpoint: vm.example.org:51820\n", "    endpoint: vm.example.org:51820\n    public_address: 203.0.113.10\n", 1)
	write("dns.yaml", withAddress)

	// site add joins a site with the data role only, and every site dials
	// the others directly, so the fixture is declared that way for it.
	dialled := strings.Replace(withAddress, "  home-a:\n    roles: [data, apps]\n", "  home-a:\n    roles: [data, apps]\n    endpoint: 192.0.2.1:51820\n", 1)
	joined := strings.Replace(dialled, "  home-b:\n    roles: [data, apps]", "  home-b:\n    roles: [data]", 1)
	if joined == dialled || dialled == withAddress {
		t.Fatal("the fixture no longer declares the sites this edit expects")
	}
	write("join.yaml", joined)

	runs["validate"] = runDefault(t, "validate", func() error { return runValidate([]string{"--config", cfg}) })
	runs["dns init"] = runDefault(t, "dns init", func() error { return runDNSInit([]string{"--config", "dns.yaml", "--secrets", secrets}) })

	{
		// failingHost with a match nothing contains answers everything an
		// empty host would, ps included.
		host := runningHost{failingHost{match: "\x00"}}
		var b strings.Builder
		r := ui.NewPlain(&b, false)
		presentPlan(r, freshPlan(t, r, host), false, map[string]bool{})
		runs["apply dry run"] = b.String()
		b.Reset()
		executed := freshPlan(t, r, host)
		presentPlan(r, executed, true, map[string]bool{})
		if err := apply.Execute(executed, host); err != nil {
			t.Fatal(err)
		}
		runs["apply execute"] = b.String()
		b.Reset()
		failed := freshPlan(t, r, failOn("infra/compose.yaml up -d", "Error response from daemon: port is already allocated"))
		_ = apply.Execute(failed, failOn("infra/compose.yaml up -d", "Error response from daemon: port is already allocated"))
		runs["apply execute failing"] = b.String()
	}

	{
		host := danglingHost{listing: "volume\t3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191\t48234496\t{\"community.paisans.deployment\":\"f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01\"}\tcache log\n" +
			"volume\tother_db\t9999\t{\"com.docker.compose.project\":\"other\"}\tpgdata\nend\n"}
		plan, err := apply.BuildVolumePrune(deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}, "home-a", host)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		showVolumePrune(ui.NewPlain(&b, false), plan)
		runs["prune plan"] = b.String()
	}

	{
		fake := withIDPFake(t)
		_ = fake
		runs["oidc client create dry run"] = runDefault(t, "oidc client create", func() error {
			return runOIDCClientCreate([]string{"--config", cfg, "--secrets", secrets, "--app", "talk"})
		})
	}

	{
		withPIDFake(t)
		runs["app admin create dry run"] = runDefault(t, "app admin create", func() error {
			return runAppAdminCreate([]string{"--config", cfg, "--secrets", secrets, "--app", "auth", "--username", "founder", "--email", "founder@example.org", "--first-name", "Fern"}, strings.NewReader(""))
		})
	}

	{
		var sent, writes []string
		withDoctorHosts(t, stuckHosts(mustLoad(t, cfg), &sent, &writes))
		runs["doctor"] = runDefault(t, "doctor", func() error { return runDoctor([]string{"--config", cfg, "--sudo=false"}) })
	}

	{
		savedSite, savedApp := removeSiteHost, removeHost
		t.Cleanup(func() { removeSiteHost, removeHost = savedSite, savedApp })
		quiet := runningHost{failingHost{match: "\x00"}}
		removeSiteHost = func(string, config.Site, bool) apply.Transport { return quiet }
		removeHost = func(string, config.Site, bool) appremove.Host { return quiet }
		runs["site remove dry run"] = runDefault(t, "site remove", func() error {
			return runSiteRemove([]string{"watch", "--config", cfg, "--secrets", secrets, "--sudo=false"}, strings.NewReader(""), io.Discard)
		})
		runs["app remove dry run"] = runDefault(t, "app remove", func() error {
			return runAppRemove([]string{"old", "--config", cfg, "--secrets", secrets, "--sudo=false"}, strings.NewReader(""), io.Discard)
		})
	}

	// The commands that reach a host through ssh meet an ssh that answers
	// as a clean, empty host.
	quietSSH(t)
	for name, args := range map[string][]string{
		"host prepare dry run": {"host prepare", "--config", cfg, "--site", "home-a", "--sudo=false"},
		"site add dry run":     {"site add", "home-b", "--config", "join.yaml", "--secrets", secrets, "--sudo=false"},
		"storage add dry run":  {"storage add", "--config", cfg, "--secrets", secrets, "--sudo=false"},
		"prune dry run":        {"prune", "--config", cfg, "--site", "home-a", "--sudo=false"},
	} {
		run := map[string]func([]string) error{
			"host prepare": runHostPrepare, "site add": runSiteAdd, "storage add": runStorageAdd, "prune": runPrune,
		}[args[0]]
		runs[name] = runDefault(t, name, func() error { return run(args[1:]) })
	}
	return runs
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
