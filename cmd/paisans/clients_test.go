package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The recorder is the one way a client's credentials reach the secrets file,
// for `oidc client create` and for apply. Written with no recipient, an
// encrypted file would come back as plaintext.
func TestSecretsRecorderRefusesAnEncryptedFileWithoutARecipient(t *testing.T) {
	path := tempSecrets(t)
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets.Encrypted = true
	rec := &secretsRecorder{app: "talk", path: path, secrets: secrets}
	err = rec.Record("c-1", "not-a-real-secret-0001")
	if err == nil || !strings.Contains(err.Error(), "names a recipient") {
		t.Fatalf("got %v", err)
	}
	if rec.wrote {
		t.Error("the recorder says it wrote")
	}
}
