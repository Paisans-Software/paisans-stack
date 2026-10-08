//go:build writefreely_integration

// This file boots the pinned writefreely-wisp image against the config.ini
// this toolkit renders, and asks the application, not the file, what value a
// passthrough key ended up with.
//
// It exists because a key in the right section textually is not the same as a
// key the application reads. The ini writer was built against go-ini's
// documented behaviour and checked against go-ini itself; this is the check
// against the software that actually runs.
//
// It has its own build tag rather than garage_integration because nothing in
// it touches Garage, and the helpers it needs live beside the fixture in this
// package rather than in internal/garage.
//
// Every container, the network and the volume it creates are removed in
// t.Cleanup, and a `go test` timeout skips that: the timeout panics the test
// binary, and no cleanup function runs. The docker host is shared with other
// work, so leftovers named paisans-wf-* are left for someone else to find. The
// test may pull images and boots the application three times, so give it a
// generous limit rather than the 10 minute default, for example:
//
//	go test -tags writefreely_integration -timeout 30m -run TestRenderedPassthroughKeyIsReadByWriteFreely ./internal/render/
package render_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	// blogDir is where the fixture's writefreely app renders, on the site it
	// is pinned to.
	blogDir = "home-a/srv/paisans/f2a9/blog/"
	// passthroughKey is the fixture's ini passthrough key, as `config` spells
	// it and as `writefreely settings get` names it.
	passthroughKey = "app.max_blogs"
)

// wfCluster is one network with a Postgres reachable under the host name the
// rendered config.ini names, so the file can be used exactly as rendered.
type wfCluster struct {
	network  string
	postgres string
	data     string
	image    string
}

// requireDockerForWriteFreely skips rather than fails without docker, so the
// tag degrades on a workstation that lacks it instead of looking like a bug.
func requireDockerForWriteFreely(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH; skipping the WriteFreely integration test")
	}
}

// renderedImage reads one service's image out of the rendered compose file, so
// this test runs what an operator receives and cannot drift when a pin moves.
func renderedImage(t *testing.T, compose, service string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(service) + `:\n    image: (\S+)`)
	m := re.FindStringSubmatch(compose)
	if m == nil {
		t.Fatalf("the rendered blog compose file names no image for %s:\n%s", service, compose)
	}
	return m[1]
}

// renderedValue reads `<key>: <value>` or `<key>=<value>` out of a rendered
// file, so the database this test starts carries the credentials the rendered
// config.ini expects rather than a copy of them typed here.
func renderedValue(t *testing.T, file, content, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)` + pattern).FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("the rendered %s does not match %q:\n%s", file, pattern, content)
	}
	return m[1]
}

// dockerRun runs a docker command and fails the test with its output if it
// does not succeed.
func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// removeContainer is what every cleanup here calls. -v matters: both images
// the test starts may declare anonymous volumes, and `rm -f` alone leaves them
// behind on the host.
func removeContainer(name string) {
	exec.Command("docker", "rm", "-f", "-v", name).Run()
}

// startWFCluster creates the network and a Postgres on it, aliased to the
// host name the rendered config.ini connects to, with the database, user and
// password the rendered compose.yaml and .env give it.
func startWFCluster(t *testing.T, files map[string]string, stamp string) *wfCluster {
	t.Helper()
	compose := files[blogDir+"compose.yaml"]
	env := files[blogDir+".env"]
	ini := files[blogDir+"config.ini"]
	if compose == "" || env == "" || ini == "" {
		t.Fatalf("the fixture rendered no complete blog under %s", blogDir)
	}

	c := &wfCluster{
		network:  "paisans-wf-" + stamp,
		postgres: "paisans-wf-pg-" + stamp,
		data:     "paisans-wf-data-" + stamp,
		image:    renderedImage(t, compose, "app"),
	}
	pgImage := renderedImage(t, compose, "postgres")
	dbHost := renderedValue(t, "config.ini", ini, `^host = (\S+)$`)
	dbName := renderedValue(t, "compose.yaml", compose, `POSTGRES_DB: (\S+)`)
	dbUser := renderedValue(t, "compose.yaml", compose, `POSTGRES_USER: (\S+)`)
	dbPass := renderedValue(t, ".env", env, `^POSTGRES_PASSWORD=(\S+)$`)

	// Registered before any container, so LIFO cleanup removes the containers
	// first and the network last: a network with a container attached cannot
	// be removed.
	t.Cleanup(func() { exec.Command("docker", "network", "rm", c.network).Run() })
	dockerRun(t, "network", "create", c.network)

	// The fork's /data, which the rendered compose.yaml bind mounts from the
	// host. It has to outlive one container: the entrypoint treats absent
	// encryption keys as a first run and initializes the schema, which fails
	// against a database that already has one. A named volume rather than a
	// bind mount, because the container writes it as root and the test has to
	// be able to delete it.
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", c.data).Run() })
	dockerRun(t, "volume", "create", c.data)

	t.Cleanup(func() { removeContainer(c.postgres) })
	dockerRun(t, "run", "-d", "--name", c.postgres,
		"--network", c.network, "--network-alias", dbHost,
		"-e", "POSTGRES_DB="+dbName,
		"-e", "POSTGRES_USER="+dbUser,
		"-e", "POSTGRES_PASSWORD="+dbPass,
		pgImage)

	// TCP rather than the socket: the image's init phase runs a server with
	// listen_addresses='' and then restarts it, so only a TCP answer means the
	// real server is up.
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := exec.Command("docker", "exec", c.postgres,
			"pg_isready", "-h", "127.0.0.1", "-U", dbUser, "-d", dbName).Run()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", c.postgres).CombinedOutput()
			t.Fatalf("Postgres did not accept TCP connections within 60s:\n%s", logs)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return c
}

// startBlog starts the fork against one config.ini, mounted read only at the
// path the rendered compose.yaml mounts it, over the cluster's /data volume,
// and waits until it serves.
func (c *wfCluster) startBlog(t *testing.T, name, iniPath string) {
	t.Helper()
	t.Cleanup(func() { removeContainer(name) })
	dockerRun(t, "run", "-d", "--name", name, "--network", c.network,
		"-v", c.data+":/data",
		"-v", iniPath+":/data/config.ini:ro", c.image)

	deadline := time.Now().Add(90 * time.Second)
	for {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Contains(string(out), "Serving on http://") {
			return
		}
		if state, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output(); err == nil && strings.TrimSpace(string(state)) == "false" {
			t.Fatalf("the fork exited rather than serving. Its output was:\n%s", out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fork did not serve within 90s. Its output was:\n%s", out)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// settingGet asks the running application for one setting. Only stdout is
// the value: the fork sends the settings command's log lines to stderr.
func settingGet(t *testing.T, container, key string) string {
	t.Helper()
	cmd := exec.Command("docker", "exec", container, "writefreely", "settings", "get", key)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("writefreely settings get %s: %v\n%s", key, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func logsOf(container string) string {
	out, _ := exec.Command("docker", "logs", container).CombinedOutput()
	return string(out)
}

// TestRenderedPassthroughKeyIsReadByWriteFreely is the one check against real
// software the passthrough design asks for, in three parts.
//
// The first is the claim: a fresh instance started against the rendered
// config.ini reports the passthrough value. The second is the control that
// makes the first mean something: the same file with only the passthrough
// line removed reports something else, so the readback is reading the file
// and not a constant. The third is a property of the fork rather than of this
// toolkit, recorded here because the README states it: the fork moves its
// settings into the database on first start, so a key added to an existing
// instance's config.ini is logged as a disagreement and not applied.
func TestRenderedPassthroughKeyIsReadByWriteFreely(t *testing.T) {
	requireDockerForWriteFreely(t)

	cfg := fixture(t)
	want := fmt.Sprint(cfg.Apps["blog"].Config[passthroughKey])
	if want == "<nil>" {
		t.Fatalf("the fixture's blog no longer sets config.%s, so there is nothing to prove", passthroughKey)
	}

	files := planFiles(build(t))
	ini := files[blogDir+"config.ini"]

	// The control is the rendered file less exactly one line: the one the
	// passthrough added. Anything else removed would make it a different
	// file, and the comparison would prove less.
	line := passthroughKey[strings.Index(passthroughKey, ".")+1:] + " = " + want + "\n"
	if n := strings.Count(ini, line); n != 1 {
		t.Fatalf("the rendered config.ini carries %q %d times, want once:\n%s", line, n, ini)
	}
	without := strings.Replace(ini, line, "", 1)

	dir := t.TempDir()
	withPath := filepath.Join(dir, "with.config.ini")
	withoutPath := filepath.Join(dir, "without.config.ini")
	for path, content := range map[string]string{withPath: ini, withoutPath: without} {
		// 0644 rather than the 0600 apply writes: the container's user is not
		// this test's, and the mode is not under test.
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	stamp := fmt.Sprint(time.Now().UnixNano())

	t.Run("a fresh instance reports the passthrough value", func(t *testing.T) {
		c := startWFCluster(t, files, stamp+"-with")
		app := "paisans-wf-app-" + stamp + "-with"
		c.startBlog(t, app, withPath)
		if !strings.Contains(logsOf(app), "settings from config.ini into the database") {
			t.Errorf("the fork did not report importing config.ini's settings:\n%s", logsOf(app))
		}
		got := settingGet(t, app, passthroughKey)
		t.Logf("writefreely settings get %s = %s", passthroughKey, got)
		if got != want {
			t.Fatalf("the fork reports %s = %q from the rendered config.ini, want %q. The key is in the file and the application did not read it.", passthroughKey, got, want)
		}
	})

	t.Run("without the passthrough line the value differs, and adding it later is not applied", func(t *testing.T) {
		c := startWFCluster(t, files, stamp+"-without")

		first := "paisans-wf-app-" + stamp + "-without"
		c.startBlog(t, first, withoutPath)
		control := settingGet(t, first, passthroughKey)
		t.Logf("writefreely settings get %s without the line = %s", passthroughKey, control)
		if control == want {
			t.Fatalf("the fork reports %s = %q with the passthrough line removed, the same as with it, so the readback does not prove the line was read", passthroughKey, control)
		}

		// The same database, now started against the file that carries the
		// key. The fork already holds its settings, so it should keep its own
		// value and say that config.ini disagrees.
		removeContainer(first)
		second := "paisans-wf-app-" + stamp + "-later"
		c.startBlog(t, second, withPath)
		later := settingGet(t, second, passthroughKey)
		t.Logf("writefreely settings get %s after adding the line to an existing instance = %s", passthroughKey, later)
		if later != control {
			t.Errorf("adding the key to an existing instance changed %s from %q to %q. The README says the fork ignores it there; if the fork changed, so must the README.", passthroughKey, control, later)
		}
		if !strings.Contains(logsOf(second), "disagrees with the database on "+passthroughKey) {
			t.Errorf("the fork did not log that config.ini disagrees with the database on %s:\n%s", passthroughKey, logsOf(second))
		}
	})
}
