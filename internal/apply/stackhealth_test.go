package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// psHost answers `ps` with fixed output.
type psHost struct{ out string }

func (h psHost) Run(string) (string, error)              { return h.out, nil }
func (h psHost) RunInput(string, string) (string, error) { return h.out, nil }
func (h psHost) ReadFile(string) (string, bool, error)   { return "", false, nil }
func (h psHost) WriteFile(string, string, uint32) error  { return nil }
func (h psHost) Describe() string                        { return "fake" }

func TestStackHealthyIsOneLook(t *testing.T) {
	if err := apply.StackHealthy("talk", psHost{`{"Service":"web","State":"running","Health":"healthy"}`}); err != nil {
		t.Fatalf("healthy stack: %v", err)
	}
	err := apply.StackHealthy("talk", psHost{`{"Service":"web","State":"running","Health":"starting"}` + "\n" + `{"Service":"worker","State":"exited","ExitCode":1}`})
	if err == nil || !strings.Contains(err.Error(), "worker (exited, exit code 1)") {
		t.Fatalf("got %v", err)
	}
	if err := apply.StackHealthy("talk", psHost{""}); err == nil || !strings.Contains(err.Error(), "no containers") {
		t.Fatalf("an empty stack is not healthy: %v", err)
	}
}
