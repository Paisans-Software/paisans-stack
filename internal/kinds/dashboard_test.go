package kinds_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Mbin's tile starts the fork's OIDC flow; every other kind opens its front
// page, which is the bare host with no trailing slash.
func TestLaunchURLDefaultsPerKind(t *testing.T) {
	for _, kind := range config.Kinds() {
		want := "https://app.example.org"
		if kind == config.KindMbin {
			want = "https://app.example.org/oauth/oidc/connect"
		}
		if got := kinds.LaunchURL(kind, "app.example.org", ""); got != want {
			t.Errorf("%s: LaunchURL = %q, want %q", kind, got, want)
		}
	}
}

func TestADashboardLinkReplacesTheKindsPath(t *testing.T) {
	cases := []struct {
		kind config.Kind
		link string
		want string
	}{
		{config.KindMbin, "/magazines", "https://app.example.org/magazines"},
		{config.KindMbin, "/", "https://app.example.org"},
		{config.KindOutline, "/home", "https://app.example.org/home"},
	}
	for _, tc := range cases {
		if got := kinds.LaunchURL(tc.kind, "app.example.org", tc.link); got != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.kind, tc.link, got, tc.want)
		}
	}
}

// The bare host is in every kind's list, because it is what every client made
// before kinds named a path was given.
func TestToolkitLaunchURLs(t *testing.T) {
	got := fmt.Sprint(kinds.ToolkitLaunchURLs(config.KindMbin, "app.example.org"))
	if got != "[https://app.example.org https://app.example.org/oauth/oidc/connect]" {
		t.Errorf("mbin: %s", got)
	}
	got = fmt.Sprint(kinds.ToolkitLaunchURLs(config.KindOutline, "app.example.org"))
	if got != "[https://app.example.org]" {
		t.Errorf("outline: %s", got)
	}
}

func TestCheckDashboardLink(t *testing.T) {
	for _, ok := range []string{"/", "/oauth/oidc/connect", "/login?next=%2Fm"} {
		if err := kinds.CheckDashboardLink(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	refused := map[any]string{
		"https://app.example.org/x": "the host is always the app's own hostname",
		"oauth/oidc/connect":        "not a path",
		"":                          "not a path",
		"//other.example.org/x":     "reads as a host",
		"/a b":                      "whitespace",
		42:                          "not a string",
	}
	for v, want := range refused {
		err := kinds.CheckDashboardLink(v)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want it refused saying %q", v, err, want)
		}
	}
}

// An Mbin app that chooses no queue backend gets Postgres, and an empty value
// is the same as none.
func TestMbinQueueDefaultsToPostgres(t *testing.T) {
	for _, settings := range []map[string]any{nil, {}, {"queue": ""}} {
		if got := kinds.MbinQueue(settings); got != kinds.MbinQueuePostgres {
			t.Errorf("MbinQueue(%v) = %q, want %q", settings, got, kinds.MbinQueuePostgres)
		}
	}
	if got := kinds.MbinQueue(map[string]any{"queue": "rabbitmq"}); got != kinds.MbinQueueRabbitMQ {
		t.Errorf("MbinQueue(rabbitmq) = %q", got)
	}
}
