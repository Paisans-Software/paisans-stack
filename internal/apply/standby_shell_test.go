package apply_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// fakeDocker stands in for docker on a host where the command runs as the
// deploy user without sudo. `docker compose` fails before it reaches the
// daemon, as Compose v2 does when a service's env_file is the root owned 0600
// .env apply writes: it loads every env_file to build the project, even for
// exec. `docker ps` and `docker exec` work, as they do for a member of the
// docker group. FAKE_RUNNING and FAKE_STANDBY say what the container is doing.
const fakeDocker = `#!/bin/sh
case "$1" in
compose)
	echo "open /srv/paisans/f2a9/auth/.env: permission denied" >&2
	exit 1 ;;
ps)
	shift
	want="-q --filter label=community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01 --filter label=com.docker.compose.project=paisans-f2a9-auth --filter label=com.docker.compose.service=app"
	if [ "$*" != "$want" ]; then
		echo "unexpected docker ps $*" >&2
		exit 2
	fi
	[ -n "${FAKE_RUNNING:-}" ] && echo 0123456789ab
	exit 0 ;;
exec)
	[ "$2" = 0123456789ab ] && [ "$3 $4 $5" = "test -f /tmp/paisans-standby" ] && [ -n "${FAKE_STANDBY:-}" ] && exit 0
	exit 1 ;;
esac
exit 2
`

// fakeCurl answers /healthz only when FAKE_ACTIVE is set; a standby's port
// is closed, which curl reports as exit 7.
const fakeCurl = `#!/bin/sh
[ -n "${FAKE_ACTIVE:-}" ] && exit 0
exit 7
`

// runInstanceCommand runs the real instance question through sh against the
// fakes, with the stack directory moved under a temporary root.
func runInstanceCommand(t *testing.T, env ...string) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	stack := filepath.Join(root, "srv", "paisans", "f2a9", "auth")
	for _, dir := range []string{bin, stack} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"docker": fakeDocker, "curl": fakeCurl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stack, "compose.yaml"), []byte("name: paisans-f2a9-auth\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := apply.InstanceCommand(deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}, "auth", "10.44.0.2", 1411)
	command = strings.ReplaceAll(command, "/srv/paisans/f2a9/", root+"/srv/paisans/f2a9/")
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// The case observed on a real deployment: a standby waiting for its next
// retry, asked without sudo by a deploy user in the docker group, read as
// down because the question went through `docker compose`, which could not
// read the stack's .env.
func TestAWaitingStandbyAskedWithoutRootIsStandby(t *testing.T) {
	if got := runInstanceCommand(t, "FAKE_RUNNING=1", "FAKE_STANDBY=1"); got != "standby" {
		t.Fatalf("a waiting standby answered %q, want standby", got)
	}
}

func TestARunningContainerWithNeitherStateFileNorHealthzIsDown(t *testing.T) {
	if got := runInstanceCommand(t, "FAKE_RUNNING=1"); got != "down" {
		t.Fatalf("answered %q, want down", got)
	}
}

func TestAStoppedContainerIsDown(t *testing.T) {
	if got := runInstanceCommand(t, "FAKE_STANDBY=1"); got != "down" {
		t.Fatalf("answered %q, want down", got)
	}
}

func TestAnAnsweringHealthzIsActive(t *testing.T) {
	if got := runInstanceCommand(t, "FAKE_RUNNING=1", "FAKE_ACTIVE=1"); got != "active" {
		t.Fatalf("answered %q, want active", got)
	}
}
