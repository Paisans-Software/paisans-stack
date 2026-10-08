package storageadd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
)

// world is a Garage cluster in maps: each site's files, its node, and the
// layout each node holds. Every gate in storage add is about what a garage
// command on some host answers, so a fake that answers from shared state is
// the same test as several machines with the failures made reachable. The
// command output imitates dxflrs/garage:v1.0.1's, as recorded in the spec.
type world struct {
	t       *testing.T
	cfg     *config.Config
	secrets *config.Secrets
	hosts   map[string]*host

	// buckets is each bucket's object count; keys the S3 keys imported;
	// grants and website what `bucket info` reports.
	buckets map[string]int
	keys    map[string]bool
	grants  map[string]bool
	website map[string]bool
	// objects is what the S3 API holds, by bucket/key.
	objects map[string]string

	// migrate is how many `layout history` reads after a layout apply still
	// report two live versions; resync how many `garage stats` reads report
	// a non empty queue.
	migrate, migrating int
	resync, resyncing  int
	// failOnce makes the first command containing it fail.
	failOnce string
	// unreachable makes every command or read containing it fail the way
	// ssh does when it cannot connect, after its retries.
	unreachable string
}

// sshDown is what SSHTransport returns when every attempt timed out.
func sshDown(host string) error {
	return fmt.Errorf("%s: %w after 3 attempts: exit status 255\nssh: connect to host 203.0.113.10 port 22: Operation timed out", host, apply.ErrUnreachable)
}

type host struct {
	w        *world
	name     string
	files    map[string]string
	commands []string
	inputs   []string

	// The node. A site that runs no Garage leaves running false and id
	// empty.
	running bool
	id      string
	// hasLayout and layoutFactor are meta/cluster_layout and the factor it
	// was built with; asides the names a reset moved it to.
	hasLayout    bool
	layoutFactor int
	asides       map[string]bool
	// version and roles are this node's copy of the layout, staged what was
	// assigned on it and not yet applied. A role is "<zone> <capacity>", the
	// capacity as layout show prints it.
	version int
	roles   map[string]string
	staged  map[string]string
	// peers are the nodes this one has met, kept across restarts the way
	// meta/peer_list is.
	peers map[string]bool
}

func (h *host) Describe() string { return h.name }

func (h *host) ReadFile(path string) (string, bool, error) {
	if h.w.unreachable != "" && strings.Contains(path, h.w.unreachable) {
		return "", false, sshDown(h.name)
	}
	c, ok := h.files[path]
	return c, ok, nil
}

func (h *host) WriteFile(path, content string, mode uint32) error {
	h.files[path] = content
	return nil
}

func (h *host) RunInput(command, stdin string) (string, error) {
	h.inputs = append(h.inputs, stdin)
	out, err := h.run(command, stdin)
	if err != nil {
		// Some tools quote their input back in an error. The fake does the
		// worst version of that, so a test can prove a secret never escapes.
		return out + "\n" + stdin, err
	}
	return out, nil
}

func (h *host) Run(command string) (string, error) { return h.run(command, "") }

func (h *host) short() string { return h.id[:16] }

func (h *host) factor() int {
	f, _ := apply.GarageReplication(h.files[storageadd.GarageToml])
	return f
}

var (
	assignRe  = regexp.MustCompile(`layout assign -z (\S+) -c (\S+) ([0-9a-f]{64})$`)
	applyRe   = regexp.MustCompile(`layout apply --version (\d+)$`)
	connectRe = regexp.MustCompile(`node connect ([0-9a-f]{64})@`)
	urlRe     = regexp.MustCompile(`url = "http://([^:/]+):[0-9]+/([^"]+)"`)
	methodRe  = regexp.MustCompile(`request = "(\w+)"`)
	bodyRe    = regexp.MustCompile(`data-binary = "([^"]*)"`)
	hostHdrRe = regexp.MustCompile(`Host: (\S+?)\.web\.garage\.internal' http://([^:]+):3902/(\S+)$`)
	mediaRe   = regexp.MustCompile(`https://([^/]+)/(\S+)$`)
)

func (h *host) run(command, stdin string) (string, error) {
	w := h.w
	h.commands = append(h.commands, command)
	if w.failOnce != "" && (strings.Contains(command, w.failOnce) || strings.Contains(stdin, w.failOnce)) {
		w.failOnce = ""
		return "failed by the test", fmt.Errorf("exit status 1")
	}
	if w.unreachable != "" && strings.Contains(command, w.unreachable) {
		return "ssh: connect to host 203.0.113.10 port 22: Operation timed out\n", sshDown(h.name)
	}
	if g, ok := strings.CutPrefix(command, garage.Command(storageadd.Fixture)+" "); ok {
		return h.garage(g)
	}
	switch {
	case command == "docker compose -f /srv/paisans/f2a9/infra/compose.yaml stop garage":
		h.running = false
		return "", nil
	case command == "docker compose -f /srv/paisans/f2a9/infra/compose.yaml up -d garage":
		h.start()
		return "", nil
	case strings.Contains(command, "ls -1 /srv/paisans/f2a9/infra/garage/meta"):
		var names []string
		if h.hasLayout {
			names = append(names, "cluster_layout")
		}
		for a := range h.asides {
			names = append(names, filepath.Base(a))
		}
		sort.Strings(names)
		return strings.Join(append(names, "db.lmdb", "node_key", "peer_list"), "\n") + "\n", nil
	case strings.Contains(command, "mv -n "+storageadd.LayoutFile+" "):
		fields := strings.Fields(command[strings.Index(command, "mv -n "):])
		aside := strings.TrimSuffix(fields[3], ";")
		if h.hasLayout {
			if h.asides[aside] {
				return "__PAISANS_LAYOUT_LEFT__\n__PAISANS_ASIDE_PRESENT__\n", fmt.Errorf("exit status 3")
			}
			h.asides[aside] = true
			h.hasLayout = false
		}
		return "", nil
	case strings.HasPrefix(command, "rm -f "):
		delete(h.files, strings.TrimPrefix(command, "rm -f "))
		return "", nil
	case strings.HasPrefix(command, "curl -fsS --max-time 20 -K -"):
		return h.s3(stdin)
	case strings.Contains(command, ".web.garage.internal"):
		m := hostHdrRe.FindStringSubmatch(command)
		if !w.upAt(m[2]) {
			return "curl: (7) Failed to connect", fmt.Errorf("exit status 7")
		}
		if body, ok := w.objects[m[1]+"/"+m[3]]; ok {
			return body, nil
		}
		return "curl: (22) The requested URL returned error: 404", fmt.Errorf("exit status 22")
	case strings.Contains(command, "--resolve "):
		// An app's media hostname is its alone, so the path is the object
		// key and the hostname says which bucket.
		m := mediaRe.FindStringSubmatch(command)
		for _, app := range w.cfg.AppNames() {
			a := w.cfg.Apps[app]
			if kinds.MediaHostname(a, w.cfg.Community.Domain) != m[1] {
				continue
			}
			if body, ok := w.objects[garage.BucketName(a, app)+"/"+m[2]]; ok {
				return body, nil
			}
		}
		return "curl: (22) The requested URL returned error: 404", fmt.Errorf("exit status 22")
	}
	return "", nil
}

// start is `up -d garage`: Garage refuses to start against a stored layout
// built at another factor, and a fresh metadata directory starts with an
// empty layout at version 0.
func (h *host) start() {
	if h.hasLayout && h.layoutFactor != h.factor() {
		h.running = false
		return
	}
	if !h.hasLayout {
		h.hasLayout = true
		h.layoutFactor = h.factor()
		h.version = 0
		h.roles = map[string]string{}
		h.staged = map[string]string{}
	}
	h.running = true
}

// component is every running node this one reaches through the peers it has
// met, itself included.
func (h *host) component() []*host {
	seen := map[string]bool{h.name: true}
	queue := []*host{h}
	var out []*host
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		out = append(out, cur)
		for name := range cur.peers {
			other := h.w.hosts[name]
			if !seen[name] && other.running {
				seen[name] = true
				queue = append(queue, other)
			}
		}
	}
	return out
}

func (h *host) garage(cmd string) (string, error) {
	w := h.w
	if !h.running {
		return "service \"garage\" is not running", fmt.Errorf("exit status 1")
	}
	switch {
	case cmd == "node id -q":
		return h.id + "@" + w.cfg.Sites[h.name].Address + ":3901\n", nil
	case cmd == "layout show":
		var b strings.Builder
		fmt.Fprintf(&b, "\x1b[2m2026-10-07T00:00:00Z\x1b[0m  INFO garage_net::netapp: Connection established to %s\n", h.short())
		b.WriteString("==== CURRENT CLUSTER LAYOUT ====\n")
		if len(h.roles) == 0 {
			b.WriteString("No nodes currently have a role in the cluster.\nSee `garage status` to view available nodes.\n")
		} else {
			b.WriteString("ID                Tags  Zone   Capacity  Usable capacity\n")
			for _, id := range sortedKeys(h.roles) {
				zone, capacity := splitRole(h.roles[id])
				fmt.Fprintf(&b, "%s        %s  %s  %s (100.0%%)\n", id, zone, capacity, capacity)
			}
		}
		fmt.Fprintf(&b, "\nCurrent cluster layout version: %d\n", h.version)
		return b.String(), nil
	case cmd == "status":
		var b strings.Builder
		b.WriteString("==== HEALTHY NODES ====\nID                Hostname  Address  Tags  Zone  Capacity  DataAvail\n")
		for _, n := range h.component() {
			zone, _ := splitRole(h.roles[n.short()])
			fmt.Fprintf(&b, "%s  %s  %s:3901  []  %s  100.0 GB  1 TB\n", n.short(), n.name, w.cfg.Sites[n.name].Address, zone)
		}
		return b.String(), nil
	case connectRe.MatchString(cmd):
		id := connectRe.FindStringSubmatch(cmd)[1]
		for _, other := range w.hosts {
			if other.id == id && other.running {
				h.peers[other.name] = true
				other.peers[h.name] = true
				return "Success.\n", nil
			}
		}
		return "Error: could not connect", fmt.Errorf("exit status 1")
	case assignRe.MatchString(cmd):
		m := assignRe.FindStringSubmatch(cmd)
		n, err := config.ParseSize(m[2])
		if err != nil {
			return "Error: invalid capacity", fmt.Errorf("exit status 1")
		}
		h.staged[m[3][:16]] = m[1] + " " + shown(n)
		return "Role changes are staged but not yet committed.\n", nil
	case applyRe.MatchString(cmd):
		v, _ := strconv.Atoi(applyRe.FindStringSubmatch(cmd)[1])
		if v != h.version+1 {
			return fmt.Sprintf("Error: Invalid new layout version: expected %d", h.version+1), fmt.Errorf("exit status 1")
		}
		roles := map[string]string{}
		for k, z := range h.roles {
			roles[k] = z
		}
		for k, z := range h.staged {
			roles[k] = z
		}
		if len(roles) < h.factor() {
			return "Error: The number of nodes with positive capacity is smaller than the replication factor", fmt.Errorf("exit status 1")
		}
		for _, n := range h.component() {
			n.version, n.roles, n.staged = v, roles, map[string]string{}
		}
		w.migrating, w.resyncing = w.migrate, w.resync
		return "New cluster layout with updated role assignment has been applied in cluster.\n", nil
	case cmd == "layout history":
		if w.migrating > 0 {
			w.migrating--
			return "==== LAYOUT HISTORY ====\nSeveral layout versions are currently live in the cluster, and data is being migrated.\n", nil
		}
		return "==== LAYOUT HISTORY ====\nYour cluster is currently in a stable state with a single live layout version.\n", nil
	case cmd == "stats":
		queue := 0
		if w.resyncing > 0 {
			w.resyncing--
			queue = 42
		}
		return fmt.Sprintf("Block manager stats:\n  number of RC entries (~= number of blocks): 10\n  resync queue length: %d\n  blocks with resync errors: 0\n", queue), nil
	}
	if _, ok := h.roles[h.short()]; !ok {
		return "Error: Internal error: Could not reach quorum of 1. 0 of 0 request succeeded", fmt.Errorf("exit status 1")
	}
	switch {
	case strings.HasPrefix(cmd, "key info "):
		if !w.keys[strings.TrimPrefix(cmd, "key info ")] {
			return "Error: 0 matching keys", fmt.Errorf("exit status 1")
		}
		return "Key name: x\n", nil
	case strings.HasPrefix(cmd, "key import "):
		w.keys[strings.Fields(cmd)[2]] = true
		return "", nil
	case strings.HasPrefix(cmd, "bucket info "):
		b := strings.TrimPrefix(cmd, "bucket info ")
		n, ok := w.buckets[b]
		if !ok {
			return "Error: Bucket not found / several matching buckets: " + b, fmt.Errorf("exit status 1")
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Bucket: 0123\n\nSize: 0 B (0 B)\nObjects: %d\n\nWebsite access: %v\n\nAuthorized keys:\n", n, w.website[b])
		if w.grants[b] {
			for _, app := range w.cfg.AppNames() {
				if garage.BucketName(w.cfg.Apps[app], app) == b {
					id, _ := garage.SecretString(w.secrets, app, "s3_access_key_id")
					fmt.Fprintf(&out, "  RWO  %s  %s\n", id, app)
				}
			}
		}
		return out.String(), nil
	case strings.HasPrefix(cmd, "bucket create "):
		w.buckets[strings.TrimPrefix(cmd, "bucket create ")] = 0
		return "", nil
	case strings.HasPrefix(cmd, "bucket allow "):
		f := strings.Fields(cmd)
		w.grants[f[len(f)-3]] = true
		return "", nil
	case strings.HasPrefix(cmd, "bucket website --allow "):
		w.website[strings.TrimPrefix(cmd, "bucket website --allow ")] = true
		return "", nil
	}
	return "", nil
}

// s3 is a signed request to Garage's S3 API, as curl's configuration on stdin
// describes it.
func (h *host) s3(stdin string) (string, error) {
	w := h.w
	m := urlRe.FindStringSubmatch(stdin)
	if !w.upAt(m[1]) {
		return "curl: (7) Failed to connect", fmt.Errorf("exit status 7")
	}
	path := m[2]
	switch methodRe.FindStringSubmatch(stdin)[1] {
	case "PUT":
		w.objects[path] = bodyRe.FindStringSubmatch(stdin)[1]
		return "", nil
	case "GET":
		if body, ok := w.objects[path]; ok {
			return body, nil
		}
		return "curl: (22) The requested URL returned error: 404", fmt.Errorf("exit status 22")
	case "DELETE":
		delete(w.objects, path)
		return "", nil
	}
	return "", fmt.Errorf("unexpected request")
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fixture(t *testing.T) (*config.Config, *config.Secrets) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, secrets
}

// newWorld is the fixture deployment as rendered and applied: every site's
// files in place with the manifest that records them. No Garage node is
// running yet; each test arranges its own.
func newWorld(t *testing.T, cfg *config.Config, secrets *config.Secrets) *world {
	t.Helper()
	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, cfg: cfg, secrets: secrets, hosts: map[string]*host{},
		buckets: map[string]int{}, keys: map[string]bool{}, grants: map[string]bool{}, website: map[string]bool{},
		objects: map[string]string{},
	}
	for i, name := range cfg.SiteNames() {
		sum := sha256.Sum256([]byte(name))
		h := &host{
			w: w, name: name, files: map[string]string{},
			id:     hex.EncodeToString(sum[:]),
			asides: map[string]bool{}, roles: map[string]string{}, staged: map[string]string{},
			peers: map[string]bool{},
		}
		_ = i
		w.hosts[name] = h
	}
	for _, f := range rendered.Files {
		site, rel, _ := strings.Cut(f.Path, "/")
		if rel == render.ManifestName {
			continue
		}
		w.hosts[site].files["/"+rel] = f.Content
	}
	for _, h := range w.hosts {
		h.recordManifest()
	}
	return w
}

// recordManifest writes the manifest apply would have left for what is on
// the host now, so the files read as apply's own.
func (h *host) recordManifest() {
	var m render.Manifest
	m.Version = 1
	for path, content := range h.files {
		if path == "/srv/paisans/f2a9/.paisans-manifest.json" {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		m.Files = append(m.Files, render.ManifestFile{Path: strings.TrimPrefix(path, "/"), SHA256: hex.EncodeToString(sum[:]), Mode: "0644"})
	}
	data, _ := json.Marshal(m)
	h.files["/srv/paisans/f2a9/.paisans-manifest.json"] = string(data)
}

// deployGarage puts a garage.toml at factor on site, as an apply at that
// factor would have, and starts its node.
func (w *world) deployGarage(site string, factor int) *host {
	h := w.hosts[site]
	toml := h.files[storageadd.GarageToml]
	toml = regexp.MustCompile(`(?m)^replication_factor = \d+$`).ReplaceAllString(toml, fmt.Sprintf("replication_factor = %d", factor))
	h.files[storageadd.GarageToml] = toml
	h.recordManifest()
	h.start()
	return h
}

// provisioned is a cluster of the given sites, each with a role and every
// app's key, bucket and grant in place, as storage init would leave it.
func (w *world) provisioned(factor int, sites ...string) {
	var hs []*host
	for _, s := range sites {
		hs = append(hs, w.deployGarage(s, factor))
	}
	roles := map[string]string{}
	for _, h := range hs {
		n, _ := config.ParseSize(w.cfg.Storage.Garage.CapacityFor(h.name))
		roles[h.short()] = h.name + " " + shown(n)
		for _, other := range hs {
			if other != h {
				h.peers[other.name] = true
			}
		}
	}
	for _, h := range hs {
		h.version, h.roles = 1, roles
	}
	for _, app := range w.cfg.AppNames() {
		a := w.cfg.Apps[app]
		id, ok := garage.SecretString(w.secrets, app, "s3_access_key_id")
		if !ok {
			continue
		}
		b := garage.BucketName(a, app)
		w.keys[id] = true
		w.buckets[b] = 7
		w.grants[b] = true
		w.website[b] = true
	}
}

func (w *world) transports() map[string]apply.Transport {
	out := map[string]apply.Transport{}
	for name, h := range w.hosts {
		out[name] = h
	}
	return out
}

func (w *world) build(opts storageadd.Options) *storageadd.Plan {
	w.t.Helper()
	p, err := storageadd.Build(w.cfg, w.secrets, w.transports(), opts)
	if err != nil {
		w.t.Fatalf("building the plan: %v", err)
	}
	return p
}

// splitRole is a fake role's zone and shown capacity.
func splitRole(role string) (zone, capacity string) {
	zone, capacity, _ = strings.Cut(role, " ")
	return zone, capacity
}

// shown is a capacity the way dxflrs/garage:v1.0.1's layout show prints it:
// decimal units, one decimal place.
func shown(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func zoneOf(h *host, of *host) string {
	zone, _ := splitRole(h.roles[of.short()])
	return zone
}

// upAt reports whether the Garage node at a mesh address is running, so a
// request to a stopped node fails the way a refused connection does.
func (w *world) upAt(address string) bool {
	for name, h := range w.hosts {
		if w.cfg.Sites[name].Address == address {
			return h.running
		}
	}
	return false
}

func (h *host) ran(sub string) int {
	n := 0
	for _, c := range h.commands {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}
