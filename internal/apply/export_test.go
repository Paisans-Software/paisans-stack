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

// SetHealthWait shortens the health gate's wait the same way SetPrimaryWait
// shortens the wait for a primary. It shares that sleep, so a test setting
// both passes the same sleeper.
func SetHealthWait(wait, poll time.Duration, sleeper func(time.Duration)) func() {
	oldWait, oldPoll, oldSleep := healthWait, healthPoll, sleep
	healthWait, healthPoll, sleep = wait, poll, sleeper
	return func() { healthWait, healthPoll, sleep = oldWait, oldPoll, oldSleep }
}

// ParseContainers exposes the reader of `docker compose ps --format json`.
func ParseContainers(out string) (int, error) {
	list, err := parseContainers(out)
	return len(list), err
}

// SSHCommand exposes the built command and its cleanup, so a test can see
// the key files exist while the command would run and are gone after.
func SSHCommand(t SSHTransport, command string) ([]string, func(), error) {
	cmd, cleanup, err := t.ssh(command)
	if err != nil {
		return nil, nil, err
	}
	return cmd.Args, cleanup, nil
}
