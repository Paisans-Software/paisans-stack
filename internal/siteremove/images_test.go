package siteremove_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

// An image only this deployment's containers ran goes with them.
func TestCleaningRemovesImagesOnlyOurContainersUsed(t *testing.T) {
	w := setup(t)
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 3, "home-b", "remove", "sha256:talk") {
		t.Fatalf("no image removal planned:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	b := w.hosts["home-b"]
	if b.images["sha256:talk"] {
		t.Error("sha256:talk is still on the host")
	}
	if !b.images["sha256:theirs"] {
		t.Error("the owner's image was removed")
	}
}

// An image a container this deployment does not own also runs from stays.
func TestImagesSharedWithAForeignContainerStay(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	b.containers = append(b.containers, hostcheck.Container{Name: "their-talk", Project: "theirs", Image: "sha256:talk"})
	p := w.mustBuild("home-b", siteremove.Options{})
	if hasStep(p, 3, "home-b", "remove", "sha256:talk") {
		t.Fatalf("a shared image is planned for removal:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if !b.images["sha256:talk"] {
		t.Error("a shared image was removed")
	}
	if !strings.Contains(strings.Join(p.Remains(), "\n"), "image kept") {
		t.Errorf("the report does not say why:\n%s", strings.Join(p.Remains(), "\n"))
	}
}

// Docker refusing an image (a container started in between) is a kept image,
// not a failed removal.
func TestDockerRefusingAnImageIsReportedNotFatal(t *testing.T) {
	w := setup(t)
	p := w.mustBuild("home-b", siteremove.Options{})
	b := w.hosts["home-b"]
	b.containers = append(b.containers, hostcheck.Container{Name: "late", Project: "theirs", Image: "sha256:talk"})
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Remains(), "\n"), "conflict") {
		t.Errorf("Docker's reason is not reported:\n%s", strings.Join(p.Remains(), "\n"))
	}
}
