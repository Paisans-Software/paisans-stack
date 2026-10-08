package appremove

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/render"
)

const (
	ours   = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"
	theirs = "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
)

func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// host is one fake machine: its files by absolute path, its Docker objects,
// and every command it was sent. Directories exist exactly when a file is
// under them, which is how `rmdir` of an empty one behaves.
type host struct {
	name       string
	files      map[string]string
	containers []hostcheck.Container
	networks   []hostcheck.Network
	volumes    []hostcheck.Volume
	sent       []string
	// db, garage and s3 are the parts of a data site.
	db     *database
	garage *garageNode
}

type database struct {
	leader string
	dbs    map[string]string // name -> owner
	roles  map[string]bool
}

type garageNode struct {
	keys    map[string]bool
	buckets map[string]*bucket // by alias
}

type bucket struct {
	id      string
	keys    []string
	objects []string
}

func newHost(name string) *host { return &host{name: name, files: map[string]string{}} }

func (h *host) Describe() string { return h.name }

func (h *host) ReadFile(path string) (string, bool, error) {
	c, ok := h.files[path]
	return c, ok, nil
}

func (h *host) WriteFile(path, content string, mode uint32) error {
	h.files[path] = content
	return nil
}

var (
	projectFilter = regexp.MustCompile(`label=com\.docker\.compose\.project='([^']+)'`)
	labelFilter   = regexp.MustCompile(`'label=community\.paisans\.deployment=([^']+)'`)
	fileLine      = regexp.MustCompile(`^f='([^']+)'; .* = '([0-9a-f]+)' \]`)
)

func (h *host) Run(command string) (string, error) {
	h.sent = append(h.sent, command)
	switch {
	case strings.HasPrefix(command, "d='") && strings.Contains(command, "du -sb"):
		dir := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(command, ";", 2)[0], "d='"), "'")
		return h.dirAnswer(dir), nil
	case strings.Contains(command, "docker ps -aq"):
		id, project := labelFilter.FindStringSubmatch(command)[1], projectFilter.FindStringSubmatch(command)[1]
		var keep []hostcheck.Container
		for _, c := range h.containers {
			if c.Deployment != id || c.Project != project {
				keep = append(keep, c)
			}
		}
		h.containers = keep
		var nets []hostcheck.Network
		for _, n := range h.networks {
			if n.Deployment != id || n.Project != project {
				nets = append(nets, n)
			}
		}
		h.networks = nets
		return "", nil
	case strings.Contains(command, "docker volume ls"):
		id, project := labelFilter.FindStringSubmatch(command)[1], projectFilter.FindStringSubmatch(command)[1]
		var keep []hostcheck.Volume
		for _, v := range h.volumes {
			if v.Deployment != id || v.Project != project {
				keep = append(keep, v)
			}
		}
		h.volumes = keep
		return "", nil
	case strings.HasPrefix(command, "f='"):
		var out strings.Builder
		for _, line := range strings.Split(strings.TrimSpace(command), "\n") {
			m := fileLine.FindStringSubmatch(line)
			if m == nil {
				return "", fmt.Errorf("unreadable files line %q", line)
			}
			content, ok := h.files[m[1]]
			switch {
			case !ok:
				fmt.Fprintf(&out, "gone %s\n", m[1])
			case sum(content) == m[2]:
				delete(h.files, m[1])
				fmt.Fprintf(&out, "removed %s\n", m[1])
			default:
				fmt.Fprintf(&out, "kept %s\n", m[1])
			}
		}
		return out.String(), nil
	case strings.HasPrefix(command, "rm -rf -- '"):
		dir := strings.TrimSuffix(strings.TrimPrefix(command, "rm -rf -- '"), "'")
		for p := range h.files {
			if strings.HasPrefix(p, dir+"/") {
				delete(h.files, p)
			}
		}
		return "", nil
	case strings.HasPrefix(command, "d='") && strings.Contains(command, "-empty -delete"):
		return "", nil
	case strings.Contains(command, "exec -T patroni curl"):
		return fmt.Sprintf(`{"members":[{"name":%q,"role":"leader","state":"running"}]}`, h.db.leader), nil
	case strings.Contains(command, "exec -T garage /garage"):
		return h.garageRun(command[strings.Index(command, "/garage ")+len("/garage "):])
	}
	return "", fmt.Errorf("%s: unexpected command %q", h.name, command)
}

func (h *host) RunInput(command, stdin string) (string, error) {
	h.sent = append(h.sent, command)
	switch {
	case strings.Contains(command, "psql"):
		return h.psql(stdin)
	case strings.HasPrefix(command, "curl "):
		return h.s3(stdin)
	}
	return "", fmt.Errorf("%s: unexpected command %q", h.name, command)
}

func (h *host) dirAnswer(dir string) string {
	var bytes, n int
	top := map[string]bool{}
	for p, c := range h.files {
		if rest, ok := strings.CutPrefix(p, dir+"/"); ok {
			bytes += len(c)
			n++
			top[strings.SplitN(rest, "/", 2)[0]] = true
		}
	}
	if n == 0 {
		return ""
	}
	entries := make([]string, 0, len(top))
	for e := range top {
		entries = append(entries, e)
	}
	sort.Strings(entries)
	return fmt.Sprintf("present\n%d\n%d\n%s\n", bytes, n, strings.Join(entries, "\n"))
}

var dbName = regexp.MustCompile(`datname = '([^']+)'`)

func (h *host) psql(sql string) (string, error) {
	if m := dbName.FindStringSubmatch(sql); m != nil {
		var out strings.Builder
		if owner, ok := h.db.dbs[m[1]]; ok {
			fmt.Fprintf(&out, "database|%s|%s\n", m[1], owner)
		}
		if h.db.roles[m[1]] {
			fmt.Fprintf(&out, "role|%s|\n", m[1])
		}
		return out.String(), nil
	}
	for _, line := range strings.Split(strings.TrimSpace(sql), "\n") {
		switch {
		case strings.HasPrefix(line, "DROP DATABASE IF EXISTS "):
			delete(h.db.dbs, strings.Trim(strings.Fields(line)[4], `"`))
		case strings.HasPrefix(line, "DROP ROLE IF EXISTS "):
			delete(h.db.roles, strings.Trim(strings.TrimSuffix(strings.Fields(line)[4], ";"), `"`))
		default:
			return "", fmt.Errorf("unexpected SQL %q", line)
		}
	}
	return "", nil
}

func (h *host) garageRun(args string) (string, error) {
	f := strings.Fields(args)
	g := h.garage
	switch {
	case f[0] == "key" && f[1] == "info":
		if !g.keys[f[2]] {
			return "0 matching keys", fmt.Errorf("exit status 1")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Key name: x\nKey ID: %s\nSecret key: SECRET-NEVER-SHOWN\nCan create buckets: false\n\nKey-specific bucket aliases:\n\nAuthorized buckets:\n", f[2])
		for alias, bk := range g.buckets {
			for _, k := range bk.keys {
				if k == f[2] {
					fmt.Fprintf(&b, "  RWO  %s    %s\n", alias, bk.id[:16])
				}
			}
		}
		return b.String(), nil
	case f[0] == "bucket" && f[1] == "info":
		for alias, bk := range g.buckets {
			if alias == f[2] || strings.HasPrefix(bk.id, f[2]) {
				var b strings.Builder
				fmt.Fprintf(&b, "Bucket: %s\n\nSize: 0 B (0 B)\nObjects: %d\n\nWebsite access: false\n\nGlobal aliases:\n  %s\n\nKey-specific aliases:\n\nAuthorized keys:\n", bk.id, len(bk.objects), alias)
				for _, k := range bk.keys {
					fmt.Fprintf(&b, "  RWO  %s  name\n", k)
				}
				return b.String(), nil
			}
		}
		return "Bucket not found / several matching buckets: " + f[2], fmt.Errorf("exit status 1")
	case f[0] == "bucket" && f[1] == "delete":
		bk := g.buckets[f[3]]
		if len(bk.objects) > 0 {
			return "Bucket is not empty", fmt.Errorf("exit status 1")
		}
		delete(g.buckets, f[3])
		return "", nil
	case f[0] == "key" && f[1] == "delete":
		delete(g.keys, f[3])
		for _, bk := range g.buckets {
			var keep []string
			for _, k := range bk.keys {
				if k != f[3] {
					keep = append(keep, k)
				}
			}
			bk.keys = keep
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected garage %q", args)
}

var curlURL = regexp.MustCompile(`url = "http://[^/]+/([^/?"]+)([^"]*)"`)

// s3 lists at most two objects a page, so emptying takes several.
func (h *host) s3(config string) (string, error) {
	if strings.Contains(config, "SECRET") == false {
		return "", fmt.Errorf("a request without the app's secret")
	}
	if strings.Contains(config, `request = "GET"`) {
		bk := h.garage.buckets[curlURL.FindStringSubmatch(config)[1]]
		page := bk.objects
		if len(page) > 2 {
			page = page[:2]
		}
		var b strings.Builder
		b.WriteString("<ListBucketResult>")
		for _, k := range page {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key></Contents>", k)
		}
		fmt.Fprintf(&b, "<IsTruncated>%v</IsTruncated></ListBucketResult>", len(bk.objects) > 2)
		return b.String(), nil
	}
	for _, m := range curlURL.FindAllStringSubmatch(config, -1) {
		bk := h.garage.buckets[m[1]]
		key := strings.TrimPrefix(m[2], "/")
		var keep []string
		for _, o := range bk.objects {
			if o != key {
				keep = append(keep, o)
			}
		}
		bk.objects = keep
	}
	return "", nil
}

// manifest writes the host's manifest from entries, each with the hash of
// the file the host holds now unless given.
func (h *host) manifest(t *testing.T, entries ...render.ManifestFile) {
	t.Helper()
	data, err := json.MarshalIndent(render.Manifest{Version: 1, Files: entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	h.files["/srv/paisans/f2a9/.paisans-manifest.json"] = string(data) + "\n"
}

func (h *host) manifestFiles(t *testing.T) []render.ManifestFile {
	t.Helper()
	var m render.Manifest
	if err := json.Unmarshal([]byte(h.files["/srv/paisans/f2a9/.paisans-manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	return m.Files
}

// entry is a manifest entry for a file the host holds, marked left over
// when leftover is set.
func (h *host) entry(path string, leftover bool) render.ManifestFile {
	e := render.ManifestFile{Path: strings.TrimPrefix(path, "/"), SHA256: sum(h.files[path]), Mode: "0644"}
	if leftover {
		e.Leftover, e.LeftoverSince = true, "2026-10-01T00:00:00Z"
	}
	return e
}

// state is what ProbeSite reads, from the fake: the inventory as
// hostcheck.Inspect would parse it, then the same file and directory reads.
func state(t *testing.T, cfg *config.Config, app, site string, h *host) SiteState {
	t.Helper()
	inv := &hostcheck.Inventory{
		Containers:   append([]hostcheck.Container(nil), h.containers...),
		Networks:     append([]hostcheck.Network(nil), h.networks...),
		Volumes:      append([]hostcheck.Volume(nil), h.volumes...),
		ManifestPath: "/srv/paisans/f2a9/.paisans-manifest.json",
	}
	if _, ok := h.files[inv.ManifestPath]; ok {
		inv.Manifest = true
		inv.ManifestFiles = h.manifestFiles(t)
	}
	st := SiteState{Site: site, Inventory: inv, Hashes: map[string]string{}}
	for _, e := range inv.ManifestFiles {
		if !AppFile(cfg, app, e.Path) {
			continue
		}
		if c, ok, _ := h.ReadFile("/" + e.Path); ok {
			st.Hashes[e.Path] = sum(c)
		}
	}
	dir, err := probeDir(h, cfg.Deployment().Dir(app))
	if err != nil {
		t.Fatal(err)
	}
	st.Dir = dir
	return st
}
