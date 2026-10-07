//go:build standby_integration

// This file runs the rendered Pocket ID standby wrapper in the pinned Pocket
// ID image, with the image's own entrypoint and su-exec, and a fake
// /app/pocket-id bind mounted over the real binary. The fake refuses with the
// real refusal line a set number of times and then serves, or fails with
// something else, so the wrapper is judged on exactly the shell and tools the
// real container has (busybox from Alpine 3.24.1), not on the workstation's.
//
// The last test then runs two real instances against a Postgres container,
// with no fake at all, so the marker is proved against the binary and a clean
// stop is seen to hand over. It is the one to rerun when the image is bumped.
//
// Containers are named paisans-standby-* and removed in t.Cleanup. Run with:
//
//	go test -tags standby_integration -timeout 10m -run TestStandbyWrapper ./internal/render/
package render_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// fakePocketID stands in for /app/pocket-id. Each server start counts itself
// in /fake/count; `healthcheck` answers from /fake/serving. In refuse mode it
// prints the line v2.14.0 logs (bootstrap.go:126-128 through root.go:23) and
// exits 1 until it has refused FAKE_REFUSALS times, then serves until a
// SIGTERM, which it records. In fail mode it exits 2 with another error. In
// buried mode it prints the refusal and then 60 more lines, so the marker is
// no longer among the last 50, and exits 1.
const fakePocketID = `#!/bin/sh
if [ "${1:-}" = healthcheck ]; then
	[ -f /fake/serving ]
	exit $?
fi
n=$(cat /fake/count 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > /fake/count
refusal='time=2026-10-07T00:00:00Z level=ERROR msg="Failed to run pocket-id" error="it appears that there'"'"'s already one instance of Pocket ID running - running multiple replicas is not (yet) supported"'
case "$FAKE_MODE" in
refuse)
	if [ "$n" -le "$FAKE_REFUSALS" ]; then
		echo "$refusal"
		exit 1
	fi
	trap 'echo "fake: got TERM as $(id -u)"; touch /fake/term; rm -f /fake/serving; exit 0' TERM
	touch /fake/serving
	echo "fake: serving as $(id -u)"
	while :; do sleep 1 & wait $!; done
	;;
fail)
	echo "fake: the database is unreachable"
	exit 2
	;;
buried)
	echo "$refusal"
	i=0
	while [ $i -lt 60 ]; do echo "fake: noise $i"; i=$((i + 1)); done
	exit 1
	;;
esac
`

type standbyBox struct {
	t    *testing.T
	name string
	dir  string
}

func standbyImage(t *testing.T) string {
	if image := os.Getenv("PAISANS_STANDBY_IMAGE"); image != "" {
		return image
	}
	image, _ := kinds.DefaultImage("pocket-id", "app")
	return image
}

// startStandbyBox runs the wrapper the fixture renders for home-a, with the
// compose file's entrypoint, command and healthcheck, at test timing.
func startStandbyBox(t *testing.T, label string, env ...string) *standbyBox {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	wrapper, ok := planFiles(build(t))["home-a/srv/auth/paisans-standby.sh"]
	if !ok {
		t.Fatal("the fixture renders no wrapper for auth on home-a")
	}
	dir := t.TempDir()
	for name, content := range map[string]string{"paisans-standby.sh": wrapper, "pocket-id": fakePocketID} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(dir, "fake")
	if err := os.Mkdir(fake, 0o777); err != nil {
		t.Fatal(err)
	}
	os.Chmod(fake, 0o777)

	box := &standbyBox{t: t, name: fmt.Sprintf("paisans-standby-%s-%d", label, time.Now().UnixNano()), dir: dir}
	args := []string{"run", "-d", "--name", box.name, "--platform", "linux/amd64",
		"-v", dir + "/paisans-standby.sh:/paisans/pocket-id-standby.sh:ro",
		"-v", dir + "/pocket-id:/app/pocket-id:ro",
		"-v", fake + ":/fake",
		"--entrypoint", "/bin/sh",
		"--health-cmd", "[ -f /tmp/paisans-standby ] || /app/pocket-id healthcheck",
		"--health-interval", "1s", "--health-retries", "3", "--health-timeout", "5s",
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, standbyImage(t), "/paisans/pocket-id-standby.sh", "/app/docker/entrypoint.sh", "/app/pocket-id")
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", box.name).Run() })
	return box
}

func (b *standbyBox) inspect(format string) string {
	out, _ := exec.Command("docker", "inspect", "--format", format, b.name).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func (b *standbyBox) logs() string {
	out, _ := exec.Command("docker", "logs", b.name).CombinedOutput()
	return string(out)
}

func (b *standbyBox) exists(name string) bool {
	_, err := os.Stat(filepath.Join(b.dir, "fake", name))
	return err == nil
}

func (b *standbyBox) inStandby() bool {
	return exec.Command("docker", "exec", b.name, "test", "-f", "/tmp/paisans-standby").Run() == nil
}

// waitFor polls cond for up to limit, failing with what the container said.
func (b *standbyBox) waitFor(what string, limit time.Duration, cond func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	b.t.Fatalf("%s: not within %s. State %s, health %s. Logs:\n%s", what, limit,
		b.inspect("{{.State.Status}}"), b.inspect("{{if .State.Health}}{{.State.Health.Status}}{{end}}"), b.logs())
}

// stop is `docker stop` with a long grace, so a quick return proves the
// signal reached the child rather than Docker giving up and killing it.
func (b *standbyBox) stop() time.Duration {
	b.t.Helper()
	start := time.Now()
	if out, err := exec.Command("docker", "stop", "-t", "30", b.name).CombinedOutput(); err != nil {
		b.t.Fatalf("docker stop: %v\n%s", err, out)
	}
	return time.Since(start)
}

const standbyLine = "paisans-standby: another Pocket ID instance is active on this database"

// Refused twice, then serving: two standby lines, healthy throughout, and the
// state file gone once it serves.
func TestStandbyWrapperRetriesUntilItServes(t *testing.T) {
	b := startStandbyBox(t, "retry", "FAKE_MODE=refuse", "FAKE_REFUSALS=2", "PAISANS_STANDBY_RETRY=1")
	b.waitFor("serving after two refusals", 60*time.Second, func() bool { return b.exists("serving") })
	logs := b.logs()
	if n := strings.Count(logs, standbyLine); n != 2 {
		t.Errorf("%d standby lines, want 2:\n%s", n, logs)
	}
	if !strings.Contains(logs, "there's already one instance of Pocket ID running") {
		t.Errorf("the child's own refusal line did not reach the log:\n%s", logs)
	}
	if !strings.Contains(logs, "fake: serving as 1000") {
		t.Errorf("the child is not running as the image's user, so the image's entrypoint was bypassed:\n%s", logs)
	}
	if b.inStandby() {
		t.Error("the state file is still there while the instance serves")
	}
	b.waitFor("healthy while serving", 30*time.Second, func() bool { return b.inspect("{{.State.Health.Status}}") == "healthy" })

	took := b.stop()
	if took > 10*time.Second {
		t.Errorf("docker stop took %s; the signal did not reach the child", took)
	}
	if !b.exists("term") {
		t.Errorf("the child never got SIGTERM. Logs:\n%s", b.logs())
	}
	if code := b.inspect("{{.State.ExitCode}}"); code != "0" {
		t.Errorf("exit code %s after a clean stop, want the child's 0", code)
	}
	t.Logf("stop returned in %s; logs:\n%s", took, b.logs())
}

// Refused for good: in standby, healthy, and a stop interrupts the wait.
func TestStandbyWrapperIsHealthyOnStandbyAndStopsPromptly(t *testing.T) {
	b := startStandbyBox(t, "standby", "FAKE_MODE=refuse", "FAKE_REFUSALS=100000", "PAISANS_STANDBY_RETRY=600")
	b.waitFor("in standby", 60*time.Second, b.inStandby)
	b.waitFor("healthy in standby", 30*time.Second, func() bool { return b.inspect("{{.State.Health.Status}}") == "healthy" })
	took := b.stop()
	if took > 10*time.Second {
		t.Errorf("docker stop took %s; the retry wait was not interrupted", took)
	}
	if code := b.inspect("{{.State.ExitCode}}"); code != "0" {
		t.Errorf("exit code %s after a stop on standby, want 0", code)
	}
	t.Logf("stop on standby returned in %s", took)
}

// Any other failure is passed on, status and all, with no standby.
func TestStandbyWrapperPassesOnAnyOtherFailure(t *testing.T) {
	b := startStandbyBox(t, "fail", "FAKE_MODE=fail", "PAISANS_STANDBY_RETRY=1")
	b.waitFor("exited", 60*time.Second, func() bool { return b.inspect("{{.State.Status}}") == "exited" })
	if code := b.inspect("{{.State.ExitCode}}"); code != "2" {
		t.Errorf("exit code %s, want the child's 2", code)
	}
	logs := b.logs()
	if strings.Contains(logs, standbyLine) || !strings.Contains(logs, "the database is unreachable") {
		t.Errorf("unexpected log:\n%s", logs)
	}
}

// Only the last 50 lines count: a refusal buried under 60 later lines is not
// a standby.
func TestStandbyWrapperReadsOnlyTheLastFiftyLines(t *testing.T) {
	b := startStandbyBox(t, "buried", "FAKE_MODE=buried", "PAISANS_STANDBY_RETRY=1")
	b.waitFor("exited", 60*time.Second, func() bool { return b.inspect("{{.State.Status}}") == "exited" })
	if code := b.inspect("{{.State.ExitCode}}"); code != "1" {
		t.Errorf("exit code %s, want the child's 1", code)
	}
	logs := b.logs()
	if strings.Contains(logs, standbyLine) {
		t.Errorf("stood by on a marker 61 lines up:\n%s", logs)
	}
	if !strings.Contains(logs, "fake: noise 59") {
		t.Errorf("the child's output was not all streamed:\n%s", logs)
	}
}

// Two real Pocket ID instances against one Postgres: the second is refused
// by francis with the real text and stands by; a stop of the first hands
// over within a retry, because a clean stop deregisters. This is what proves
// the marker against the binary rather than against the spec's reading of
// it, so it is the test to run when the image is bumped.
func TestStandbyWrapperWithTwoRealInstances(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	wrapper := planFiles(build(t))["home-a/srv/auth/paisans-standby.sh"]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "paisans-standby.sh"), []byte(wrapper), 0o644); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	network, pg := "paisans-standby-net-"+suffix, "paisans-standby-pg-"+suffix
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker("network", "create", network)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	docker("run", "-d", "--name", pg, "--network", network, "--network-alias", "pg",
		"-e", "POSTGRES_USER=pid", "-e", "POSTGRES_PASSWORD=pid", "-e", "POSTGRES_DB=pid", "postgres:17-alpine")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", pg).Run() })
	deadline := time.Now().Add(60 * time.Second)
	for exec.Command("docker", "exec", pg, "pg_isready", "-U", "pid", "-d", "pid").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("postgres never became ready")
		}
		time.Sleep(time.Second)
	}

	start := func(name string) *standbyBox {
		box := &standbyBox{t: t, name: "paisans-standby-" + name + "-" + suffix, dir: dir}
		docker("run", "-d", "--name", box.name, "--network", network, "--platform", "linux/amd64",
			"-e", "APP_URL=http://localhost:1411",
			"-e", "ENCRYPTION_KEY=fixture-encryption-key-not-a-secret",
			"-e", "DB_CONNECTION_STRING=postgres://pid:pid@pg:5432/pid?sslmode=disable",
			"-e", "FILE_BACKEND=database",
			"-e", "PAISANS_STANDBY_RETRY=3",
			"-v", dir+"/paisans-standby.sh:/paisans/pocket-id-standby.sh:ro",
			"--entrypoint", "/bin/sh",
			"--health-cmd", "[ -f /tmp/paisans-standby ] || /app/pocket-id healthcheck",
			"--health-interval", "2s", "--health-retries", "3", "--health-timeout", "5s",
			standbyImage(t), "/paisans/pocket-id-standby.sh", "/app/docker/entrypoint.sh", "/app/pocket-id")
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", box.name).Run() })
		return box
	}
	serving := func(b *standbyBox) func() bool {
		return func() bool {
			return exec.Command("docker", "exec", b.name, "sh", "-c", "! test -f /tmp/paisans-standby && /app/pocket-id healthcheck").Run() == nil
		}
	}

	a := start("a")
	a.waitFor("the first instance serving", 3*time.Minute, serving(a))
	b := start("b")
	b.waitFor("the second instance on standby", 3*time.Minute, b.inStandby)
	b.waitFor("the standby healthy", time.Minute, func() bool { return b.inspect("{{.State.Health.Status}}") == "healthy" })
	if !strings.Contains(b.logs(), "already one instance of Pocket ID running") {
		t.Fatalf("the standby's log lacks the refusal:\n%s", b.logs())
	}

	took := a.stop()
	if !strings.Contains(a.logs(), "Unregistered actor host") {
		t.Errorf("the stopped instance did not deregister, so a takeover would wait out the deadline:\n%s", a.logs())
	}
	handover := time.Now()
	b.waitFor("the standby taking over", time.Minute, serving(b))
	t.Logf("stop of the active instance took %s; the standby served %s later", took, time.Since(handover).Round(time.Second))
}
