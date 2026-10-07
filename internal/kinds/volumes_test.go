package kinds_test

import (
	"reflect"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Every image a kind ships by default has its declared volumes recorded. A
// bump changes the reference, the new one has no entry, and this fails until
// somebody inspects the image and records what it declares.
func TestEveryDefaultImageHasItsVolumesRecorded(t *testing.T) {
	for _, kind := range []config.Kind{config.KindMbin, config.KindOutline, config.KindPocketID, config.KindSynapse, config.KindElement, config.KindWriteFreely, config.KindOAuth2Proxy} {
		for _, service := range kinds.Services(kind) {
			if service.Image == "" {
				continue
			}
			if _, ok := kinds.ImageVolumes[service.Image]; !ok {
				t.Errorf("%s %s runs %s, whose declared volumes are not in kinds.ImageVolumes. Pull it, run `docker image inspect --platform linux/amd64 --format '{{json .Config.Volumes}}'` on it, and record the answer", kind, service.Name, service.Image)
			}
		}
	}
}

func TestComposeMountsReadsEverySyntax(t *testing.T) {
	compose := `
services:
  app:
    image: example/app:1
    tmpfs:
      - /app/var:size=256m,mode=0755
    volumes:
      - /srv/x/log:/app/var/log
      - /srv/x/oauth:/app/config/oauth2:ro
      - /anonymous
      - type: bind
        source: /srv/x/data
        target: /data
      - type: tmpfs
        target: /scratch
      - type: volume
        target: /also-anonymous
  sidecar:
    image: example/sidecar:1
    tmpfs: /run
`
	got, err := kinds.ComposeMounts(compose)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]kinds.ServiceMounts{
		"app":     {Image: "example/app:1", Targets: []string{"/app/var/log", "/app/config/oauth2", "/data", "/scratch", "/app/var"}},
		"sidecar": {Image: "example/sidecar:1", Targets: []string{"/run"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// Only a mount at exactly the declared path covers it. A bind at a parent was
// observed to leave Docker's anonymous volume in place beneath it, and one at
// a child obviously does too.
func TestUncoveredIsExactMatchOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		declared  []string
		targets   []string
		uncovered []string
	}{
		{"exact, trailing slash ignored", []string{"/app/var/"}, []string{"/app/var"}, nil},
		{"a child does not cover", []string{"/app/var/"}, []string{"/app/var/log"}, []string{"/app/var"}},
		{"a parent does not cover", []string{"/var/lib/postgresql"}, []string{"/var/lib"}, []string{"/var/lib/postgresql"}},
		{"postgres 18 with the old target", []string{"/var/lib/postgresql"}, []string{"/var/lib/postgresql/data"}, []string{"/var/lib/postgresql"}},
		{"nothing declared", nil, []string{"/data"}, nil},
		{"several, sorted", []string{"/z", "/a", "/m"}, []string{"/m"}, []string{"/a", "/z"}},
	} {
		if got := kinds.Uncovered(tc.declared, tc.targets); !reflect.DeepEqual(got, tc.uncovered) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.uncovered)
		}
	}
}

// The official image moved its declared volume, and its PGDATA, in 18. The
// image's own tag decides when it is the official image, since an app may pin
// an older one than the cluster runs.
func TestPostgresDataMountFollowsTheImage(t *testing.T) {
	for _, tc := range []struct{ image, cluster, want string }{
		{"postgres:16-alpine", "18", "/var/lib/postgresql/data"},
		{"postgres:17-alpine", "18", "/var/lib/postgresql/data"},
		{"docker.io/library/postgres:17.6", "18", "/var/lib/postgresql/data"},
		{"postgres:18-alpine", "18", "/var/lib/postgresql"},
		{"postgres:18-alpine", "17", "/var/lib/postgresql"},
		{"postgres:latest", "17", "/var/lib/postgresql/data"},
		{"ghcr.io/example-org/postgres:16", "18", "/var/lib/postgresql"},
		{"", "17", "/var/lib/postgresql/data"},
	} {
		got := kinds.PostgresDataMount(tc.image, tc.cluster)
		if got != tc.want {
			t.Errorf("%s at cluster %s: got %s, want %s", tc.image, tc.cluster, got, tc.want)
		}
		if declared, ok := kinds.ImageVolumes[tc.image]; ok {
			if u := kinds.Uncovered(declared, []string{got}); len(u) > 0 {
				t.Errorf("%s declares %v, and %s leaves %v uncovered", tc.image, declared, got, u)
			}
		}
	}
}
