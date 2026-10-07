package storageadd

import (
	"os"
	"testing"
	"time"
)

// TestMain makes every wait a few polls with no sleep, so a gate that fails
// fails at once and one that passes passes on the first poll it can.
func TestMain(m *testing.M) {
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
