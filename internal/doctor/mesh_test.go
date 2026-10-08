package doctor

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/mesh"
)

const (
	linkUp   = `[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP","UP","LOWER_UP"],"operstate":"UNKNOWN"}]`
	ownRoute = `[{"dst":"default","dev":"eth0"},{"dst":"10.44.0.0/24","dev":"psns-f2a9"}]`
	ownAddr  = `[{"ifname":"psns-f2a9","addr_info":[{"family":"inet","local":"10.44.0.1","prefixlen":24}]}]`
)

func TestMeshPassesOnAClearHost(t *testing.T) {
	got := Mesh(fixture(t), []MeshProbe{{Site: "home-a", Link: linkUp, Probed: mesh.Probed{Routes: ownRoute, Addrs: ownAddr, Networks: "[]"}}})
	if len(got) != 2 || got[0].Level != OK || got[1].Level != OK {
		t.Fatalf("Mesh = %+v, want two OKs", got)
	}
}

func TestMeshFailsOnAnOverlapNamingBothSides(t *testing.T) {
	probed := mesh.Probed{
		Routes:   `[{"dst":"10.44.0.0/24","dev":"psns-f2a9"},{"dst":"10.44.0.0/16","dev":"psns-0c1d"}]`,
		Addrs:    ownAddr,
		Networks: `[{"Name":"lan","IPAM":{"Config":[{"Subnet":"10.44.0.128/25"}]}}]`,
	}
	got := Mesh(fixture(t), []MeshProbe{{Site: "home-a", Link: linkUp, Probed: probed}})
	var fail *Finding
	for i := range got {
		if got[i].Level == Fail {
			fail = &got[i]
		}
	}
	if fail == nil {
		t.Fatalf("no FAIL in %+v", got)
	}
	for _, want := range []string{"home-a", "10.44.0.0/24", "route 10.44.0.0/16 dev psns-0c1d", "Docker network lan (10.44.0.128/25)"} {
		if !strings.Contains(fail.Line, want) {
			t.Errorf("the finding %q does not name %q", fail.Line, want)
		}
	}
	if !strings.Contains(strings.Join(fail.More, "\n"), "fixed once deployed") || !strings.Contains(strings.Join(fail.More, "\n"), "has to move") {
		t.Errorf("the advice does not say the other network moves:\n%s", strings.Join(fail.More, "\n"))
	}
}

func TestMeshFailsOnADownInterface(t *testing.T) {
	for link, want := range map[string]string{
		"absent\n": "psns-f2a9 does not exist",
		`[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP"]}]`: "psns-f2a9 is down",
	} {
		got := Mesh(fixture(t), []MeshProbe{{Site: "home-b", Link: link, Probed: mesh.Probed{Routes: "[]", Addrs: "[]", Networks: "[]"}}})
		if len(got) == 0 || got[0].Level != Fail || !strings.Contains(got[0].Line, want) {
			t.Errorf("link %q: Mesh = %+v, want a FAIL %q", link, got, want)
		}
	}
}
