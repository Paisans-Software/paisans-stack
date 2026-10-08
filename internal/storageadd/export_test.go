package storageadd

import (
	"os"
	"testing"
	"time"
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

// Paths the tests read on fake hosts.
const (
	GarageToml = garageToml
	LayoutFile = layoutFile
	CountsFile = countsFile
)

// KeepsImagesOn is whether a plan built with opts passes KeepImages to the
// applies it runs on site.
func KeepsImagesOn(opts Options, site string) bool {
	return (&Plan{opts: opts}).keepImages(site)
}
