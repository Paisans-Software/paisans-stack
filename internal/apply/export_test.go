package apply

import (
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// Fixture is the fixture deployment's identity.
var Fixture = deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}

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

// SetStandbyWait shortens the wait for an active Pocket ID the same way, and
// shares the same sleep.
func SetStandbyWait(wait, poll time.Duration, sleeper func(time.Duration)) func() {
	oldWait, oldPoll, oldSleep := standbyWait, standbyPoll, sleep
	standbyWait, standbyPoll, sleep = wait, poll, sleeper
	return func() { standbyWait, standbyPoll, sleep = oldWait, oldPoll, oldSleep }
}

// InstanceCommand exposes what a site is asked.
func InstanceCommand(d deployment.Deployment, app, address string, port int) string {
	return instanceCommand(d, app, address, port)
}

// FakeSSH stands in for the ssh binary: each call gets the command's
// arguments and its stdin, and says what ssh printed and how it exited. It
// also replaces the retry delays and the sleep between them, and captures the
// retry log. It returns a function restoring the real ones.
func FakeSSH(fake func(args []string, stdin string) (out string, exit int), delays []time.Duration, sleeper func(time.Duration), log io.Writer) func() {
	oldRun, oldDelays, oldSleep, oldLog := runSSH, sshRetryDelays, sleep, retryLog
	runSSH = func(cmd *exec.Cmd) error {
		var stdin string
		if cmd.Stdin != nil {
			b, _ := io.ReadAll(cmd.Stdin)
			stdin = string(b)
		}
		out, exit := fake(cmd.Args, stdin)
		io.WriteString(cmd.Stdout, out)
		if exit != 0 {
			return exitStatus(exit)
		}
		return nil
	}
	sshRetryDelays, sleep, retryLog = delays, sleeper, log
	return func() { runSSH, sshRetryDelays, sleep, retryLog = oldRun, oldDelays, oldSleep, oldLog }
}

// exitStatus is an error carrying an exit status, as *exec.ExitError does.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// SetNow replaces the clock a left over mark is stamped with, and returns a
// function restoring it.
func SetNow(at func() time.Time) func() {
	old := now
	now = at
	return func() { now = old }
}
