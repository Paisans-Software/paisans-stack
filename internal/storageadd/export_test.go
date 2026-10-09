package storageadd

import (
	"os"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// TestMain makes every wait a few polls with no sleep, so a gate that fails
// fails at once and one that passes passes on the first poll it can. Not
// under the garage_integration tag, where the containers need real time.
func TestMain(m *testing.M) {
	if !fastWaits {
		os.Exit(m.Run())
	}
	sleep = func(time.Duration) {}
	for _, d := range []*time.Duration{&connectWait, &layoutWait, &startWait, &probeWait} {
		*d = 3 * time.Second
	}
	for _, d := range []*time.Duration{&connectPoll, &layoutPoll, &startPoll, &probePoll, &syncPoll, &sampleGap} {
		*d = time.Second
	}
	os.Exit(m.Run())
}

// Fixture is the fixture deployment's identity, whose paths the tests read.
var Fixture = deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}

// Paths the tests read on fake hosts.
var (
	GarageToml = garageToml(Fixture)
	LayoutFile = layoutFile(Fixture)
	CountsFile = countsFile(Fixture)
)

// KeepsImagesOn is whether a plan built with opts passes KeepImages to the
// applies it runs on site.
func KeepsImagesOn(opts Options, site string) bool {
	return (&Plan{opts: opts}).keepImages(site)
}

// AppConfigNote is the note a join leaves for an app configuration that is
// out of date.
func AppConfigNote(site string, files int) string { return appConfigNote(site, files) }
