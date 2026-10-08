package apply_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// volumeHost answers the prune's two commands: the listing, and removals.
type volumeHost struct {
	listing  string
	commands []string
	// inUse names volumes `docker volume rm` refuses, as Docker does for one
	// a container mounts.
	inUse map[string]bool
}

func (h *volumeHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	if strings.Contains(command, "vs=$(docker volume ls -qf dangling=true)") {
		return h.listing, nil
	}
	if name, ok := strings.CutPrefix(command, "docker volume rm "); ok {
		name = strings.Trim(name, "'")
		if h.inUse[name] {
			return "Error response from daemon: remove " + name + ": volume is in use", fmt.Errorf("exit status 1")
		}
		return name + "\n", nil
	}
	return "", nil
}
func (h *volumeHost) RunInput(c, _ string) (string, error)  { return h.Run(c) }
func (h *volumeHost) ReadFile(string) (string, bool, error) { return "", false, nil }
func (h *volumeHost) WriteFile(string, string, uint32) error {
	return nil
}
func (h *volumeHost) Describe() string { return "fake" }

const (
	leaked1 = "3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191"
	leaked2 = "4e69f2bcb465d8db8ce062bb7928930be85a8324d7829d34be34fc3b5b4bf68f"
	oldName = "8cce176c65a4f3a4a255ca46dc4588b38d917bffc8f0c13fd4d5335d4fc8f830"
)

func listing(lines ...string) string { return strings.Join(lines, "\n") + "\nend\n" }

// Each class of dangling volume gets its verdict: anonymous ones (by Docker's
// label, or by a bare 64 hex name from an engine that sets none) and ones a
// paisans compose project made are removed; another project's and a named
// one are kept.
func TestPruneDecidesEachDanglingVolume(t *testing.T) {
	host := &volumeHost{listing: listing(
		"volume\t"+leaked1+"\t48234496\t{\"com.docker.volume.anonymous\":\"\"}\tcache log",
		"volume\t"+oldName+"\t1024\tnull\t",
		"volume\tpaisans-talk_data\t4096\t{\"com.docker.compose.project\":\"paisans-talk\",\"com.docker.compose.volume\":\"data\"}\tx",
		"volume\tother_db\t9999\t{\"com.docker.compose.project\":\"other\",\"com.docker.compose.volume\":\"db\"}\tpgdata",
		"volume\tbackups\t1\t{}\tdump.sql",
	)}
	p, err := apply.BuildVolumePrune("home-a", host, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{leaked1: true, oldName: true, "paisans-talk_data": true, "other_db": false, "backups": false}
	if len(p.Volumes) != len(want) {
		t.Fatalf("read %d volumes, want %d: %+v", len(p.Volumes), len(want), p.Volumes)
	}
	for _, v := range p.Volumes {
		if v.Remove != want[v.Name] {
			t.Errorf("%s: remove is %v (%s), want %v", v.Name, v.Remove, v.Reason, want[v.Name])
		}
	}
	first := p.Volumes[0]
	if first.Size != 48234496 || strings.Join(first.Entries, " ") != "cache log" {
		t.Errorf("size and entries were not read: %+v", first)
	}
	if !strings.Contains(p.Volumes[3].Reason, "compose project other") {
		t.Errorf("a kept volume does not say whose it is: %s", p.Volumes[3].Reason)
	}
	if len(host.commands) != 1 {
		t.Errorf("the dry run ran more than the listing: %v", host.commands)
	}

	if err := apply.ExecuteVolumePrune(p, host); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range host.commands {
		if name, ok := strings.CutPrefix(c, "docker volume rm "); ok {
			removed = append(removed, strings.Trim(name, "'"))
		}
	}
	if strings.Join(removed, " ") != leaked1+" "+oldName+" paisans-talk_data" {
		t.Errorf("removed %v", removed)
	}
}

// A volume something started using since the listing is refused by Docker,
// and the rest are still removed; the failure is reported at the end.
func TestPruneReportsAVolumeDockerWouldNotRemove(t *testing.T) {
	host := &volumeHost{
		listing: listing("volume\t"+leaked1+"\t1\tnull\t", "volume\t"+leaked2+"\t1\tnull\t"),
		inUse:   map[string]bool{leaked1: true},
	}
	p, err := apply.BuildVolumePrune("home-a", host, false)
	if err != nil {
		t.Fatal(err)
	}
	err = apply.ExecuteVolumePrune(p, host)
	if err == nil || !strings.Contains(err.Error(), leaked1) || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("an in use volume was not reported: %v", err)
	}
	if !strings.Contains(strings.Join(host.commands, "\n"), "docker volume rm '"+leaked2+"'") {
		t.Error("one refusal stopped the remaining removals")
	}
}

// An answer without its end marker was cut short, and is not read as a
// shorter list.
func TestPruneRefusesACutShortListing(t *testing.T) {
	host := &volumeHost{listing: "volume\t" + leaked1 + "\t1\tnull\t\n"}
	if _, err := apply.BuildVolumePrune("home-a", host, false); err == nil {
		t.Error("a listing with no end marker was accepted")
	}
}

// On a shared host only a volume a paisans-* compose project labelled is
// removed. An anonymous one may be another project's leftover as easily as
// a paisans container's, so it is listed and kept.
func TestASharedHostPrunesOnlyLabelledVolumes(t *testing.T) {
	host := &volumeHost{listing: listing(
		"volume\t"+leaked1+"\t48234496\t{\"com.docker.volume.anonymous\":\"\"}\tcache log",
		"volume\t"+oldName+"\t1024\tnull\t",
		"volume\tpaisans-talk_data\t4096\t{\"com.docker.compose.project\":\"paisans-talk\",\"com.docker.compose.volume\":\"data\"}\tx",
		"volume\tother_db\t9999\t{\"com.docker.compose.project\":\"other\"}\tpgdata",
	)}
	p, err := apply.BuildVolumePrune("home-a", host, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Shared {
		t.Error("the plan does not say the host is shared")
	}
	if len(p.Volumes) != 4 {
		t.Fatalf("listed %d volumes, want every dangling one: %+v", len(p.Volumes), p.Volumes)
	}
	for _, v := range p.Volumes {
		if v.Remove != (v.Name == "paisans-talk_data") {
			t.Errorf("%s: remove %v (%s)", v.Name, v.Remove, v.Reason)
		}
		if v.Name == leaked1 && !strings.Contains(v.Reason, "shared") {
			t.Errorf("an anonymous volume kept without saying why: %s", v.Reason)
		}
	}
	if err := apply.ExecuteVolumePrune(p, host); err != nil {
		t.Fatal(err)
	}
	for _, c := range host.commands {
		if strings.HasPrefix(c, "docker volume rm ") && c != "docker volume rm 'paisans-talk_data'" {
			t.Errorf("a shared host's prune ran %s", c)
		}
	}
}
