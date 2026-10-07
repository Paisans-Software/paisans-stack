package siteadd

import "time"

// SetFast makes every wait three polls with no sleep, so a gate that fails
// fails at once and one that passes passes on the first poll it can.
func SetFast() func() {
	saved := []time.Duration{meshWait, meshPoll, learnerWait, learnerPoll, promoteFirstDelay, promoteMaxDelay, etcdWait, etcdPoll, streamWait, streamPoll, lagInterval, syncWait, syncPoll, haproxyWait, haproxyPoll}
	oldSleep := sleep
	sleep = func(time.Duration) {}
	for _, d := range []*time.Duration{&meshWait, &learnerWait, &etcdWait, &streamWait, &syncWait, &haproxyWait} {
		*d = 3 * time.Second
	}
	for _, d := range []*time.Duration{&meshPoll, &learnerPoll, &etcdPoll, &streamPoll, &syncPoll, &haproxyPoll, &lagInterval, &promoteFirstDelay, &promoteMaxDelay} {
		*d = time.Second
	}
	return func() {
		sleep = oldSleep
		meshWait, meshPoll, learnerWait, learnerPoll, promoteFirstDelay, promoteMaxDelay, etcdWait, etcdPoll, streamWait, streamPoll, lagInterval, syncWait, syncPoll, haproxyWait, haproxyPoll =
			saved[0], saved[1], saved[2], saved[3], saved[4], saved[5], saved[6], saved[7], saved[8], saved[9], saved[10], saved[11], saved[12], saved[13], saved[14]
	}
}

// PromoteAttempts is how many times a promotion is tried.
func PromoteAttempts() int { return promoteAttempts }
