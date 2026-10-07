package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A rotation names its app; there is no "every app" form.
func TestRunStorageRotateKeyRequiresAnApp(t *testing.T) {
	err := runStorageRotateKey(nil)
	if err == nil || !strings.Contains(err.Error(), "--app is required") {
		t.Fatalf("want --app to be required, got %v", err)
	}
}

// A malformed key, current or previous, is refused before any host is
// reached: the rotation deletes a Garage key by the previous ID.
func TestRunStorageRotateKeyRefusesAMalformedKeyBeforeReachingAHost(t *testing.T) {
	err := runStorageRotateKey([]string{
		"--config", filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"),
		"--secrets", filepath.Join("testdata", "garage-key-is-malformed-secrets.yaml"),
		"--app", "talk",
	})
	if err == nil || !strings.Contains(err.Error(), "garage-key-is-malformed") {
		t.Fatalf("want garage-key-is-malformed, got %v", err)
	}
}
