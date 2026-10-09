package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	t.Cleanup(func() { reporterOverride = nil })
	return rec
}
