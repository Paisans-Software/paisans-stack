package apply

import "time"

// SetPrimaryWait shortens the wait for a Patroni primary and replaces the
// sleep between polls, so a test of the timeout neither sleeps three minutes
// nor races a clock. It returns a function restoring the real values.
func SetPrimaryWait(wait, poll time.Duration, sleeper func(time.Duration)) func() {
	oldWait, oldPoll, oldSleep := primaryWait, primaryPoll, sleep
	primaryWait, primaryPoll, sleep = wait, poll, sleeper
	return func() { primaryWait, primaryPoll, sleep = oldWait, oldPoll, oldSleep }
}

// BootstrapSQL exposes the script, so its shape can be asserted without a
// database.
func BootstrapSQL(b *Bootstrap) string { return bootstrapSQL(b) }
