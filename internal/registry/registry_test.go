package registry

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ours   = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"
	theirs = "f2a91111-2222-4333-8444-555566667777"
	other  = "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
)

func entry(token, domain, site string) Entry {
	return Entry{Token: token, Root: "/srv/paisans/" + token, Domain: domain, Site: site, ClaimedAt: "2026-10-01T00:00:00Z"}
}

func TestParseEmptyIsAnEmptyRegistry(t *testing.T) {
	r, err := Parse(nil)
	if err != nil || len(r.Deployments) != 0 || r.Version != Version {
		t.Fatalf("Parse(nil) = %+v, %v", r, err)
	}
	if _, err := Parse([]byte(`{"version":2,"deployments":{}}`)); err == nil {
		t.Error("a registry of another version was read")
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Error("garbage was read as a registry")
	}
}

func TestMerge(t *testing.T) {
	base := Registry{Version: Version, Deployments: map[string]Entry{ours: entry("f2a9", "example.org", "home-a")}}

	t.Run("same token, another id, is refused", func(t *testing.T) {
		got, err := Merge(base, theirs, entry("f2a9", "example.net", "vm"))
		var c Conflict
		if !errors.As(err, &c) || c.ID != ours || c.Entry.Domain != "example.org" {
			t.Fatalf("Merge = %v, want a conflict naming %s and example.org", err, ours)
		}
		if !strings.Contains(err.Error(), ours) || !strings.Contains(err.Error(), "example.org") {
			t.Errorf("the refusal does not name the other deployment: %v", err)
		}
		if len(got.Deployments) != 1 {
			t.Errorf("a refused merge changed the registry: %+v", got)
		}
	})
	t.Run("same root, another token, is refused", func(t *testing.T) {
		e := entry("beef", "example.net", "vm")
		e.Root = "/srv/paisans/f2a9"
		if _, err := Merge(base, other, e); err == nil {
			t.Fatal("a second deployment was given a root already held")
		}
	})
	t.Run("same id is refreshed", func(t *testing.T) {
		got, err := Merge(base, ours, entry("f2a9", "example.org", "home-b"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Deployments) != 1 || got.Deployments[ours].Site != "home-b" {
			t.Errorf("refresh = %+v", got)
		}
	})
	t.Run("another token coexists", func(t *testing.T) {
		got, err := Merge(base, other, entry("0c1d", "example.net", "vm"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Deployments) != 2 {
			t.Errorf("merge = %+v", got)
		}
	})
}

func TestEncodeRoundTrips(t *testing.T) {
	r := Registry{Version: Version, Deployments: map[string]Entry{
		ours:  entry("f2a9", "example.org", "home-a"),
		other: entry("0c1d", "example.net", "vm"),
	}}
	data, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 4 || lines[0] != header || lines[3] != footer || !strings.HasPrefix(lines[1], `"`+other+`":`) {
		t.Fatalf("layout is not one deployment per line, sorted:\n%s", data)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Deployments) != 2 || back.Deployments[ours] != r.Deployments[ours] {
		t.Errorf("round trip = %+v", back)
	}
}

func TestClaimCommandShape(t *testing.T) {
	command, err := ClaimCommand(ours, entry("f2a9", "example.org", "home-a"))
	if err != nil {
		t.Fatal(err)
	}
	// Each step's position in the one command: the lock is taken before the
	// registry is read, the merge writes a temporary file, and only a merge
	// that succeeded is moved over the registry.
	at := func(step string) int {
		i := strings.Index(command, step)
		if i < 0 {
			t.Fatalf("the claim has no step %q:\n%s", step, command)
		}
		return i
	}
	order := []int{
		at("set -e; "),
		at("exec 9>>" + LockPath + "; "),
		at("; flock -w "),
		at("; tmp=$(mktemp " + Dir + "/"),
		at("; awk "),
		at(`"$src" > "$tmp"; `),
		at(`; chmod 600 "$tmp"; `),
		at(`; mv "$tmp" ` + Path),
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("the claim's steps are out of order (lock, temp file, merge, mv):\n%s", command)
		}
	}
	if !strings.HasSuffix(command, `mv "$tmp" `+Path) {
		t.Errorf("the move is not the claim's last step:\n%s", command)
	}
}

// runMerge runs the claim's awk program as ClaimCommand would, on input, and
// returns its stdout, stderr and exit code.
func runMerge(t *testing.T, input string, id string, e Entry) (string, string, int) {
	t.Helper()
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk here")
	}
	src := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(src, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := awkArgs(id, e)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("awk", append(args, src)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

// The awk merge agrees with Merge: what it writes parses to Merge's result,
// and what it refuses Merge refuses, with the refusal naming the other
// deployment.
func TestClaimMergeAgreesWithMerge(t *testing.T) {
	base := Registry{Version: Version, Deployments: map[string]Entry{
		ours:  entry("f2a9", "example.org", "home-a"),
		other: entry("0c1d", "example.net", "vm"),
	}}
	input, err := Encode(base)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		in   string
		id   string
		e    Entry
	}{
		{"a first claim on an empty host", "", ours, entry("f2a9", "example.org", "home-a")},
		{"a refresh", string(input), ours, entry("f2a9", "example.org", "home-b")},
		{"a new token", string(input), "1d2e3f4a-5b6c-4d7e-8f90-a1b2c3d4e5f6", entry("1d2e", "example.com", "home-a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runMerge(t, tc.in, tc.id, tc.e)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			got, err := Parse([]byte(stdout))
			if err != nil {
				t.Fatalf("the merge wrote an unreadable registry: %v\n%s", err, stdout)
			}
			start, _ := Parse([]byte(tc.in))
			want, err := Merge(start, tc.id, tc.e)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Deployments) != len(want.Deployments) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			for id, e := range want.Deployments {
				if got.Deployments[id] != e {
					t.Errorf("%s = %+v, want %+v", id, got.Deployments[id], e)
				}
			}
		})
	}

	stdout, stderr, code := runMerge(t, string(input), theirs, entry("f2a9", "example.com", "vm"))
	if code != 3 || stdout != "" {
		t.Fatalf("a second f2a9 was not refused: exit %d, stdout %q", code, stdout)
	}
	var c Conflict
	if err := ParseClaim(stderr); !errors.As(err, &c) || c.ID != ours || c.Entry.Domain != "example.org" {
		t.Errorf("the refusal %q parsed to %v, want a conflict naming %s", stderr, err, ours)
	}

	if _, _, code := runMerge(t, "{\"something\":\"else\"}\n", ours, entry("f2a9", "example.org", "home-a")); code != 4 {
		t.Errorf("a registry in another layout exited %d, want 4", code)
	}
}
