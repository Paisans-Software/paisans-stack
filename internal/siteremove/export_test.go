package siteremove

import (
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

// SetFast makes every wait a few polls with no sleep, so a gate that fails
// fails at once and one that passes passes on the first poll it can.
func SetFast() func() {
	durations := []*time.Duration{&switchWait, &switchPoll, &patroniWait, &patroniPoll, &garageWait, &garagePoll, &garageGap, &etcdWait, &etcdPoll, &haproxyWait, &haproxyPoll, &handoverWait, &handoverPoll}
	saved := make([]time.Duration, len(durations))
	for i, d := range durations {
		saved[i] = *d
	}
	oldSleep := sleep
	sleep = func(time.Duration) {}
	for _, d := range []*time.Duration{&switchWait, &patroniWait, &garageWait, &etcdWait, &haproxyWait, &handoverWait} {
		*d = 10 * time.Second
	}
	for _, d := range []*time.Duration{&switchPoll, &patroniPoll, &garagePoll, &garageGap, &etcdPoll, &haproxyPoll, &handoverPoll} {
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

// SetNow fixes the clock the hand over marker records.
func SetNow(at time.Time) func() {
	saved := now
	now = func() time.Time { return at }
	return func() { now = saved }
}
