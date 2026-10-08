package deployment_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

const fixtureID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"

func TestNewIDIsAValidVersion4ID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := deployment.NewID()
		if err != nil {
			t.Fatal(err)
		}
		if !deployment.ValidID(id) {
			t.Fatalf("NewID returned %q, which ValidID refuses", id)
		}
		if seen[id] {
			t.Fatalf("NewID returned %q twice", id)
		}
		seen[id] = true
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		fixtureID:                              true,
		"F2A9C4E1-0B7D-4C3A-9E2F-5A6B7C8D9E01": false,
		"f2a9c4e1-0b7d-1c3a-9e2f-5a6b7c8d9e01": false,
		"f2a9c4e1-0b7d-4c3a-7e2f-5a6b7c8d9e01": false,
		"f2a9c4e10b7d4c3a9e2f5a6b7c8d9e01":     false,
		"":                                     false,
	} {
		if got := deployment.ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestNamesAndPathsDeriveFromTheToken(t *testing.T) {
	d := deployment.Deployment{ID: fixtureID}
	for got, want := range map[string]string{
		d.Token():                 "f2a9",
		d.Prefix():                "paisans-f2a9",
		d.Project("talk"):         "paisans-f2a9-talk",
		d.Root():                  "/srv/paisans/f2a9",
		d.Rel():                   "srv/paisans/f2a9",
		d.Compose("infra"):        "/srv/paisans/f2a9/infra/compose.yaml",
		d.ComposeCmd("talk"):      "docker compose -f /srv/paisans/f2a9/talk/compose.yaml",
		d.Path("infra", "x"):      "/srv/paisans/f2a9/infra/x",
		d.RelPath("talk", ".env"): "srv/paisans/f2a9/talk/.env",
		d.LabelFilter():           "label=community.paisans.deployment=" + fixtureID,
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestSplitRel(t *testing.T) {
	token, stack, rest, ok := deployment.SplitRel("srv/paisans/f2a9/talk/config/x.yaml")
	if !ok || token != "f2a9" || stack != "talk" || rest != "config/x.yaml" {
		t.Errorf("SplitRel = %q %q %q %v", token, stack, rest, ok)
	}
	for _, rel := range []string{"etc/wireguard/wg0.conf", "srv/talk/.env", "srv/paisans/f2a9/talk", "srv/paisans/f2a9/.paisans-manifest.json"} {
		if _, _, _, ok := deployment.SplitRel(rel); ok {
			t.Errorf("SplitRel(%q) reported a stack", rel)
		}
	}
}
