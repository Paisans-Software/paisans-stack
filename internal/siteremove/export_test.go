package siteremove

import (
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

// SetFast makes every wait a few polls with no sleep, so a gate that fails
// fails at once and one that passes passes on the first poll it can.
func SetFast() func() {
	durations := []*time.Duration{&switchWait, &switchPoll, &patroniWait, &patroniPoll, &garageWait, &garagePoll, &garageGap, &etcdWait, &etcdPoll, &haproxyWait, &haproxyPoll, &replicaWait, &replicaPoll}
	saved := make([]time.Duration, len(durations))
	for i, d := range durations {
		saved[i] = *d
	}
	oldSleep := sleep
	sleep = func(time.Duration) {}
	for _, d := range []*time.Duration{&switchWait, &patroniWait, &garageWait, &etcdWait, &haproxyWait, &replicaWait} {
		*d = 10 * time.Second
	}
	for _, d := range []*time.Duration{&switchPoll, &patroniPoll, &garagePoll, &garageGap, &etcdPoll, &haproxyPoll, &replicaPoll} {
		*d = time.Second
	}
	return func() {
		sleep = oldSleep
		for i, d := range durations {
			*d = saved[i]
		}
	}
}

// SetInspect stands a fake host's inventory in for hostcheck.Inspect.
func SetInspect(f func(t apply.Transport, cfg *config.Config) (*hostcheck.Inventory, error)) func() {
	saved := inspect
	inspect = f
	return func() { inspect = saved }
}

// SetNow fixes the clock the deployment record is stamped with.
func SetNow(at time.Time) func() {
	saved := now
	now = func() time.Time { return at }
	return func() { now = saved }
}

// WorstCaseRemains is every line a removal leaves for the operator, built
// with the longest inputs a real deployment gives it: 20-character site
// names, 36-character IDs, and 60-character paths.
func WorstCaseRemains() []string {
	site, name, path, rule := strings.Repeat("s", 20), strings.Repeat("n", 60), "/"+strings.Repeat("p", 59), strings.Repeat("r", 60)
	out := []string{
		containerKept(site, name), networkKept(site, name), volumeKept(site, name), editedFileKept(site, path),
		sshAllowKept(site, rule), rootEditedKept(site, path), dataLeft(site, path, 99999),
		secretsLeft(site), DNSNotModified(deployment.Deployment{ID: strings.Repeat("i", 36)}, config.Destination{Host: "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"}), ownedNote(site, name),
		apply.LeaderPatroniEnvNote(site),
		imageKept(site, "sha256:"+strings.Repeat("f", 64), strings.Repeat("w", 80)),
		recordMissed(site, strings.Repeat("w", 80)),
		recordRaced(site, site, strings.Repeat("w", 80)),
		secretsNotRead(site), pocketIDNotChecked(site),
	}
	out = append(out, caddyKept(site, name, []string{path, path}, path))
	return out
}
