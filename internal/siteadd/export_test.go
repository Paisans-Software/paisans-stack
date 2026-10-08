package siteadd

import (
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/preflight"
)

// SetFast makes every wait three polls with no sleep, so a gate that fails
// fails at once and one that passes passes on the first poll it can.
func SetFast() func() {
	saved := []time.Duration{meshWait, meshPoll, learnerWait, learnerPoll, promoteFirstDelay, promoteMaxDelay, etcdWait, etcdPoll, streamWait, streamPoll, lagInterval, syncWait, syncPoll, haproxyWait, haproxyPoll, replicaWait, replicaPoll}
	oldSleep := sleep
	sleep = func(time.Duration) {}
	for _, d := range []*time.Duration{&meshWait, &learnerWait, &etcdWait, &streamWait, &syncWait, &haproxyWait, &replicaWait} {
		*d = 3 * time.Second
	}
	for _, d := range []*time.Duration{&meshPoll, &learnerPoll, &etcdPoll, &streamPoll, &syncPoll, &haproxyPoll, &replicaPoll, &lagInterval, &promoteFirstDelay, &promoteMaxDelay} {
		*d = time.Second
	}
	return func() {
		sleep = oldSleep
		meshWait, meshPoll, learnerWait, learnerPoll, promoteFirstDelay, promoteMaxDelay, etcdWait, etcdPoll, streamWait, streamPoll, lagInterval, syncWait, syncPoll, haproxyWait, haproxyPoll, replicaWait, replicaPoll =
			saved[0], saved[1], saved[2], saved[3], saved[4], saved[5], saved[6], saved[7], saved[8], saved[9], saved[10], saved[11], saved[12], saved[13], saved[14], saved[15], saved[16]
	}
}

// PromoteAttempts is how many times a promotion is tried.
func PromoteAttempts() int { return promoteAttempts }

// PassPreflight replaces stage 1 with a passing report for the rest of the
// test binary's run, and returns a function that restores it.
func PassPreflight() func() {
	saved := runPreflight
	runPreflight = func(*config.Config, string, map[string]apply.Transport) (preflight.Report, error) {
		return preflight.Report{}, nil
	}
	return func() { runPreflight = saved }
}
