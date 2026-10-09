package siteremove_test

import (
	"regexp"
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

// The report points at `secrets prune` for the removed site's secrets.
func TestTheReportPointsAtSecretsPrune(t *testing.T) {
	w := setup(t)
	p := w.mustBuild("home-b", siteremove.Options{})
	if !strings.Contains(strings.Join(p.Remains(), "\n"), "paisans secrets prune") {
		t.Errorf("no pointer at secrets prune:\n%s", strings.Join(p.Remains(), "\n"))
	}
}

// A run stopped after the containers went, before their images did, still
// finds the images on the next run: the compose files the manifest proves are
// this deployment's name them.
func TestImagesAreFoundAfterTheContainersWent(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	var kept []hostcheck.Container
	for _, c := range b.containers {
		if c.Deployment != ourID {
			kept = append(kept, c)
		}
	}
	b.containers = kept
	compose := b.files[root+"/talk/compose.yaml"]
	ref := regexp.MustCompile(`(?m)^    image: (\S+)`).FindStringSubmatch(compose)
	if ref == nil {
		t.Fatalf("no image in talk's compose.yaml:\n%s", compose)
	}
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 3, "home-b", "remove", "sha256:"+hexSum(ref[1])) {
		t.Fatalf("%s is not planned for removal:\n%s", ref[1], printed(p))
	}
}
