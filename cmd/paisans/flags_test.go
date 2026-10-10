package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

func TestVerboseFlagShortAndLong(t *testing.T) {
	for _, args := range [][]string{{"-v"}, {"--verbose"}} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		reporter := commonFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if !reporter().Verbose() {
			t.Errorf("%v did not turn on verbose", args)
		}
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	reporter := commonFlags(fs)
	_ = fs.Parse(nil)
	if reporter().Verbose() {
		t.Error("verbose by default")
	}
}

// Every command accepts the flag, so an operator never has to remember
// which do. Each run* registers commonFlags; this reads the source so a new
// command cannot forget it.
func TestEveryCommandAcceptsVerbose(t *testing.T) {
	assertEveryFlagSetHasCommonFlags(t)
}

func assertEveryFlagSetHasCommonFlags(t *testing.T) {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		sets := strings.Count(string(src), "flag.NewFlagSet(")
		common := strings.Count(string(src), "commonFlags(")
		if f == "flags.go" {
			continue
		}
		if common < sets {
			t.Errorf("%s: %d FlagSet(s) but %d commonFlags call(s)", f, sets, common)
		}
	}
}

// withRecorder makes every command run in this test report to a recorder,
// so the test asserts on steps and items rather than on how they are drawn.
func withRecorder(t *testing.T, verbose bool) *ui.Recorder {
	t.Helper()
	rec := &ui.Recorder{Verbose_: verbose}
	reporterOverride = rec
	t.Cleanup(func() { reporterOverride = nil; apply.SetRetryLog(nil) })
	return rec
}

// retryOnce makes one ssh retry happen: an ssh on PATH that fails to connect
// the first time it is run and succeeds the second.
func retryOnce(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = -G ] && exit 0\nif [ ! -e '" + filepath.Join(dir, "seen") + "' ]; then : > '" + filepath.Join(dir, "seen") + "'; echo 'ssh: connect to host x port 22: Connection refused'; exit 255; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := (apply.SSHTransport{Site: "home-a", Destination: "ubuntu@x"}).Run("true"); err != nil {
		t.Fatal(err)
	}
}

// A retry is shown as a detail when verbose and not at all otherwise.
func TestRetryLinesFollowVerbosity(t *testing.T) {
	t.Cleanup(func() { apply.SetRetryLog(nil) })
	for _, verbose := range []bool{true, false} {
		var b strings.Builder
		reporterOverride = ui.NewPlain(&b, verbose)
		t.Cleanup(func() { reporterOverride = nil })
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		reporter := commonFlags(fs)
		_ = fs.Parse(nil)
		reporter()
		retryOnce(t)
		if got := strings.Contains(b.String(), "ssh could not connect"); got != verbose {
			t.Errorf("verbose %t: retry shown %t\n%s", verbose, got, b.String())
		}
	}
}

// A command whose stdout is data sends its retries to stderr with the rest
// of its report, so -v never writes a line beside the data.
func TestRetryLinesStayOffAStdoutThatCarriesData(t *testing.T) {
	t.Cleanup(func() { apply.SetRetryLog(nil) })
	var err error
	stdout, stderr := captureOutput(t, func() {
		err = runIngress([]string{"show", "-v", "--app", "status", "--config", fixtureConfig()})
		retryOnce(t)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "ssh could not connect") || !strings.Contains(stdout, "nothing to hand off") {
		t.Errorf("stdout is not the sheet alone:\n%s", stdout)
	}
	if !strings.Contains(stderr, "ssh could not connect") {
		t.Errorf("the retry did not reach stderr:\n%s", stderr)
	}
}
