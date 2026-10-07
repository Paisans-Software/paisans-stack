package kinds

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImageVolumes records the VOLUME paths each image this toolkit pins declares,
// as `docker image inspect --format '{{json .Config.Volumes}}'` printed them
// for linux/amd64 on 2026-10-07, after a `docker pull` of each exact
// reference. An empty list means the image declares none.
//
// A declared volume that the compose file does not mount at exactly that path
// gets an anonymous volume from Docker, a new one for every container, and
// Compose leaves the old one behind whenever it replaces a container. A real
// apps site collected 18 that way under Mbin's /app/var/. Every path here is
// therefore mounted by the kind's template, and a render test checks it.
//
// The table is keyed by the whole reference on purpose. Bumping an image
// changes its reference, the new one has no entry, and a test fails until
// somebody inspects it and records what it declares here, which is the
// moment a new VOLUME has to be noticed. `paisans apply` makes the same check
// against the host's copy of every image, which is what covers an operator's
// own `images` override; this table covers what the toolkit ships.
//
// Inspect with --platform linux/amd64 on a machine of another architecture:
// Docker's containerd image store otherwise answers for the native platform,
// and printed null for images that do declare volumes.
var ImageVolumes = map[string][]string{
	// Applications, the defaults in catalogue.
	"ghcr.io/paisans-software/mbin:1.13.3-paisans":            {"/app/var/"},
	"docker.io/cloudamqp/amqproxy:3.2.0":                      nil,
	"docker.io/library/rabbitmq:3.13.7-management-alpine":     {"/var/lib/rabbitmq"},
	"docker.io/valkey/valkey:9.1.2-trixie":                    nil,
	"outlinewiki/outline:1.10.0":                              {"/var/lib/outline/data"},
	"ghcr.io/pocket-id/pocket-id:v2.14.0":                     nil,
	"ghcr.io/element-hq/synapse:v1.160.0":                     nil,
	"ghcr.io/element-hq/matrix-authentication-service:1.24.0": nil,
	"ghcr.io/element-hq/element-web:v1.12.27":                 nil,
	"ghcr.io/josephquigley/writefreely-wisp@sha256:4d21f45879bd98c8485eb8169ea57fbab925f0cbd5a38ac3c3bdd79901d809ea": nil,
	"quay.io/oauth2-proxy/oauth2-proxy:v7.15.4": nil,
	// A pinned app's own database, one per postgres_version the toolkit
	// knows. The path moved in 18; see PostgresDataMount.
	"postgres:16-alpine": {"/var/lib/postgresql/data"},
	"postgres:17-alpine": {"/var/lib/postgresql/data"},
	"postgres:18-alpine": {"/var/lib/postgresql"},
	// The infrastructure stack.
	"ghcr.io/zalando/spilo-16:3.3-p3":      nil,
	"ghcr.io/zalando/spilo-17:4.0-p3":      nil,
	"ghcr.io/zalando/spilo-18:4.1-p2":      nil,
	"gcr.io/etcd-development/etcd:v3.5.16": nil,
	"haproxy:3.0-alpine":                   nil,
	"dxflrs/garage:v1.0.1":                 nil,
	"ghcr.io/paisans-software/caddy:2.11.7@sha256:b401d1cb18074026a535a1facc5f6d9d35bc62fdff480f3b1fd91042c8e48186": nil,
}

// ServiceMounts is what one compose service mounts, and from which image.
type ServiceMounts struct {
	Image string
	// Targets are the container paths something persistent or deliberate is
	// mounted at: a bind, a named volume or a tmpfs. An anonymous volume the
	// file itself asks for (a bare `- /path`) is not one, since it leaks
	// exactly like the image's own.
	Targets []string
}

// ComposeMounts reads every service of a rendered compose file: its image,
// and the paths its volumes and tmpfs entries mount. Both compose syntaxes
// are read, the short `source:target[:mode]` string and the long mapping.
func ComposeMounts(content string) (map[string]ServiceMounts, error) {
	var doc struct {
		Services map[string]struct {
			Image   string      `yaml:"image"`
			Volumes []yaml.Node `yaml:"volumes"`
			Tmpfs   yaml.Node   `yaml:"tmpfs"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	out := map[string]ServiceMounts{}
	for name, service := range doc.Services {
		m := ServiceMounts{Image: service.Image}
		for _, v := range service.Volumes {
			target, ok, err := volumeTarget(v)
			if err != nil {
				return nil, fmt.Errorf("service %s: %w", name, err)
			}
			if ok {
				m.Targets = append(m.Targets, target)
			}
		}
		var tmpfs []string
		switch service.Tmpfs.Kind {
		case 0:
		case yaml.ScalarNode:
			tmpfs = []string{service.Tmpfs.Value}
		default:
			if err := service.Tmpfs.Decode(&tmpfs); err != nil {
				return nil, fmt.Errorf("service %s: tmpfs: %w", name, err)
			}
		}
		for _, entry := range tmpfs {
			target, _, _ := strings.Cut(entry, ":")
			m.Targets = append(m.Targets, target)
		}
		out[name] = m
	}
	return out, nil
}

// volumeTarget is the container path of one volumes entry, false for an
// anonymous volume.
func volumeTarget(v yaml.Node) (string, bool, error) {
	if v.Kind == yaml.ScalarNode {
		parts := strings.Split(v.Value, ":")
		if len(parts) == 1 {
			return "", false, nil
		}
		return parts[1], true, nil
	}
	var long struct {
		Type   string `yaml:"type"`
		Source string `yaml:"source"`
		Target string `yaml:"target"`
	}
	if err := v.Decode(&long); err != nil {
		return "", false, fmt.Errorf("volumes: %w", err)
	}
	if long.Type != "tmpfs" && long.Source == "" {
		return "", false, nil
	}
	return long.Target, true, nil
}

// Uncovered returns the declared volume paths no target mounts, cleaned and
// sorted.
//
// Only a mount at exactly the declared path counts. Docker skips creating an
// anonymous volume for a declared path only when the container already has a
// mount there; a bind at a parent leaves one at the declared path anyway,
// beneath it, which was seen on Docker 29.7.2 with a bind at /app and a
// declared /app/var/. A trailing slash is not a difference: Docker cleans
// the declared path, so /app/var/ is covered by a mount at /app/var.
func Uncovered(declared, targets []string) []string {
	have := map[string]bool{}
	for _, t := range targets {
		have[path.Clean(t)] = true
	}
	var out []string
	for _, d := range declared {
		if d = path.Clean(d); !have[d] {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}
