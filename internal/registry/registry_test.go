package registry

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
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

// meshEntry is entry with a mesh: interface, listen port and subnet.
func meshEntry(token, domain, site string, port int, subnet string) Entry {
	e := entry(token, domain, site)
	e.Interface = "psns-" + token
	e.ListenPort = port
	e.Subnet = subnet
	return e
}

// The mesh fields refuse a claim the same way a token does, in Merge and in
// the awk merge alike, and two deployments with nothing in common coexist.
func TestMeshConflicts(t *testing.T) {
	base := Registry{Version: Version, Deployments: map[string]Entry{
		other: meshEntry("0c1d", "example.net", "vm", 51820, "10.44.0.0/24"),
	}}
	input, err := Encode(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		e     Entry
		clash string
	}{
		{"same interface", func() Entry {
			e := meshEntry("f2a9", "example.org", "vm", 51821, "10.45.0.0/24")
			e.Interface = "psns-0c1d"
			return e
		}(), "WireGuard interface psns-0c1d"},
		{"same listen port", meshEntry("f2a9", "example.org", "vm", 51820, "10.45.0.0/24"), "WireGuard listen port 51820/udp"},
		{"same subnet", meshEntry("f2a9", "example.org", "vm", 51821, "10.44.0.0/24"), "mesh subnet 10.44.0.0/24"},
		{"a subnet containing theirs", meshEntry("f2a9", "example.org", "vm", 51821, "10.0.0.0/8"), "mesh subnet 10.44.0.0/24, which overlaps this deployment's 10.0.0.0/8"},
		{"a subnet inside theirs", meshEntry("f2a9", "example.org", "vm", 0, "10.44.0.128/25"), "mesh subnet 10.44.0.0/24, which overlaps this deployment's 10.44.0.128/25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Merge(base, ours, tc.e)
			var c Conflict
			if !errors.As(err, &c) || c.ID != other {
				t.Fatalf("Merge = %v, want a conflict with %s", err, other)
			}
			if !strings.Contains(err.Error(), tc.clash) || !strings.Contains(err.Error(), "example.net") {
				t.Errorf("the refusal %q does not name %q and the other deployment", err, tc.clash)
			}
			stdout, stderr, code := runMerge(t, string(input), ours, tc.e)
			if code != 3 || stdout != "" {
				t.Fatalf("the awk merge did not refuse: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if err := ParseClaim(stderr); !errors.As(err, &c) || c.ID != other {
				t.Errorf("the awk refusal parsed to %v", err)
			}
			if got := clashes(c.Entry, tc.e); len(got) == 0 || got[0] != tc.clash {
				t.Errorf("clashes = %q, want %q first", got, tc.clash)
			}
		})
	}

	t.Run("disjoint meshes coexist", func(t *testing.T) {
		e := meshEntry("f2a9", "example.org", "vm", 51821, "10.45.0.0/24")
		got, err := Merge(base, ours, e)
		if err != nil || len(got.Deployments) != 2 {
			t.Fatalf("Merge = %+v, %v", got, err)
		}
		stdout, stderr, code := runMerge(t, string(input), ours, e)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		back, err := Parse([]byte(stdout))
		if err != nil || back.Deployments[ours] != e || back.Deployments[other] != base.Deployments[other] {
			t.Errorf("the awk merge wrote %+v, %v", back, err)
		}
	})
	t.Run("no listen port on either side is no clash", func(t *testing.T) {
		start := Registry{Version: Version, Deployments: map[string]Entry{other: meshEntry("0c1d", "example.net", "home-a", 0, "10.44.0.0/24")}}
		in, _ := Encode(start)
		e := meshEntry("f2a9", "example.org", "home-a", 0, "10.45.0.0/24")
		if _, err := Merge(start, ours, e); err != nil {
			t.Fatal(err)
		}
		if _, stderr, code := runMerge(t, string(in), ours, e); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	})
}

func TestMeshesListsOtherDeploymentsSubnets(t *testing.T) {
	r := Registry{Version: Version, Deployments: map[string]Entry{
		ours:   meshEntry("f2a9", "example.org", "home-a", 0, "10.44.0.0/24"),
		other:  meshEntry("0c1d", "example.net", "home-a", 0, "10.45.0.0/24"),
		theirs: entry("f2a9", "example.com", "home-a"),
	}}
	got := Meshes(r, ours, "home-a")
	if len(got) != 1 || got[0].Prefix.String() != "10.45.0.0/24" || !strings.Contains(got[0].What, other) || !strings.Contains(got[0].What, "on home-a") {
		t.Errorf("Meshes = %+v", got)
	}
}

// roleEntry is meshEntry on disjoint meshes, with roles.
func roleEntry(token, domain string, port int, subnet, roles string) Entry {
	e := meshEntry(token, domain, "vm", port, subnet)
	e.Roles = roles
	return e
}

// The gateway and data roles are one deployment's per host, in Merge and in
// the awk merge alike; every other role, and an entry without roles, shares.
func TestRoleConflicts(t *testing.T) {
	theirsEntry := roleEntry("0c1d", "example.net", 51820, "10.44.0.0/24", "apps,data,gateway")
	base := Registry{Version: Version, Deployments: map[string]Entry{other: theirsEntry}}
	input, err := Encode(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, roles, clash, advice string
	}{
		{"a second gateway", "gateway", "the gateway role", "Use another host for one of the gateways"},
		{"a second data site", "apps,data", "the data role", "Use another host for one of the data sites"},
		{"both", "data,gateway", "the data role", "Use another host for one of the data sites"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := roleEntry("f2a9", "example.org", 51821, "10.45.0.0/24", tc.roles)
			_, err := Merge(base, ours, e)
			var c Conflict
			if !errors.As(err, &c) || c.ID != other || len(c.Clashes) == 0 || c.Clashes[0] != tc.clash {
				t.Fatalf("Merge = %v, want a conflict with %s on %q", err, other, tc.clash)
			}
			stdout, stderr, code := runMerge(t, string(input), ours, e)
			if code != 3 || stdout != "" {
				t.Fatalf("the awk merge did not refuse: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if err := ParseClaim(stderr); !errors.As(err, &c) || c.ID != other {
				t.Fatalf("the awk refusal parsed to %v", err)
			}
			c.Clashes = clashes(c.Entry, e)
			msg := refused(fakeHost{}, e, c).Error()
			if !strings.Contains(msg, tc.clash) || !strings.Contains(msg, tc.advice) {
				t.Errorf("the refusal %q does not name %q and say %q", msg, tc.clash, tc.advice)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		theirs string
		ours   string
	}{
		{"apps beside a gateway and data", "apps,data,gateway", "apps,witness"},
		{"an entry without roles holds none", "", "data,gateway"},
		{"a claim without roles claims none", "apps,data,gateway", ""},
		{"a role that only contains the word", "databases", "data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := Registry{Version: Version, Deployments: map[string]Entry{
				other: roleEntry("0c1d", "example.net", 51820, "10.44.0.0/24", tc.theirs),
			}}
			in, _ := Encode(start)
			e := roleEntry("f2a9", "example.org", 51821, "10.45.0.0/24", tc.ours)
			got, err := Merge(start, ours, e)
			if err != nil {
				t.Fatalf("Merge = %v", err)
			}
			stdout, stderr, code := runMerge(t, string(in), ours, e)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			back, err := Parse([]byte(stdout))
			if err != nil || back.Deployments[ours] != got.Deployments[ours] || back.Deployments[other] != got.Deployments[other] {
				t.Errorf("the awk merge wrote %+v, %v, want %+v", back, err, got)
			}
		})
	}
}

func TestJoinRolesSorts(t *testing.T) {
	if got := JoinRoles([]config.Role{config.RoleGateway, config.RoleApps, config.RoleData}); got != "apps,data,gateway" {
		t.Errorf("JoinRoles = %q", got)
	}
	if got := JoinRoles(nil); got != "" {
		t.Errorf("JoinRoles(nil) = %q", got)
	}
}

type fakeHost struct{}

func (fakeHost) Run(string) (string, error)            { return "", nil }
func (fakeHost) ReadFile(string) (string, bool, error) { return "", false, nil }
func (fakeHost) Describe() string                      { return "vm" }
