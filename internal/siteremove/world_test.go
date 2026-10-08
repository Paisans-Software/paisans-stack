package siteremove_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

const (
	ourID   = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"
	otherID = "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
	root    = "/srv/paisans/f2a9"
	wgConf  = "/etc/wireguard/psns-f2a9.conf"
	record  = "/etc/paisans/authorized_keys.ubuntu.paisans-f2a9.owned"
	theirs  = "/etc/paisans/authorized_keys.ubuntu.paisans-0c1d.owned"
	keysAt  = "/home/ubuntu/.ssh/authorized_keys"
	alice   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org"
)

var dep = deployment.Deployment{ID: ourID}

// world is a running deployment in maps: five hosts, etcd's membership,
// Patroni's members, Garage's layout and what each HAProxy serves. Every
// gate in site remove is about what a command on some host answers, so a
// fake that answers from shared state is the same test as five machines with
// the failures made reachable.
type world struct {
	t          *testing.T
	cfg        *config.Config
	secrets    *config.Secrets
	configPath string
	hosts      map[string]*host

	etcd     []apply.EtcdMember
	nextID   uint64
	etcdDown map[string]bool

	members  []map[string]any
	syncMode bool

	layout     map[string]string // short ID to zone
	staged     map[string]bool   // short IDs staged for removal
	version    int
	moving     int
	resyncing  int
	garageDown map[string]bool

	served map[string]string

	// Knobs that break one thing.
	failOnce    string
	unreachable map[string]bool
	caddyBroken bool
}

type host struct {
	w          *world
	name       string
	files      map[string]string
	commands   []string
	wgUp       bool
	patroniUp  bool
	etcdUp     bool
	containers []hostcheck.Container
	networks   []hostcheck.Network
	volumes    []hostcheck.Volume
	rules      []string
	handedUp   bool
}

func (h *host) Describe() string { return h.name }

func (h *host) ReadFile(path string) (string, bool, error) {
	if h.w.unreachable[h.name] {
		return "", false, fmt.Errorf("%s: %w", h.name, apply.ErrUnreachable)
	}
	c, ok := h.files[path]
	return c, ok, nil
}

func (h *host) WriteFile(path, content string, mode uint32) error {
	h.files[path] = content
	return nil
}

func (h *host) RunInput(command, stdin string) (string, error) { return h.Run(command) }

func (h *host) ran(sub string) int {
	n := 0
	for _, c := range h.commands {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

var (
	memberRemove = regexp.MustCompile(`member remove ([0-9a-f]+)`)
	switchover   = regexp.MustCompile(`switchover --leader (\S+) --candidate (\S+)`)
	memberDel    = regexp.MustCompile(`del /service/paisans/members/(\S+)`)
	serverRe     = regexp.MustCompile(`(?m)^\s+server (\S+) `)
	peerRe       = regexp.MustCompile(`(?m)^PublicKey = (\S+)`)
	fileLine     = regexp.MustCompile(`^f='([^']+)'; .* = '([0-9a-f]+)' \]`)
	layoutRemove = regexp.MustCompile(`^layout remove ([0-9a-f]+)$`)
	layoutApply  = regexp.MustCompile(`^layout apply --version (\d+)$`)
)

func (h *host) Run(command string) (string, error) {
	w := h.w
	h.commands = append(h.commands, command)
	if w.unreachable[h.name] {
		return "ssh: connect to host port 22: Operation timed out\n", fmt.Errorf("%s: %w after 3 attempts: exit status 255", h.name, apply.ErrUnreachable)
	}
	if w.failOnce != "" && strings.Contains(command, w.failOnce) {
		w.failOnce = ""
		return "failed by the test", errors.New("exit status 1")
	}
	switch {
	case command == "true":
		return "", nil

	// etcd
	case strings.Contains(command, "--quiet etcd"):
		if !h.etcdUp {
			return "__PAISANS_NO_ETCD__\n", nil
		}
		return w.memberList(), nil
	case strings.Contains(command, "endpoint health --cluster"):
		var out []map[string]any
		failed := false
		for _, m := range w.etcd {
			site := apply.EtcdMemberSite(w.cfg, m)
			ok := !w.etcdDown[site] && w.hosts[site] != nil && w.hosts[site].etcdUp
			failed = failed || !ok
			out = append(out, map[string]any{"endpoint": m.ClientURLs[0], "health": ok})
		}
		data, _ := json.Marshal(out)
		if failed {
			return string(data), errors.New("exit status 1")
		}
		return string(data), nil
	case strings.Contains(command, "member list -w json"):
		return w.memberList(), nil
	case memberRemove.MatchString(command):
		hex := memberRemove.FindStringSubmatch(command)[1]
		var kept []apply.EtcdMember
		for _, m := range w.etcd {
			if m.HexID() == hex {
				if other := w.hosts[apply.EtcdMemberSite(w.cfg, m)]; other != nil {
					other.etcdUp = false
				}
				continue
			}
			kept = append(kept, m)
		}
		w.etcd = kept
		return "Member removed\n", nil
	case memberDel.MatchString(command):
		name := memberDel.FindStringSubmatch(command)[1]
		var kept []map[string]any
		for _, m := range w.members {
			if m["Member"] != name {
				kept = append(kept, m)
			}
		}
		w.members = kept
		return "1\n", nil

	// Patroni
	case strings.Contains(command, "--quiet patroni"):
		if !h.patroniUp {
			return "__PAISANS_NO_PATRONI__\n", nil
		}
		return w.patroniList(), nil
	case strings.Contains(command, "patronictl") && strings.Contains(command, "list -f json"):
		return w.patroniList(), nil
	case strings.Contains(command, "show-config"):
		return fmt.Sprintf("loop_wait: 10\nsynchronous_mode: %v\n", w.syncMode), nil
	case strings.Contains(command, "edit-config"):
		w.syncMode = false
		for _, m := range w.members {
			if m["Role"] == "Sync Standby" {
				m["Role"] = "Replica"
			}
		}
		return "", nil
	case switchover.MatchString(command):
		sw := switchover.FindStringSubmatch(command)
		for _, m := range w.members {
			switch m["Member"] {
			case sw[1]:
				m["Role"], m["State"] = "Sync Standby", "streaming"
			case sw[2]:
				m["Role"], m["State"] = "Leader", "running"
			default:
				if m["Role"] == "Sync Standby" {
					m["Role"] = "Replica"
				}
			}
		}
		return "Successfully switched over\n", nil
	case strings.HasSuffix(command, "stop etcd; fi"):
		h.etcdUp = false
		return "", nil
	case strings.HasSuffix(command, "stop patroni; fi"):
		h.patroniUp = false
		wasSync := false
		for _, m := range w.members {
			if m["Member"] == h.name {
				wasSync = m["Role"] == "Sync Standby"
				m["Role"], m["State"] = "Replica", "stopped"
			}
		}
		if wasSync && w.syncMode {
			for _, m := range w.members {
				if m["Role"] == "Replica" && m["State"] == "streaming" {
					m["Role"] = "Sync Standby"
					break
				}
			}
		}
		return "", nil

	// Garage
	case strings.Contains(command, "exec -T garage /garage "):
		return w.garage(h, command[strings.Index(command, "/garage ")+len("/garage "):])

	// the mesh
	case strings.Contains(command, "ip link show psns-f2a9"):
		if h.wgUp {
			return "up\n", nil
		}
		return "down\n", nil
	case strings.Contains(command, "wg syncconf"):
		return "", nil
	case command == "wg show psns-f2a9 peers":
		var b strings.Builder
		for _, m := range peerRe.FindAllStringSubmatch(h.files[wgConf], -1) {
			b.WriteString(m[1] + "\n")
		}
		return b.String(), nil

	// HAProxy and the apps around its restart
	case strings.Contains(command, "/stats;csv"):
		return w.stats(h.name)
	case strings.HasSuffix(command, "restart haproxy"):
		w.served[h.name] = h.files[root+"/infra/haproxy/haproxy.cfg"]
		return "", nil
	case strings.Contains(command, " ps --all --format json"):
		return `{"Service":"x","Name":"x","State":"running","Health":""}` + "\n", nil

	// the hand over
	case strings.Contains(command, "ls -A '/srv/caddy'"):
		var b strings.Builder
		seen := map[string]bool{}
		for p := range h.files {
			if rest, ok := strings.CutPrefix(p, "/srv/caddy/"); ok {
				seen[strings.SplitN(rest, "/", 2)[0]] = true
			}
		}
		for name := range seen {
			b.WriteString("dst " + name + "\n")
		}
		for _, x := range []string{"data", "config"} {
			if h.under(root + "/infra/caddy/" + x) {
				b.WriteString("src " + x + "\n")
			}
		}
		if _, ok := h.files[root+"/infra/caddy/caddy.env"]; ok {
			b.WriteString("env\n")
		}
		return b.String(), nil
	case strings.HasPrefix(command, "install -m 600 "):
		h.files["/srv/caddy/caddy.env"] = h.files[root+"/infra/caddy/caddy.env"]
		return "", nil
	case strings.HasPrefix(command, "docker run --rm --network none"):
		if w.caddyBroken {
			return "Error: adapting config using caddyfile: unknown directive", errors.New("exit status 1")
		}
		return "Valid configuration\n", nil
	case strings.HasSuffix(command, "stop caddy; fi"):
		return "", nil
	case strings.HasPrefix(command, "set -e; for x in data config;"):
		h.move(root+"/infra/caddy", "/srv/caddy")
		return "", nil
	case strings.Contains(command, "/srv/caddy/compose.yaml down"):
		h.handedUp = false
		h.move("/srv/caddy", root+"/infra/caddy")
		return "", nil
	case strings.Contains(command, "/srv/caddy/compose.yaml up -d"):
		if !w.caddyBroken {
			h.handedUp = true
			h.containers = append(h.containers, hostcheck.Container{Name: "caddy-caddy-1", Project: "caddy", Service: "caddy", PID: 9})
		}
		return "", nil
	case strings.Contains(command, "/srv/caddy/compose.yaml ps --status running"):
		if h.handedUp {
			return "deadbeef\n", nil
		}
		return "", nil
	case strings.Contains(command, "/srv/caddy/compose.yaml exec -T caddy caddy validate"):
		return "Valid configuration\n", nil
	case strings.Contains(command, "caddy validate"), strings.Contains(command, "caddy reload"):
		return "", nil

	// the host stage
	case strings.Contains(command, `/etc/systemd/system/paisans-f2a9-*; do [ -e "$f" ] && echo`):
		var b strings.Builder
		for _, p := range h.sortedFiles() {
			if strings.HasPrefix(p, "/etc/systemd/system/paisans-f2a9-") {
				b.WriteString("unit " + p + "\n")
			} else if strings.HasPrefix(p, "/etc/systemd/system/") && strings.Contains(p, ".d/paisans-f2a9-") {
				b.WriteString("dropin " + p + "\n")
			}
		}
		return b.String(), nil
	case strings.Contains(command, `/etc/systemd/system/paisans-f2a9-*; do [ -e "$f" ] || continue`):
		for p := range h.files {
			if strings.HasPrefix(p, "/etc/systemd/system/paisans-f2a9-") || strings.HasPrefix(p, "/etc/systemd/system/") && strings.Contains(p, ".d/paisans-f2a9-") {
				delete(h.files, p)
			}
		}
		return "", nil
	case command == hostprep.AddedRulesProbe:
		var b strings.Builder
		for _, r := range h.rules {
			b.WriteString("rule " + r + "\n")
		}
		return b.String(), nil
	case strings.HasPrefix(command, "ufw delete "):
		line := strings.TrimPrefix(command, "ufw delete ")
		var kept []string
		for _, r := range h.rules {
			if r != line {
				kept = append(kept, r)
			}
		}
		h.rules = kept
		return "Rule deleted\n", nil
	case strings.Contains(command, "/etc/paisans/authorized_keys.ubuntu.paisans-*.owned"):
		var b strings.Builder
		for _, p := range h.sortedFiles() {
			if strings.HasPrefix(p, "/etc/paisans/authorized_keys.ubuntu.paisans-") {
				b.WriteString(p + "\n")
			}
		}
		return b.String(), nil
	case strings.HasPrefix(command, "getent passwd "):
		return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", nil
	case strings.HasPrefix(command, "d='") && strings.Contains(command, "du -sb"):
		dir := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(command, ";", 2)[0], "d='"), "'")
		return h.dirAnswer(dir), nil
	case strings.HasPrefix(command, "d='") && strings.Contains(command, "-empty -delete"):
		return "", nil
	case strings.HasPrefix(command, "rm -rf -- '"):
		dir := strings.TrimSuffix(strings.TrimPrefix(command, "rm -rf -- '"), "'")
		for p := range h.files {
			if strings.HasPrefix(p, dir+"/") {
				delete(h.files, p)
			}
		}
		return "", nil
	case strings.Contains(command, "docker ps -aq --no-trunc --filter 'label=community.paisans.deployment="+ourID+"'"):
		var kept []hostcheck.Container
		for _, c := range h.containers {
			if c.Deployment != ourID {
				kept = append(kept, c)
			}
		}
		h.containers = kept
		var nets []hostcheck.Network
		for _, n := range h.networks {
			if n.Deployment != ourID {
				nets = append(nets, n)
			}
		}
		h.networks = nets
		return "", nil
	case strings.Contains(command, "docker volume ls -q --filter 'label=community.paisans.deployment="+ourID+"'"):
		var kept []hostcheck.Volume
		for _, v := range h.volumes {
			if v.Deployment != ourID {
				kept = append(kept, v)
			}
		}
		h.volumes = kept
		return "", nil
	case strings.HasPrefix(command, "systemctl disable --now wg-quick@psns-f2a9"):
		h.wgUp = false
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
	case strings.HasPrefix(command, "rm -f -- '"):
		delete(h.files, strings.TrimSuffix(strings.TrimPrefix(command, "rm -f -- '"), "'"))
		return "", nil
	case strings.Contains(command, "awk ") && strings.Contains(command, registry.Path):
		if command != registry.RemoveCommand(ourID) {
			return "", fmt.Errorf("not the removal: %s", command)
		}
		content, ok := h.files[registry.Path]
		if !ok {
			return "", nil
		}
		r, err := registry.Parse([]byte(content))
		if err != nil {
			return "paisans-registry-unreadable", errors.New("exit status 4")
		}
		data, _ := registry.Encode(registry.Remove(r, ourID))
		h.files[registry.Path] = string(data)
		return "", nil
	case strings.Contains(command, "grep -vxF "):
		lines := strings.Split(h.files[keysAt], "\n")
		var kept []string
		for _, l := range lines {
			if !strings.Contains(command, shellQuote(l)) || l == "" {
				kept = append(kept, l)
			}
		}
		h.files[keysAt] = strings.Join(kept, "\n")
		return "", nil
	case strings.HasSuffix(command, " stop"), strings.HasSuffix(command, " up -d"):
		return "", nil
	}
	return "", fmt.Errorf("%s: unexpected command %q", h.name, command)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (h *host) sortedFiles() []string {
	var out []string
	for p := range h.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (h *host) under(dir string) bool {
	for p := range h.files {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// move moves data and config from one directory to another, each only when
// it is there and the target is not, as the hand over's mv does.
func (h *host) move(from, to string) {
	for _, x := range []string{"data", "config"} {
		if !h.under(from+"/"+x) || h.under(to+"/"+x) {
			continue
		}
		for p, c := range h.files {
			if rest, ok := strings.CutPrefix(p, from+"/"+x+"/"); ok {
				h.files[to+"/"+x+"/"+rest] = c
				delete(h.files, p)
			}
		}
	}
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

func (w *world) memberList() string {
	data, _ := json.Marshal(map[string]any{"header": map[string]any{}, "members": w.etcd})
	return string(data)
}

func (w *world) patroniList() string {
	data, _ := json.Marshal(w.members)
	return string(data)
}

func (w *world) leader() string {
	for _, m := range w.members {
		if m["Role"] == "Leader" {
			return m["Member"].(string)
		}
	}
	return ""
}

func (w *world) member(name string) map[string]any {
	for _, m := range w.members {
		if m["Member"] == name {
			return m
		}
	}
	return nil
}

func (w *world) stats(site string) (string, error) {
	cfg := w.served[site]
	if !strings.Contains(cfg, "listen stats") {
		return "curl: (7) Failed to connect", errors.New("exit status 7")
	}
	var b strings.Builder
	b.WriteString("# pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,\n")
	b.WriteString("postgres,FRONTEND,,,0,0,500,0,0,0,0,0,0,,,,,OPEN,\n")
	for _, m := range serverRe.FindAllStringSubmatch(cfg, -1) {
		status := "DOWN"
		if m[1] == w.leader() {
			status = "UP"
		}
		fmt.Fprintf(&b, "postgres,%s,0,0,0,0,100,0,0,0,,0,,0,0,0,0,%s,\n", m[1], status)
	}
	b.WriteString("postgres,BACKEND,0,0,0,0,50,0,0,0,0,0,,0,0,0,0,UP,\n")
	return b.String(), nil
}

func (w *world) garage(h *host, cmd string) (string, error) {
	switch {
	case cmd == "layout show":
		var b strings.Builder
		b.WriteString("==== CURRENT CLUSTER LAYOUT ====\nID                Tags  Zone   Capacity  Usable capacity\n")
		var ids []string
		for id := range w.layout {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&b, "%s  []  %s  100.0 GB  100.0 GB (100.0%%)\n", id, w.layout[id])
		}
		fmt.Fprintf(&b, "\nCurrent cluster layout version: %d\n", w.version)
		return b.String(), nil
	case cmd == "status":
		var b strings.Builder
		b.WriteString("==== HEALTHY NODES ====\nID                Hostname  Address  Tags  Zone  Capacity  DataAvail\n")
		for id, zone := range w.layout {
			if !w.garageDown[zone] {
				fmt.Fprintf(&b, "%s  %s  10.44.0.9:3901  []  %s  100.0 GB  1 TB\n", id, zone, zone)
			}
		}
		return b.String(), nil
	case layoutRemove.MatchString(cmd):
		w.staged[layoutRemove.FindStringSubmatch(cmd)[1]] = true
		return "Role removal is staged but not yet committed.\n", nil
	case layoutApply.MatchString(cmd):
		v, _ := strconv.Atoi(layoutApply.FindStringSubmatch(cmd)[1])
		if v != w.version+1 {
			return fmt.Sprintf("Error: Invalid new layout version: expected %d", w.version+1), errors.New("exit status 1")
		}
		for id := range w.staged {
			delete(w.layout, id)
		}
		w.staged = map[string]bool{}
		w.version = v
		w.moving, w.resyncing = 2, 2
		return "New cluster layout with updated role assignment has been applied in cluster.\n", nil
	case cmd == "layout history":
		if w.moving > 0 {
			w.moving--
			return "==== LAYOUT HISTORY ====\nSeveral layout versions are currently live in the cluster, and data is being migrated.\n", nil
		}
		return "==== LAYOUT HISTORY ====\nYour cluster is currently in a stable state with a single live layout version.\n", nil
	case cmd == "stats":
		queue := 0
		if w.resyncing > 0 {
			w.resyncing--
			queue = 42
		}
		return fmt.Sprintf("Block manager stats:\n  resync queue length: %d\n  blocks with resync errors: 0\n", queue), nil
	}
	return "", fmt.Errorf("%s: unexpected garage command %q", h.name, cmd)
}

// inspect stands in for hostcheck.Inspect, from the fake host's maps.
func (w *world) inspect(t apply.Transport, cfg *config.Config) (*hostcheck.Inventory, error) {
	h := t.(*host)
	if w.unreachable[h.name] {
		return nil, fmt.Errorf("%s: %w", h.name, apply.ErrUnreachable)
	}
	inv := &hostcheck.Inventory{Host: h.name, Docker: hostcheck.Docker{Present: true}, ManifestPath: dep.Manifest()}
	inv.Containers = append(inv.Containers, h.containers...)
	inv.Networks = append(inv.Networks, h.networks...)
	inv.Volumes = append(inv.Volumes, h.volumes...)
	if content, ok := h.files[dep.Manifest()]; ok {
		inv.Manifest = true
		var m render.Manifest
		if err := json.Unmarshal([]byte(content), &m); err == nil {
			inv.ManifestFiles = m.Files
			if inv.ManifestFiles == nil {
				inv.ManifestFiles = []render.ManifestFile{}
			}
			for _, f := range m.Files {
				if f.Path == dep.WireGuardConf() {
					inv.ManifestWireGuard = true
				}
			}
		}
	}
	if h.wgUp {
		inv.Links = []string{dep.Interface()}
	}
	for _, p := range h.sortedFiles() {
		if strings.HasPrefix(p, render.HostSitesDir+"/") && strings.HasSuffix(p, ".caddy") {
			inv.HostSites = append(inv.HostSites, p)
		}
	}
	return inv, nil
}

func (w *world) transports() map[string]apply.Transport {
	out := map[string]apply.Transport{}
	for name, h := range w.hosts {
		out[name] = h
	}
	return out
}

func (w *world) options() siteremove.Options {
	return siteremove.Options{ConfigPath: w.configPath}
}

func (w *world) build(site string, o siteremove.Options) (*siteremove.Plan, error) {
	o.ConfigPath = w.configPath
	return siteremove.Build(w.cfg, w.secrets, site, w.transports(), o)
}

func (w *world) mustBuild(site string, o siteremove.Options) *siteremove.Plan {
	w.t.Helper()
	p, err := w.build(site, o)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

func sum(s string) string { return hexSum(s) }

func hexSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func mustTime() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }

// otherKey is a second key on the login user, which no deployment's record
// lists: what an operator added by hand.
func otherKey(t *testing.T) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " bob@example.org"
}

func fingerprint(t *testing.T, line string) string {
	k, _, err := config.ParseKeyLine(line)
	if err != nil {
		t.Fatal(err)
	}
	return k.Fingerprint
}

// worldConfig is the fixture grown to the shape a removal needs: three data
// sites, so one can go and leave a cluster of two with three etcd voters; a
// third Garage node, so replication 2 survives losing one; and the apps
// pinned to home-b moved to home-a.
func worldConfig(t *testing.T, edits ...func(string) string) (*config.Config, *config.Secrets, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	text = strings.Replace(text, "  vm:\n    roles: [gateway, witness]", `  # The third data site.
  home-c:
    roles: [data]
    address: 10.44.0.5
    public_address: 203.0.113.50
    ssh:
      host: home-c.local
      user: ubuntu
      public_key: |
        `+alice+`
  vm:
    roles: [gateway, witness]`, 1)
	text = strings.Replace(text, "    address: 10.44.0.2\n", "    address: 10.44.0.2\n    public_address: 203.0.113.20\n", 1)
	text = strings.Replace(text, "cluster:\n  sites: [home-a, home-b]", "cluster:\n  sites: [home-a, home-b, home-c]", 1)
	text = strings.Replace(text, "  members: [home-a, home-b, vm]", "  members: [home-a, home-b, home-c, vm]", 1)
	text = strings.Replace(text, "    sites: [home-a, home-b]\n    replication: 2", "    sites: [home-a, home-b, home-c]\n    replication: 2", 1)
	text = strings.Replace(text, "placement: { pinned: home-b }", "placement: { pinned: home-a }", -1)
	for _, edit := range edits {
		text = edit(text)
	}
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets.Sites["home-c"] = config.SiteSecrets{WireGuardPrivateKey: "REREREREREREREREREREREREREREREREREREREREREQ="}
	// box is a site only some worlds declare.
	secrets.Sites["box"] = config.SiteSecrets{WireGuardPrivateKey: "RUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUU="}
	return cfg, secrets, path
}

// newWorld is the deployment before the removal: every site applied from
// the configuration as it stands, home-a leading with home-b its Sync
// Standby, three Garage nodes, and on home-b a host prepare's units, rules
// and key record beside things that are not this deployment's.
func newWorld(t *testing.T, edits ...func(string) string) *world {
	t.Helper()
	cfg, secrets, path := worldConfig(t, edits...)
	w := &world{t: t, cfg: cfg, secrets: secrets, configPath: path, hosts: map[string]*host{},
		etcdDown: map[string]bool{}, syncMode: true, staged: map[string]bool{}, version: 3,
		garageDown: map[string]bool{}, served: map[string]string{}, unreachable: map[string]bool{}}
	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cfg.SiteNames() {
		h := &host{w: w, name: name, files: map[string]string{}, wgUp: true}
		w.hosts[name] = h
		var entries []render.ManifestFile
		for _, f := range rendered.Files {
			rel, ok := strings.CutPrefix(f.Path, name+"/")
			if !ok || rel == render.ManifestName {
				continue
			}
			h.files["/"+rel] = f.Content
			entries = append(entries, render.ManifestFile{Path: rel, SHA256: hexSum(f.Content), Mode: fmt.Sprintf("%04o", f.Mode)})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		manifest, _ := json.MarshalIndent(render.Manifest{Version: 1, Files: entries}, "", "  ")
		h.files[dep.Manifest()] = string(manifest) + "\n"
		if contains(cfg.Etcd.Members, name) {
			h.etcdUp = true
			w.nextID++
			addr := cfg.Sites[name].Address
			w.etcd = append(w.etcd, apply.EtcdMember{ID: w.nextID * 0x1111, Name: name, PeerURLs: []string{"http://" + addr + ":2380"}, ClientURLs: []string{"http://" + addr + ":2379"}})
		}
		if contains(cfg.Cluster.Sites, name) {
			h.patroniUp = true
		}
		if proxy, ok := h.files[root+"/infra/haproxy/haproxy.cfg"]; ok {
			w.served[name] = proxy
		}
		reg := registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{}}
		id, e := registry.For(cfg, name, mustTime())
		reg.Deployments[id] = e
		h.files[registry.Path] = encode(t, reg)
		h.containers = append(h.containers, hostcheck.Container{Name: "paisans-f2a9-infra-etcd-1", Project: "paisans-f2a9-infra", Deployment: ourID, PID: 10})
		h.networks = append(h.networks, hostcheck.Network{Name: "bridge"})
	}
	for i, name := range cfg.Cluster.Sites {
		role, state := "Replica", "streaming"
		switch i {
		case 0:
			role, state = "Leader", "running"
		case 1:
			role = "Sync Standby"
		}
		w.members = append(w.members, map[string]any{"Member": name, "Role": role, "State": state, "Replay Lag": float64(0)})
	}
	w.layout = map[string]string{}
	for i, name := range cfg.Storage.Garage.Sites {
		w.layout[strings.Repeat(string("abcdef"[i]), 16)] = name
	}

	b := w.hosts["home-b"]
	if b == nil {
		return w
	}
	b.containers = append(b.containers,
		hostcheck.Container{Name: "paisans-f2a9-talk-app-1", Project: "paisans-f2a9-talk", Deployment: ourID, PID: 11},
		hostcheck.Container{Name: "someone-elses-db", Project: "theirs"},
		hostcheck.Container{Name: "paisans-0c1d-infra-etcd-1", Project: "paisans-0c1d-infra", Deployment: otherID},
	)
	b.networks = append(b.networks,
		hostcheck.Network{Name: "paisans-f2a9-talk_default", Project: "paisans-f2a9-talk", Deployment: ourID},
		hostcheck.Network{Name: "theirs_default", Project: "theirs"},
	)
	b.volumes = append(b.volumes, hostcheck.Volume{Name: "paisans-f2a9-talk_media", Project: "paisans-f2a9-talk", Deployment: ourID})
	b.files[root+"/infra/postgres/PG_VERSION"] = "18\n"
	b.files["/etc/systemd/system/paisans-f2a9-watchdog.service"] = "[Unit]\n"
	b.files["/etc/systemd/system/docker.service.d/paisans-f2a9-after-wireguard.conf"] = "[Unit]\n"
	b.files["/etc/systemd/system/paisans-0c1d-watchdog.service"] = "[Unit]\n"
	b.files["/etc/systemd/system/docker.service.d/override.conf"] = "[Service]\n"
	b.rules = []string{
		"allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'",
		"allow in on psns-f2a9 comment 'paisans-f2a9: the mesh'",
		"allow 51821/udp comment 'paisans-0c1d: wireguard'",
		"allow 8080/tcp",
	}
	bob := otherKey(t)
	b.files[keysAt] = "# managed by hand\n" + alice + "\n" + bob + "\n"
	b.files[record] = "# Keys paisans host prepare added.\n" + fingerprint(t, alice) + " alice@example.org\n"
	reg := registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{}}
	id, e := registry.For(cfg, "home-b", mustTime())
	reg.Deployments[id] = e
	reg.Deployments[otherID] = registry.Entry{Token: "0c1d", Root: "/srv/paisans/0c1d", Domain: "example.net", Site: "x"}
	b.files[registry.Path] = encode(t, reg)
	return w
}

func encode(t *testing.T, r registry.Registry) string {
	data, err := registry.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
