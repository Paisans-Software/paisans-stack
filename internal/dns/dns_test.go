package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// testToken is a placeholder, not a credential. It is distinctive so that the
// assertion that it never appears in an error cannot pass by accident.
const testToken = "placeholder-token-7f3a9c-not-a-secret"

// fakeCloudflare is enough of Cloudflare's API v4 to plan and create against.
// No test in this package talks to the real API.
type fakeCloudflare struct {
	mu      sync.Mutex
	zones   map[string]string     // zone name -> id
	records map[string][]cfRecord // zone id -> records
	posts   []cfRecord
	deletes []string // record ids, in order
	lookups []string // zone names asked about, in order
	pages   []string // page numbers asked for by whole zone listings
	nextID  int
	// dropWrites accepts a POST and stores nothing, to prove the read-back.
	dropWrites bool
	// dropDeletes answers a DELETE with success and removes nothing, to
	// prove prune's confirming listing.
	dropDeletes bool
	// echoToken makes every response an error quoting the Authorization
	// header back, the worst thing a provider could do with it.
	echoToken bool
}

func newFake() *fakeCloudflare {
	return &fakeCloudflare{zones: map[string]string{}, records: map[string][]cfRecord{}}
}

func (f *fakeCloudflare) serve(t *testing.T) *cloudflare {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(server.Close)
	c := newCloudflare(testToken)
	c.base = server.URL + "/client/v4"
	return c
}

func writeEnvelope(w http.ResponseWriter, status int, result any, errs ...string) {
	type cfError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	body := map[string]any{"success": len(errs) == 0, "errors": []cfError{}, "result": result}
	for _, e := range errs {
		body["errors"] = append(body["errors"].([]cfError), cfError{Code: 9109, Message: e})
	}
	if list, ok := result.([]any); ok {
		body["result_info"] = map[string]int{"page": 1, "total_pages": 1, "total_count": len(list)}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeCloudflare) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.echoToken {
		writeEnvelope(w, http.StatusForbidden, nil, "Invalid access token: "+r.Header.Get("Authorization"))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		writeEnvelope(w, http.StatusForbidden, nil, "Invalid access token")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	switch {
	case r.Method == http.MethodGet && path == "/zones":
		name := r.URL.Query().Get("name")
		f.lookups = append(f.lookups, name)
		result := []any{}
		if id, ok := f.zones[name]; ok {
			result = append(result, map[string]string{"id": id, "name": name})
		}
		writeEnvelope(w, http.StatusOK, result)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/zones/") && strings.Contains(path, "/dns_records/"):
		zoneID, recordID, _ := strings.Cut(strings.TrimPrefix(path, "/zones/"), "/dns_records/")
		f.deletes = append(f.deletes, recordID)
		kept := f.records[zoneID][:0:0]
		found := false
		for _, rec := range f.records[zoneID] {
			if rec.ID == recordID {
				found = true
				continue
			}
			kept = append(kept, rec)
		}
		if !found {
			writeEnvelope(w, http.StatusNotFound, nil, "Record does not exist.")
			return
		}
		if !f.dropDeletes {
			f.records[zoneID] = kept
		}
		writeEnvelope(w, http.StatusOK, map[string]string{"id": recordID})
	case strings.HasPrefix(path, "/zones/") && strings.HasSuffix(path, "/dns_records"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/zones/"), "/dns_records")
		switch r.Method {
		case http.MethodGet:
			name := r.URL.Query().Get("name.exact")
			if name == "" {
				f.listPage(w, r, id)
				return
			}
			result := []any{}
			for _, rec := range f.records[id] {
				if rec.Name == name {
					result = append(result, rec)
				}
			}
			writeEnvelope(w, http.StatusOK, result)
		case http.MethodPost:
			var rec cfRecord
			if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
				writeEnvelope(w, http.StatusBadRequest, nil, "bad body")
				return
			}
			f.posts = append(f.posts, rec)
			f.nextID++
			rec.ID = fmt.Sprintf("rec-new-%d", f.nextID)
			if !f.dropWrites {
				f.records[id] = append(f.records[id], rec)
			}
			writeEnvelope(w, http.StatusOK, rec)
		}
	default:
		writeEnvelope(w, http.StatusNotFound, nil, "no route")
	}
}

// listPage answers a whole zone listing one page at a time, as Cloudflare's
// page and per_page parameters do.
func (f *fakeCloudflare) listPage(w http.ResponseWriter, r *http.Request, zoneID string) {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	perPage, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = 20
	}
	f.pages = append(f.pages, strconv.Itoa(page))
	all := f.records[zoneID]
	totalPages := (len(all) + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	result := []cfRecord{}
	for i := (page - 1) * perPage; i < len(all) && i < page*perPage; i++ {
		result = append(result, all[i])
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "result": result,
		"result_info": map[string]int{"page": page, "per_page": perPage, "count": len(result), "total_count": len(all), "total_pages": totalPages},
	})
}

// declared is a gateway with a public address, two apps that each serve
// their objects on a derived media hostname, and a site endpoint given as a
// name.
func declared() *config.Config {
	return &config.Config{
		ID:   fixtureID,
		Mesh: config.Mesh{Subnet: "10.44.0.0/24"},
		Sites: map[string]config.Site{
			"home-a": {Roles: []config.Role{config.RoleData, config.RoleApps}, Address: "10.44.0.1"},
			"vm": {
				Roles: []config.Role{config.RoleGateway, config.RoleWitness}, Address: "10.44.0.3",
				Endpoint: "vm.example.org:51820", PublicAddress: "203.0.113.10",
			},
		},
		Apps: map[string]config.App{
			"talk": {Kind: config.KindMbin, Hostname: "talk.example.org"},
			"docs": {Kind: config.KindOutline, Hostname: "Docs.Example.org."},
		},
		Community: config.Community{Domain: "example.org"},
	}
}

func plan(t *testing.T, c *cloudflare, cfg *config.Config) *Plan {
	t.Helper()
	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(context.Background(), c, cfg.Deployment(), wants)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFreshZonePlansCreatesAndExecuteReadsBack(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	c := fake.serve(t)
	p := plan(t, c, declared())

	var out bytes.Buffer
	p.Write(&out)
	t.Logf("dry run:\n%s", out.String())

	if len(p.Entries) != 5 || len(p.Creates()) != 5 {
		t.Fatalf("want 5 creates, got %+v", p.Entries)
	}
	if len(fake.posts) != 0 {
		t.Fatalf("planning wrote %d record(s)", len(fake.posts))
	}
	if err := Execute(context.Background(), c, p); err != nil {
		t.Fatal(err)
	}
	if len(fake.posts) != 5 {
		t.Fatalf("want 5 POSTs, got %d", len(fake.posts))
	}
	for _, rec := range fake.posts {
		if rec.Type != "A" || rec.Content != "203.0.113.10" || rec.Proxied || rec.TTL != 1 || rec.Comment != "paisans-f2a9: created by paisans dns init" {
			t.Errorf("unexpected record body %+v", rec)
		}
	}
	// docs was declared with capitals and a trailing dot, which its derived
	// media hostname inherits, and both sort first.
	if fake.posts[0].Name != "docs-media.example.org" || fake.posts[1].Name != "docs.example.org" {
		t.Errorf("names should be normalised and sorted, first was %q", fake.posts[0].Name)
	}

	// A second run finds everything present and writes nothing.
	again := plan(t, c, declared())
	if len(again.Creates()) != 0 || len(again.Conflicts()) != 0 {
		t.Fatalf("second run should be all present: %+v", again.Entries)
	}
}

func TestExistingMatchingRecordIsPresent(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.records["zone-1"] = []cfRecord{{Type: "A", Name: "talk.example.org", Content: "203.0.113.10"}}
	p := plan(t, fake.serve(t), declared())
	for _, e := range p.Entries {
		want := Create
		if e.Name == "talk.example.org" {
			want = Present
		}
		if e.Action != want {
			t.Errorf("%s: got %s, want %s", e.Name, e.Action, want)
		}
	}
}

func TestConflictRefusesBeforeAnyWrite(t *testing.T) {
	cases := map[string]cfRecord{
		"different address": {Type: "A", Name: "talk.example.org", Content: "198.51.100.7"},
		"cname":             {Type: "CNAME", Name: "talk.example.org", Content: "elsewhere.example.net"},
		"proxied":           {Type: "A", Name: "talk.example.org", Content: "203.0.113.10", Proxied: true},
		"stray aaaa":        {Type: "AAAA", Name: "talk.example.org", Content: "2001:db8::99"},
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFake()
			fake.zones["example.org"] = "zone-1"
			fake.records["zone-1"] = []cfRecord{existing}
			c := fake.serve(t)
			p := plan(t, c, declared())
			if len(p.Conflicts()) != 1 || p.Conflicts()[0].Name != "talk.example.org" {
				t.Fatalf("want one conflict on talk.example.org, got %+v", p.Entries)
			}
			if p.Conflicts()[0].Detail == "" {
				t.Fatal("a conflict must say what is in the way")
			}
			err := Execute(context.Background(), c, p)
			if err == nil {
				t.Fatal("execute should refuse a plan with a conflict")
			}
			if len(fake.posts) != 0 {
				t.Fatalf("a conflict on one record must stop every write, got %d POST(s)", len(fake.posts))
			}
		})
	}
}

func TestZoneWalkPicksTheZoneThatHoldsTheName(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-apex"
	c := fake.serve(t)
	z, err := FindZone(context.Background(), c, "talk.staging.example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	if z.name != "example.org" || z.id != "zone-apex" {
		t.Fatalf("got %+v", z)
	}
	want := []string{"talk.staging.example.org", "staging.example.org", "example.org"}
	if strings.Join(fake.lookups, ",") != strings.Join(want, ",") {
		t.Fatalf("walk should go longest suffix first and stop before the TLD: %v", fake.lookups)
	}

	// A delegated subzone wins over its parent.
	fake.zones["staging.example.org"] = "zone-sub"
	z, err = FindZone(context.Background(), c, "talk.staging.example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	if z.id != "zone-sub" {
		t.Fatalf("a delegated subzone should win, got %+v", z)
	}

	// No zone at all names what was tried, and never asks about the TLD.
	fake.lookups = nil
	_, err = FindZone(context.Background(), c, "talk.example.net", nil)
	if err == nil || !strings.Contains(err.Error(), "example.net") {
		t.Fatalf("want a no-zone error naming what was tried, got %v", err)
	}
	for _, asked := range fake.lookups {
		if asked == "net" {
			t.Fatal("the walk asked about a bare TLD")
		}
	}
}

func TestReadBackCatchesARecordThatDidNotLand(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.dropWrites = true
	c := fake.serve(t)
	err := Execute(context.Background(), c, plan(t, c, declared()))
	if err == nil || !strings.Contains(err.Error(), "does not read back") {
		t.Fatalf("want a read-back failure, got %v", err)
	}
	if len(fake.posts) != 1 {
		t.Fatalf("execute should stop at the first failure, got %d POSTs", len(fake.posts))
	}
}

func TestUnsupportedProviderIsRefusedByName(t *testing.T) {
	_, err := For("desec", testToken)
	if err == nil {
		t.Fatal("desec should be refused")
	}
	for _, want := range []string{"desec record management is not implemented yet", "cloudflare"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q: %v", want, err)
		}
	}
	if _, err := For("cloudflare", " "); err == nil || !strings.Contains(err.Error(), "external.acme_dns_token") {
		t.Errorf("an empty token should name the secret it comes from, got %v", err)
	}
}

func TestTokenNeverAppearsInAnError(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("the token leaked into an error: %v", err)
		}
	}
	t.Run("provider echoes the header back", func(t *testing.T) {
		fake := newFake()
		fake.echoToken = true
		c := fake.serve(t)
		_, err := Build(context.Background(), c, deployment.Deployment{ID: fixtureID}, []Want{{Type: "A", Name: "talk.example.org", Content: "203.0.113.10"}})
		check(t, err)
		if !strings.Contains(err.Error(), "redacted") {
			t.Errorf("expected the echoed token to be redacted: %v", err)
		}
		check(t, c.Create(context.Background(), "zone-1", Record{Type: "A", Name: "x.example.org", Content: "203.0.113.10"}))
	})
	t.Run("transport failure", func(t *testing.T) {
		c := newCloudflare(testToken)
		c.base = "http://127.0.0.1:1/" + testToken // even a URL carrying it
		_, err := c.Records(context.Background(), "zone-1", "talk.example.org")
		check(t, err)
	})
	t.Run("provider constructor", func(t *testing.T) {
		_, err := For("desec", testToken)
		check(t, err)
	})
}

func TestDesiredRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*config.Config)
		want   string
	}{
		"gateway without a public address": {
			mutate: func(c *config.Config) {
				vm := c.Sites["vm"]
				vm.PublicAddress = ""
				c.Sites["vm"] = vm
			},
			want: "sites.vm.public_address is not set",
		},
		"two gateways": {
			mutate: func(c *config.Config) {
				c.Sites["home-b"] = config.Site{Roles: []config.Role{config.RoleGateway}, Address: "10.44.0.2", PublicAddress: "203.0.113.11"}
			},
			want: "2 sites hold the gateway role",
		},
		"named endpoint without a public address": {
			mutate: func(c *config.Config) {
				c.Sites["home-a"] = config.Site{Roles: []config.Role{config.RoleData}, Address: "10.44.0.1", Endpoint: "home-a.example.org:51820"}
			},
			want: "sites.home-a.public_address is not set, but sites.home-a.endpoint names home-a.example.org",
		},
		"one name, two addresses": {
			mutate: func(c *config.Config) {
				c.Sites["home-a"] = config.Site{Roles: []config.Role{config.RoleData}, Address: "10.44.0.1", Endpoint: "talk.example.org:51820", PublicAddress: "203.0.113.12"}
			},
			want: "One name can point at one place",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := declared()
			tc.mutate(cfg)
			_, err := Desired(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDesiredDerivesAndDedupes(t *testing.T) {
	cfg := declared()
	// An endpoint given as an address needs no record.
	cfg.Sites["home-a"] = config.Site{Roles: []config.Role{config.RoleData}, Address: "10.44.0.1", Endpoint: "198.51.100.20:51820"}
	// A role hostname is a public name too, and the same name twice is one record.
	cfg.Apps["chat"] = config.App{Kind: config.KindSynapse, Hostname: "matrix.example.org", Hostnames: map[string]string{"wellknown": "example.org"}}
	vm := cfg.Sites["vm"]
	vm.PublicAddress6 = "2001:db8::10"
	vm.Endpoint = "talk.example.org:51820"
	cfg.Sites["vm"] = vm

	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range wants {
		got = append(got, w.Type+" "+w.Name)
		if w.Name == "talk.example.org" && len(w.Sources) != 2 {
			t.Errorf("talk.example.org should be one record with two sources, got %v", w.Sources)
		}
	}
	want := []string{
		"A docs-media.example.org", "AAAA docs-media.example.org",
		"A docs.example.org", "AAAA docs.example.org",
		"A example.org", "AAAA example.org",
		"A matrix.example.org", "AAAA matrix.example.org",
		"A talk-media.example.org", "AAAA talk-media.example.org",
		"A talk.example.org", "AAAA talk.example.org",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// A monitor serves its own hostname, so its records point at the monitor's
// public address, IPv6 too, while every other app's stay on the gateway.
func TestMonitorAppsPointAtTheMonitor(t *testing.T) {
	cfg := declared()
	cfg.Sites["watch"] = config.Site{Roles: []config.Role{config.RoleMonitor}, Address: "10.44.0.4", PublicAddress: "203.0.113.20", PublicAddress6: "2001:db8::20"}
	cfg.Apps["status"] = config.App{Kind: config.KindUptime, Hostname: "status.example.org", Placement: config.Placement{Mode: config.PlacementPinned, Site: "watch"}}
	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range wants {
		got[w.Type+" "+w.Name] = w.Content
	}
	if got["A status.example.org"] != "203.0.113.20" || got["AAAA status.example.org"] != "2001:db8::20" {
		t.Errorf("the monitor: %v", got)
	}
	if got["A talk.example.org"] != "203.0.113.10" {
		t.Errorf("an ordinary app moved off the gateway: %v", got)
	}
	if _, ok := got["AAAA talk.example.org"]; ok {
		t.Errorf("the gateway has no IPv6 address, yet talk got AAAA: %v", got)
	}

	// The monitor's hostnames need no gateway, and its own address is
	// required.
	delete(cfg.Apps, "talk")
	delete(cfg.Apps, "docs")
	vm := cfg.Sites["vm"]
	vm.Roles = []config.Role{config.RoleWitness}
	cfg.Sites["vm"] = vm
	if _, err := Desired(cfg); err != nil {
		t.Fatalf("a deployment whose only hostname is the monitor's: %v", err)
	}
	watch := cfg.Sites["watch"]
	watch.PublicAddress = ""
	cfg.Sites["watch"] = watch
	if _, err := Desired(cfg); err == nil || !strings.Contains(err.Error(), "sites.watch.public_address is not set") {
		t.Fatalf("a monitor with no address: %v", err)
	}
}
