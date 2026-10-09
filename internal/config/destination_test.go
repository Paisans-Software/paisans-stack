package config_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

func TestParseDestination(t *testing.T) {
	for in, want := range map[string]string{
		"admin@203.0.113.9":      "admin@203.0.113.9:22",
		"admin@203.0.113.9:2222": "admin@203.0.113.9:2222",
		"admin@[2001:db8::1]:22": "admin@[2001:db8::1]:22",
		"admin@host.example.org": "admin@host.example.org:22",
		"admin@[2001:db8::1]":    "admin@[2001:db8::1]:22",
	} {
		d, err := config.ParseDestination(in)
		if err != nil || d.String() != want {
			t.Errorf("%s: got %q, %v; want %q", in, d.String(), err, want)
		}
	}
	for _, bad := range []string{"203.0.113.9", "myalias", "admin@", "admin@h:0", "admin@h:x"} {
		if _, err := config.ParseDestination(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestSiteDestinationUsesTheSSHSection(t *testing.T) {
	s := config.Site{SSH: config.SSH{User: "admin", Host: "203.0.113.9"}}
	if got := s.Destination().String(); got != "admin@203.0.113.9:22" {
		t.Errorf("got %q", got)
	}
}
