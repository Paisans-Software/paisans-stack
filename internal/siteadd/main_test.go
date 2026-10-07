package siteadd_test

import (
	"os"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/siteadd"
)

// TestMain stands a passing preflight in for stage 1. These tests drive stages
// 2 to 6 against fake hosts that are not shaped for preflight's probes; those
// checks have their own tests in internal/preflight.
func TestMain(m *testing.M) {
	restore := siteadd.PassPreflight()
	code := m.Run()
	restore()
	os.Exit(code)
}
