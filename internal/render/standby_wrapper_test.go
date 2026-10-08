package render_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These run the rendered standby wrapper under the workstation's sh with a
// fake child, for what the state file does across retries. The busybox shell
// the real image has is covered by standby_integration_test.go.

// wrapperChild stands in for the image's entrypoint and command. It records,
// for every start, whether the state file was present, so a test can see what
// the instance probe would have read during a retry. It refuses with the real
// refusal text until it has refused CHILD_REFUSALS times, then serves until a
// SIGTERM; it becomes ready (the healthcheck passes) unless CHILD_NEVER_READY
// is set.
const wrapperChild = `#!/bin/sh
dir=$CHILD_DIR
n=$(cat "$dir/count" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "$dir/count"
if [ -f "$dir/paisans-standby" ]; then s=present; else s=absent; fi
if [ -f "$dir/shared-standby" ]; then sh=shared; else sh=unshared; fi
echo "$n $s $sh" >> "$dir/seen"
if [ "$n" -le "$CHILD_REFUSALS" ]; then
	echo 'error="it appears that there'"'"'s already one instance of Pocket ID running"'
	exit 1
fi
trap 'rm -f "$dir/serving"; exit 0' TERM
[ -n "${CHILD_NEVER_READY:-}" ] || touch "$dir/serving"
while :; do sleep 1 & wait $!; done
`

// wrapperReady stands in for `/app/pocket-id healthcheck`.
const wrapperReady = `#!/bin/sh
[ -f "$CHILD_DIR/serving" ]
`

type wrapperRun struct {
	t   *testing.T
	dir string
	cmd *exec.Cmd
}

func startWrapper(t *testing.T, env ...string) *wrapperRun {
	t.Helper()
	wrapper, ok := planFiles(build(t))["home-a/srv/paisans/f2a9/auth/paisans-standby.sh"]
	if !ok {
		t.Fatal("the fixture renders no wrapper for auth on home-a")
	}
	dir := t.TempDir()
	if !strings.Contains(wrapper, "/app/pocket-id healthcheck") {
		t.Fatal("the wrapper does not ask the image's healthcheck whether the instance is ready")
	}
	wrapper = strings.ReplaceAll(wrapper, "/tmp/paisans-standby", dir+"/paisans-standby")
	wrapper = strings.ReplaceAll(wrapper, "/paisans/run/standby", dir+"/shared-standby")
	wrapper = strings.ReplaceAll(wrapper, "/app/pocket-id healthcheck", dir+"/ready")
	for name, content := range map[string]string{"wrapper.sh": wrapper, "child": wrapperChild, "ready": wrapperReady} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", filepath.Join(dir, "wrapper.sh"), filepath.Join(dir, "child"))
	cmd.Env = append(os.Environ(), append([]string{"CHILD_DIR=" + dir, "PAISANS_STANDBY_RETRY=0"}, env...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &wrapperRun{t: t, dir: dir, cmd: cmd}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return r
}

func (r *wrapperRun) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.dir, name))
	return err == nil
}

func (r *wrapperRun) waitFor(what string, limit time.Duration, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	seen, _ := os.ReadFile(filepath.Join(r.dir, "seen"))
	r.t.Fatalf("%s: not within %s. Starts:\n%s", what, limit, seen)
}

func (r *wrapperRun) stop() {
	r.t.Helper()
	r.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			r.t.Errorf("the wrapper exited with %v after a stop, want 0", err)
		}
	case <-time.After(10 * time.Second):
		r.t.Fatal("the wrapper did not exit after a stop")
	}
}

// A standby keeps its state file through each retry, so the instance probe
// never reads a waiting standby as down; once the instance serves, the file
// goes, so an active instance does not read as standby.
func TestStandbyWrapperKeepsTheStateFileAcrossRetries(t *testing.T) {
	r := startWrapper(t, "CHILD_REFUSALS=3")
	r.waitFor("serving after three refusals", 20*time.Second, func() bool { return r.exists("serving") })
	r.waitFor("the state file gone once ready", 5*time.Second, func() bool { return !r.exists("paisans-standby") })
	if r.exists("shared-standby") {
		t.Error("the shared marker outlives the state file once the instance serves")
	}
	seen, _ := os.ReadFile(filepath.Join(r.dir, "seen"))
	want := "1 absent unshared\n2 present shared\n3 present shared\n4 present shared\n"
	if string(seen) != want {
		t.Errorf("state file at each start:\n%s\nwant:\n%s", seen, want)
	}
	r.stop()
	if r.exists("paisans-standby") {
		t.Error("the state file is left behind after a stop")
	}
	if r.exists("shared-standby") {
		t.Error("the shared marker is left behind after a stop")
	}
}

// An instance that was admitted but never becomes ready loses the state file
// after PAISANS_STANDBY_HOLD seconds, so a broken takeover reads as down
// instead of hiding behind the standby's healthy healthcheck.
func TestStandbyWrapperDropsTheStateFileForAnAdmittedInstanceThatIsNotReady(t *testing.T) {
	r := startWrapper(t, "CHILD_REFUSALS=1", "CHILD_NEVER_READY=1", "PAISANS_STANDBY_HOLD=1")
	r.waitFor("a second start", 20*time.Second, func() bool {
		seen, _ := os.ReadFile(filepath.Join(r.dir, "seen"))
		return strings.Contains(string(seen), "2 present")
	})
	r.waitFor("the state file gone after the hold", 5*time.Second, func() bool { return !r.exists("paisans-standby") })
	if r.exists("shared-standby") {
		t.Error("the shared marker outlives the state file after the hold")
	}
	r.stop()
}
