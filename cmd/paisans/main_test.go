package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// garage-key-is-malformed has to stop `render`, not only `init`: `render` and
// `apply` both call config.LoadSecrets directly and never call
// secretsgen.Fill, so a key hand edited into the secrets file after the last
// `init` would otherwise reach the rendered artifacts, and from there a host,
// with nothing ever having looked at it. This test proves the wiring, not
// just the check: it drives runRender exactly as the CLI would, over a
// fixture where the configuration is otherwise fine and only the secret is
// broken.
func TestRunRenderRefusesAMalformedGarageKey(t *testing.T) {
	out := t.TempDir()
	err := runRender([]string{
		"--config", filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"),
		"--secrets", filepath.Join("testdata", "garage-key-is-malformed-secrets.yaml"),
		"--out", out,
	})
	if err == nil {
		t.Fatal("expected render to refuse a configuration with a malformed Garage key")
	}
	if !strings.Contains(err.Error(), "garage-key-is-malformed") {
		t.Fatalf("the error should name the rule garage-key-is-malformed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docs.s3_access_key_id") {
		t.Fatalf("the error should name the offending field, got: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("render wrote %d file(s) to --out before refusing the key", len(entries))
	}
}
