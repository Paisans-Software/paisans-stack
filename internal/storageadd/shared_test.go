package storageadd_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/storageadd"
)

// A site the host check found shared keeps its images through every apply
// storage add runs there, as apply itself does on that site.
func TestASharedSiteKeepsItsImages(t *testing.T) {
	opts := storageadd.Options{SharedSites: map[string]bool{"home-b": true}}
	if !storageadd.KeepsImagesOn(opts, "home-b") || storageadd.KeepsImagesOn(opts, "home-a") {
		t.Error("only the shared site keeps its images")
	}
}
