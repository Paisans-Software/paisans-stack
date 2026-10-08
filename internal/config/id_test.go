package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
)

const fixtureID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"

// withID returns minimal with its id line replaced by line, or removed when
// line is empty.
func withID(line string) string {
	body := strings.Replace(minimal, "id: "+fixtureID+"\n", "", 1)
	if line == "" {
		return body
	}
	return strings.Replace(body, "version: 1\n", "version: 1\n"+line+"\n", 1)
}

func TestLoadRefusesAMissingOrMalformedID(t *testing.T) {
	for _, tc := range []struct{ name, line, want string }{
		{"missing", "", "id: required"},
		{"not a uuid", "id: talk", "not a lowercase version 4 UUID"},
		{"uppercase", "id: F2A9C4E1-0B7D-4C3A-9E2F-5A6B7C8D9E01", "not a lowercase version 4 UUID"},
		{"version 1", "id: f2a9c4e1-0b7d-1c3a-9e2f-5a6b7c8d9e01", "not a lowercase version 4 UUID"},
		{"no hyphens", "id: f2a9c4e10b7d4c3a9e2f5a6b7c8d9e01", "not a lowercase version 4 UUID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(write(t, withID(tc.line)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load: %v, want an error containing %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "paisans init") {
				t.Errorf("the refusal does not say that `paisans init` writes an id: %v", err)
			}
		})
	}
	if _, err := config.Load(write(t, minimal)); err != nil {
		t.Fatalf("the fixture id was refused: %v", err)
	}
}

func TestEnsureIDWritesAVersion4IDAndKeepsEveryOtherByte(t *testing.T) {
	body := "# a comment the operator wrote\n\n" + withID("") + "\n# a trailing comment\n"
	path := write(t, body)
	id, added, err := config.EnsureID(path)
	if err != nil {
		t.Fatal(err)
	}
	if !added || !deployment.ValidID(id) {
		t.Fatalf("EnsureID = %q, %v, want a new version 4 id", id, added)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(body, "version: 1\n", "version: 1\nid: "+id+"\n", 1)
	if string(got) != want {
		t.Errorf("the file became\n%s\nwant\n%s", got, want)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the file EnsureID wrote does not load: %v", err)
	}
	if cfg.ID != id {
		t.Errorf("loaded id %q, want %q", cfg.ID, id)
	}

	// A second run is a no op.
	again, added, err := config.EnsureID(path)
	if err != nil || added || again != id {
		t.Errorf("second EnsureID = %q, %v, %v; want %q unchanged", again, added, err, id)
	}
}

func TestEnsureIDNeverReplacesAnID(t *testing.T) {
	for _, line := range []string{"id: " + fixtureID, "id: not-a-uuid"} {
		body := withID(line)
		path := write(t, body)
		id, added, err := config.EnsureID(path)
		if err != nil {
			t.Fatal(err)
		}
		if added || "id: "+id != line {
			t.Errorf("EnsureID on %q = %q, %v; want it returned as it is", line, id, added)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("EnsureID rewrote a file that had an id:\n%s", got)
		}
	}
}
