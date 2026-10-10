package siteremove_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

const (
	caddyID   = "c0ffee0000000000000000000000000000000000000000000000000000000001"
	caddyfile = root + "/infra/caddy/Caddyfile"
	blog      = render.HostSitesDir + "/blog.caddy"
)

// gatewayWorld has a second gateway on home-c, so vm, a gateway and the
// witness, may go, with this deployment's Caddy running on vm and its
// certificates there; ownerSites puts a site block of the host owner's in
// /srv/caddy.d.
func gatewayWorld(t *testing.T, ownerSites bool) *world {
	w := newWorld(t,
		replace("  home-c:\n    roles: [data]", "  home-c:\n    roles: [data, gateway]"),
		replace("placement: { pinned: vm }", "placement: { pinned: box }"),
		replace("  vm:\n    roles: [gateway, witness]", "  box:\n    roles: [apps]\n    address: 10.44.0.6\n    ssh:\n      host: box.local\n      user: ubuntu\n      public_key: |\n        "+alice+"\n  vm:\n    roles: [gateway, witness]"),
	)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	vm := w.hosts["vm"]
	vm.containers = append(vm.containers, hostcheck.Container{ID: caddyID, Name: "paisans-f2a9-infra-caddy-1", Project: "paisans-f2a9-infra", Service: "caddy", Deployment: ourID, Image: caddyImage(t, vm), PID: 12, Networks: []string{"host"}})
	vm.images[caddyImage(t, vm)] = true
	vm.files[root+"/infra/caddy/data/caddy/certificates/blog.example.net.crt"] = "CERT"
	vm.files[root+"/infra/caddy/config/caddy/autosave.json"] = "{}"
	vm.files[deployrecord.Path(dep)] = recordListing("vm")
	if ownerSites {
		vm.files[blog] = "blog.example.net {\n\timport upstream_unavailable\n\trespond \"hi\"\n}\n"
	}
	return w
}

// caddyImage is the ID of the image the infrastructure compose file names
// for Caddy, as the image probe answers it.
func caddyImage(t *testing.T, h *host) string {
	refs, err := apply.ComposeImages(h.files[root+"/infra/compose.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if strings.Contains(r, "caddy") {
			return "sha256:" + hexSum(r)
		}
	}
	t.Fatal("the infrastructure compose file names no Caddy image")
	return ""
}

func vmDest() config.Destination {
	d, _ := config.ParseDestination("ubuntu@192.0.2.30")
	return d
}

// siteBlocks are the hostnames a Caddyfile opens a site block for.
func siteBlocks(caddyfile string) []string {
	var out []string
	for _, l := range strings.Split(caddyfile, "\n") {
		if strings.HasSuffix(l, " {") && l != "{" && !strings.HasPrefix(l, "(") && !strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, "#") {
			out = append(out, strings.TrimSuffix(l, " {"))
		}
	}
	return out
}

func vmRegistry(t *testing.T, w *world) registry.Registry {
	r, err := registry.Parse([]byte(w.hosts["vm"].files[registry.Path]))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func index(commands []string, sub string) int {
	for i, c := range commands {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// With no site of the owner's in /srv/caddy.d, Caddy goes like everything
// else: nothing of the deployment's is left.
func TestAGatewayNobodyReliesOnIsRemovedCompletely(t *testing.T) {
	w := gatewayWorld(t, false)
	p := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
	if len(p.CaddyKept) != 0 || hasStepIn(stageNamed(p, "clean the host"), "vm", "reduce", "") {
		t.Fatalf("Caddy is kept with nothing relying on it:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	vm := w.hosts["vm"]
	for _, c := range vm.containers {
		if c.Deployment == ourID {
			t.Errorf("container %s is left", c.Name)
		}
	}
	if vm.under(root) {
		t.Errorf("files are left under %s: %v", root, vm.sortedFiles())
	}
	if _, ok := vmRegistry(t, w).Deployments[ourID]; ok {
		t.Error("the registry entry is left")
	}
	if len(vm.caddyLoads) != 0 {
		t.Error("Caddy was reloaded")
	}
}

// With a site of the owner's there, Caddy stays, serving only it, and
// everything else of the deployment's goes, Caddy's files kept even with
// --delete-data.
func TestAGatewaysCaddyIsKeptForTheOwnersSites(t *testing.T) {
	w := gatewayWorld(t, true)
	vm := w.hosts["vm"]
	original := vm.files[caddyfile]
	if len(siteBlocks(original)) == 0 {
		t.Fatal("the rendered Caddyfile has no site block to strip")
	}
	p := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
	var first siteremove.Step
	for _, s := range stageNamed(p, "clean the host").Steps {
		if s.Verb != "note" {
			first = s
			break
		}
	}
	if first.Verb != "reduce" || !strings.Contains(first.Text, blog) {
		t.Fatalf("reducing Caddy is not the first step:\n%s", printed(p))
	}
	if strings.Join(p.CaddyKept, ",") != blog {
		t.Errorf("CaddyKept = %v", p.CaddyKept)
	}
	if plan := printed(p); !strings.Contains(plan, "kept with --delete-data") {
		t.Errorf("the plan does not say Caddy's files are kept with --delete-data:\n%s", plan)
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}

	reduced := vm.files[caddyfile]
	for _, want := range []string{"\temail admin@", "acme_dns", "(upstream_unavailable) {", "(upstream_failover) {", "(upstream_single) {", "import /etc/caddy.d/*.caddy"} {
		if !strings.Contains(reduced, want) {
			t.Errorf("the reduced Caddyfile has no %q:\n%s", want, reduced)
		}
	}
	if blocks := siteBlocks(reduced); len(blocks) != 0 {
		t.Errorf("the reduced Caddyfile still serves %v", blocks)
	}
	if len(vm.caddyLoads) != 1 || vm.caddyLoads[0] != reduced {
		t.Errorf("Caddy loaded %d configuration(s), want the reduced one once", len(vm.caddyLoads))
	}
	validate, reload, write, removal := index(vm.commands, "caddy validate"), index(vm.commands, "caddy reload"), index(vm.commands, "> '"+caddyfile+"'"), index(vm.commands, "docker ps -aq --no-trunc")
	if !(validate >= 0 && validate < reload && reload < write && write < removal) {
		t.Errorf("validate %d, reload %d, write %d, first removal %d: want them in that order", validate, reload, write, removal)
	}
	if _, ok := vm.files[root+"/infra/caddy/snippets/.paisans-kept-Caddyfile"]; ok {
		t.Error("the staged file is left")
	}

	kept, others := 0, 0
	for _, c := range vm.containers {
		switch {
		case c.ID == caddyID:
			kept++
		case c.Deployment == ourID:
			others++
		}
	}
	if kept != 1 || others != 0 {
		t.Errorf("Caddy kept %d, other containers of the deployment's left %d", kept, others)
	}
	if !vm.images[caddyImage(t, vm)] || vm.images["sha256:etcd"] {
		t.Errorf("images: %v", vm.images)
	}
	var left []string
	for _, f := range vm.sortedFiles() {
		if strings.HasPrefix(f, root+"/") {
			left = append(left, strings.TrimPrefix(f, root+"/"))
		}
	}
	want := []string{".paisans-manifest.json", "infra/caddy/Caddyfile", "infra/caddy/caddy.env", "infra/caddy/config/caddy/autosave.json", "infra/caddy/data/caddy/certificates/blog.example.net.crt", "infra/compose.yaml"}
	if strings.Join(left, "\n") != strings.Join(want, "\n") {
		t.Errorf("left under the root:\n%s\nwant:\n%s", strings.Join(left, "\n"), strings.Join(want, "\n"))
	}
	var m render.Manifest
	if err := json.Unmarshal([]byte(vm.files[dep.Manifest()]), &m); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, f := range m.Files {
		listed = append(listed, f.Path)
		if f.SHA256 != hexSum(vm.files["/"+f.Path]) {
			t.Errorf("the manifest's hash of %s is not the file's", f.Path)
		}
	}
	sort.Strings(listed)
	if strings.Join(listed, ",") != dep.RelPath("infra", "caddy", "Caddyfile")+","+dep.RelPath("infra", "caddy", "caddy.env")+","+dep.RelPath("infra", "compose.yaml") {
		t.Errorf("the manifest lists %v", listed)
	}
	if e := vmRegistry(t, w).Deployments[ourID]; e.Kept != registry.KeptCaddy || e.Roles == "" {
		t.Errorf("the registry entry: %+v", e)
	}
	if _, ok := vm.files[deployrecord.Path(dep)]; ok {
		t.Error("the deployment record is left")
	}
	if _, ok := vm.files[blog]; !ok {
		t.Error("the owner's site block was touched")
	}
	if remains := strings.Join(p.Remains(), "\n"); !strings.Contains(remains, "Caddy kept") || !strings.Contains(remains, blog) {
		t.Errorf("the remains do not say Caddy is kept:\n%s", remains)
	}
}

// A reduced Caddyfile that does not validate, or a reload that fails, puts
// the original back and stops before anything else is removed.
func TestAFailedReductionChangesNothing(t *testing.T) {
	for name, fail := range map[string]func(*world){
		"validate": func(w *world) { w.caddyBroken = true },
		"reload":   func(w *world) { w.failOnce = "caddy reload --config '/etc/caddy/snippets/" },
	} {
		t.Run(name, func(t *testing.T) {
			w := gatewayWorld(t, true)
			vm := w.hosts["vm"]
			before := map[string]string{}
			for k, v := range vm.files {
				before[k] = v
			}
			containers := len(vm.containers)
			p := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
			fail(w)
			err := siteremove.Execute(p)
			if err == nil || !strings.Contains(err.Error(), "nothing else was removed") {
				t.Fatalf("err = %v", err)
			}
			if len(vm.files) != len(before) {
				t.Errorf("%d files before, %d after", len(before), len(vm.files))
			}
			for k, v := range before {
				if vm.files[k] != v {
					t.Errorf("%s changed", k)
				}
			}
			if len(vm.containers) != containers {
				t.Error("a container was removed")
			}
			for _, loaded := range vm.caddyLoads {
				if loaded != before[caddyfile] {
					t.Error("Caddy was left on another configuration than the original")
				}
			}
			if name == "reload" && len(vm.caddyLoads) == 0 {
				t.Error("the original was not reloaded after the failed reload")
			}
		})
	}
}

// A Caddy that is not running, or a Caddyfile without what the owner's
// sites need, is refused with nothing changed.
func TestAReductionThatCannotBeDoneIsRefused(t *testing.T) {
	for name, edit := range map[string]func(*host){
		"not running": func(h *host) {
			for i := range h.containers {
				if h.containers[i].ID == caddyID {
					h.containers[i].PID = 0
				}
			}
		},
		"no import": func(h *host) {
			h.files[caddyfile] = strings.Replace(h.files[caddyfile], "import /etc/caddy.d/*.caddy", "", 1)
		},
		"no snippet": func(h *host) {
			h.files[caddyfile] = strings.Replace(h.files[caddyfile], "(upstream_single) {", "(other) {", 1)
		},
		"no global block": func(h *host) {
			h.files[caddyfile] = strings.Replace(h.files[caddyfile], "\n{\n", "\n", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := gatewayWorld(t, true)
			vm := w.hosts["vm"]
			edit(vm)
			_, err := siteremove.BuildForced(w.cfg, w.secrets, "vm", vmDest(), vm, siteremove.Options{})
			if err == nil || !strings.Contains(err.Error(), "Nothing was changed") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// Run again while the owner's sites are there, nothing changes; once they
// are gone, Caddy and the entry go too.
func TestRunningAgainKeepsCaddyUntilTheSitesAreGone(t *testing.T) {
	w := gatewayWorld(t, true)
	vm := w.hosts["vm"]
	image := caddyImage(t, vm)
	if err := siteremove.Execute(forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{})); err != nil {
		t.Fatal(err)
	}
	again := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
	if again.Pending() || strings.Join(again.CaddyKept, ",") != blog {
		t.Fatalf("a run with the sites still there plans something, or does not say Caddy is kept (%v):\n%s", again.CaddyKept, printed(again))
	}
	n := len(vm.commands)
	if err := siteremove.Execute(again); err != nil {
		t.Fatal(err)
	}
	for _, c := range vm.commands[n:] {
		if strings.Contains(c, "rm ") || strings.Contains(c, "docker rm") || strings.Contains(c, "caddy reload") || strings.HasPrefix(c, "cat > ") {
			t.Errorf("a run with nothing to do ran %s", c)
		}
	}

	delete(vm.files, blog)
	last := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
	if len(last.CaddyKept) != 0 || !last.Pending() {
		t.Fatalf("with the sites gone:\n%s", printed(last))
	}
	if err := siteremove.Execute(last); err != nil {
		t.Fatal(err)
	}
	for _, c := range vm.containers {
		if c.Deployment == ourID {
			t.Errorf("container %s is left", c.Name)
		}
	}
	if vm.under(root) {
		t.Errorf("files are left: %v", vm.sortedFiles())
	}
	if _, ok := vmRegistry(t, w).Deployments[ourID]; ok {
		t.Error("the registry entry is left")
	}
	if vm.images[image] {
		t.Error("Caddy's image is left")
	}
}

// --id builds the reduced Caddyfile from the host's, as a run with the
// configuration does, so the two leave the same Caddy.
func TestByIDKeepsTheSameCaddyAsTheConfiguration(t *testing.T) {
	results := map[string]string{}
	for _, how := range []string{"config", "id"} {
		w := gatewayWorld(t, true)
		var p *siteremove.Plan
		if how == "config" {
			p = forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
		} else {
			var err error
			if p, err = siteremove.BuildForcedByID(vmDest(), w.hosts["vm"], "f2a9", siteremove.Options{DeleteData: true}); err != nil {
				t.Fatal(err)
			}
		}
		if err := siteremove.Execute(p); err != nil {
			t.Fatalf("%s: %v", how, err)
		}
		vm := w.hosts["vm"]
		var b strings.Builder
		for _, f := range vm.sortedFiles() {
			if strings.HasPrefix(f, root+"/") {
				b.WriteString(f + " " + hexSum(vm.files[f]) + "\n")
			}
		}
		e := vmRegistry(t, w).Deployments[ourID]
		b.WriteString("kept " + e.Kept + "\n")
		results[how] = b.String()
	}
	if results["config"] != results["id"] {
		t.Errorf("with the configuration:\n%s\nwith --id:\n%s", results["config"], results["id"])
	}
}

// The question --execute asks with --id says Caddy is kept for the owner's
// sites, and what --delete-data deletes.
func TestByIDConfirmationNamesTheKeptCaddy(t *testing.T) {
	w := gatewayWorld(t, true)
	p, err := siteremove.BuildForcedByID(vmDest(), w.hosts["vm"], "f2a9", siteremove.Options{DeleteData: true})
	if err != nil {
		t.Fatal(err)
	}
	q := p.Confirmation(vmDest())
	for _, want := range []string{ourID, "vm", blog, "Caddy is kept", "data"} {
		if !strings.Contains(q, want) {
			t.Errorf("no %q in %q", want, q)
		}
	}
	w = setup(t)
	q = byID(t, w, "watch", "f2a9", siteremove.Options{}).Confirmation(vmDest())
	if strings.Contains(q, "Caddy") || strings.Contains(q, "data") {
		t.Errorf("a monitor without --delete-data: %q", q)
	}
}

// The rules host prepare opened 80 and 443 with stay while Caddy is kept:
// ufw denies incoming by default, so without them nothing from outside
// reaches the owner's sites. The run that removes Caddy deletes them.
func TestAKeptCaddyKeepsItsWebRules(t *testing.T) {
	w := gatewayWorld(t, true)
	vm := w.hosts["vm"]
	web := []string{
		"allow 80/tcp comment 'paisans-f2a9: the gateway, HTTP'",
		"allow 443/tcp comment 'paisans-f2a9: the gateway, HTTPS'",
	}
	mesh := "allow in on psns-f2a9 comment 'paisans-f2a9: the mesh: etcd, Patroni, Garage, HAProxy'"
	vm.rules = append([]string{"allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'", mesh}, web...)
	if err := siteremove.Execute(forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{})); err != nil {
		t.Fatal(err)
	}
	for _, r := range web {
		if !contains(vm.rules, r) {
			t.Errorf("%s was deleted with Caddy kept", r)
		}
	}
	if contains(vm.rules, mesh) {
		t.Error("the mesh rule is left")
	}
	again := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{})
	if again.Pending() {
		t.Errorf("the kept rules make a second run plan something:\n%s", printed(again))
	}
	delete(vm.files, blog)
	if err := siteremove.Execute(forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{})); err != nil {
		t.Fatal(err)
	}
	for _, r := range web {
		if contains(vm.rules, r) {
			t.Errorf("%s is left once Caddy has gone", r)
		}
	}
}

// A failure writing the reduced file over the Caddyfile puts the original
// back from a copy on the host, reloads it, and removes nothing else.
func TestAFailedWriteBackPutsTheOriginalBack(t *testing.T) {
	w := gatewayWorld(t, true)
	vm := w.hosts["vm"]
	original := vm.files[caddyfile]
	containers := len(vm.containers)
	p := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
	w.failOnce = "cat -- '" + root + "/infra/caddy/snippets/.paisans-kept-Caddyfile' > "
	err := siteremove.Execute(p)
	if err == nil || !strings.Contains(err.Error(), "nothing else was removed") {
		t.Fatalf("err = %v", err)
	}
	if vm.files[caddyfile] != original {
		t.Error("the original Caddyfile is not back")
	}
	if n := len(vm.caddyLoads); n == 0 || vm.caddyLoads[n-1] != original {
		t.Error("Caddy was not left on the original")
	}
	if len(vm.containers) != containers {
		t.Error("a container was removed")
	}
	for f := range vm.files {
		if strings.Contains(f, ".paisans-") && strings.HasPrefix(f, root+"/infra/caddy") {
			t.Errorf("%s is left", f)
		}
	}
}

// A kept Caddy on a host whose registry has no entry for the deployment
// gets one, marked kept, so the gate can pass.
func TestAKeptCaddyWithNoEntryGetsOne(t *testing.T) {
	w := gatewayWorld(t, true)
	vm := w.hosts["vm"]
	r := vmRegistry(t, w)
	delete(r.Deployments, ourID)
	vm.files[registry.Path] = encode(t, r)
	if err := siteremove.Execute(forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{})); err != nil {
		t.Fatal(err)
	}
	if e, ok := vmRegistry(t, w).Deployments[ourID]; !ok || e.Kept != registry.KeptCaddy || e.Site != "vm" {
		t.Errorf("the entry: %+v, %v", e, ok)
	}
}

// A site of the owner's that appears after the plan, which removes Caddy,
// stops the run before Caddy goes.
func TestASiteThatAppearsAfterThePlanStopsCaddysRemoval(t *testing.T) {
	w := gatewayWorld(t, false)
	vm := w.hosts["vm"]
	p := forced(t, w, w.cfg, "vm", vmDest(), siteremove.Options{DeleteData: true})
	vm.files[blog] = "blog.example.net {\n}\n"
	if err := siteremove.Execute(p); err == nil || !strings.Contains(err.Error(), render.HostSitesDir) {
		t.Fatalf("err = %v", err)
	}
	found := false
	for _, c := range vm.containers {
		found = found || c.ID == caddyID
	}
	if !found {
		t.Error("Caddy was removed")
	}
	if !vm.under(root + "/infra/caddy/data") {
		t.Error("Caddy's certificates were deleted")
	}
}
