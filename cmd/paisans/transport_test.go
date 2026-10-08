package main

import (
	"reflect"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// A site's section becomes the transport's user, host, port and keys; --ssh
// replaces all four rather than one of them.
func TestSiteTransportUsesTheSectionOrTheOverride(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	site := cfg.Sites["home-a"]
	got := siteTransport(site, "", true)
	if got.Auth == nil || got.Auth != siteTransport(site, "", true).Auth {
		t.Fatalf("want one shared SudoAuth per site, got %p", got.Auth)
	}
	got.Auth = nil
	keys, _ := site.SSH.Keys()
	want := apply.SSHTransport{User: "ubuntu", Host: "home-a.local", Port: 22, PublicKeys: []string{keys[0].Line}, Sudo: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("section transport:\n got %+v\nwant %+v", got, want)
	}
	if got := siteTransport(site, "admin@jump-alias", false); !reflect.DeepEqual(got, apply.SSHTransport{Destination: "admin@jump-alias"}) {
		t.Fatalf("override transport: %+v", got)
	}
}
