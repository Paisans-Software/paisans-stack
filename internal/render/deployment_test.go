package render_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

const fixtureID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"

// Every compose project is paisans-<token>-<stack>, and every service and the
// project's network carry the deployment label with the full id, which is
// what doctor and prune match ownership on.
func TestComposeFilesNameTheTokenAndLabelTheID(t *testing.T) {
	plan := build(t)
	if plan.Deployment.ID != fixtureID {
		t.Fatalf("the plan's deployment is %q, want the fixture's", plan.Deployment.ID)
	}
	seen := 0
	for _, f := range plan.Files {
		if !strings.HasSuffix(f.Path, "/compose.yaml") {
			continue
		}
		seen++
		_, rel, _ := strings.Cut(f.Path, "/")
		_, stack, _, ok := deployment.SplitRel(rel)
		if !ok {
			t.Fatalf("%s is not under a deployment root", f.Path)
		}
		var doc struct {
			Name     string `yaml:"name"`
			Services map[string]struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"services"`
			Networks map[string]struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"networks"`
		}
		if err := yaml.Unmarshal([]byte(f.Content), &doc); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		if want := "paisans-f2a9-" + stack; doc.Name != want {
			t.Errorf("%s: project %q, want %q", f.Path, doc.Name, want)
		}
		if len(doc.Services) == 0 {
			t.Errorf("%s has no services", f.Path)
		}
		for name, svc := range doc.Services {
			if svc.Labels[deployment.Label] != fixtureID {
				t.Errorf("%s: service %s carries %s=%q, want the deployment id", f.Path, name, deployment.Label, svc.Labels[deployment.Label])
			}
		}
		if doc.Networks["default"].Labels[deployment.Label] != fixtureID {
			t.Errorf("%s: the default network is not labelled with the deployment id", f.Path)
		}
	}
	if seen == 0 {
		t.Fatal("no compose file was rendered")
	}
}

// Everything apply renders lands under /srv/paisans/<token>/ or /etc, and
// every bind mount a compose file names on the host is under the root too.
func TestEveryPathIsUnderTheDeploymentRoot(t *testing.T) {
	for _, f := range build(t).Files {
		site, rel, _ := strings.Cut(f.Path, "/")
		if rel == "manifest.json" || strings.HasPrefix(rel, "etc/") {
			continue
		}
		if !strings.HasPrefix(rel, "srv/paisans/f2a9/") {
			t.Errorf("%s/%s is outside the deployment root", site, rel)
		}
		if !strings.HasSuffix(rel, "compose.yaml") {
			continue
		}
		for _, line := range strings.Split(f.Content, "\n") {
			// A bind is host:container with both absolute; a tmpfs entry's
			// second part is its options.
			mount, ok := strings.CutPrefix(strings.TrimSpace(line), "- /")
			_, target, bind := strings.Cut(mount, ":")
			if !ok || !bind || !strings.HasPrefix(target, "/") || strings.HasPrefix(mount, "dev/") {
				continue
			}
			if !strings.HasPrefix(mount, "srv/paisans/f2a9/") {
				t.Errorf("%s: bind mount /%s is outside the deployment root", f.Path, mount)
			}
		}
	}
}
