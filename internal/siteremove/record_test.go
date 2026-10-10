package siteremove_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func recordListing(sites ...string) string {
	return `{"version":1,"sites":["` + strings.Join(sites, `","`) + `"],"apps":[],"pocket_id_groups":[]}` + "\n"
}

// The remaining gateway's record no longer lists the removed site.
func TestSiteRemoveForgetsTheSiteOnEveryGateway(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[deployrecord.Path(dep)] = recordListing("home-a", "home-b", "vm", "watch")
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 2, "vm", "forget", "home-b") {
		t.Fatalf("no forget planned:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(vm.files[deployrecord.Path(dep)], "home-b") {
		t.Error("vm's record still lists home-b")
	}
}

// Cleaning a gateway's host deletes its record with the rest.
func TestCleaningAGatewayDeletesItsRecord(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[deployrecord.Path(dep)] = recordListing("vm")
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p, err := siteremove.BuildForced(w.cfg.WithoutSite("vm"), w.secrets, "vm", dest, vm, siteremove.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasStepIn(stageNamed(p, "clean the host"), "vm", "delete", deployrecord.Path(dep)) {
		t.Fatalf("no record deletion:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := vm.files[deployrecord.Path(dep)]; ok {
		t.Error("the record is still on the host")
	}
}
