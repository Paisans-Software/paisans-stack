package kinds_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// The monitor's client returns to the fork's own callback, always sends a
// code challenge, and admits exactly the group named in settings.admin_group:
// that group is both the one whose members become administrators and the one
// a person must be in to sign in at all, so the client is restricted to it at
// Pocket ID and a non-admin is refused before the monitor sees them.
func TestUptimesClientIsRestrictedToItsAdminGroup(t *testing.T) {
	spec, ok := kinds.OIDCClient(config.KindUptime, "status.example.org")
	if !ok {
		t.Fatal("the toolkit does not know an uptime client")
	}
	if spec.CallbackURL != "https://status.example.org/login/oidc/callback" {
		t.Errorf("callback %s", spec.CallbackURL)
	}
	if !spec.PKCE {
		t.Error("the fork always sends a code challenge, so PKCE must be on")
	}
	app := config.App{Kind: config.KindUptime, Hostname: "status.example.org", Settings: map[string]any{"admin_group": "admins"}}
	admin, member := spec.Groups(app)
	if admin != "admins" || member != "admins" {
		t.Errorf("groups admin=%q member=%q, want both admins", admin, member)
	}
	if got := spec.AdminGroupSource("status"); got != "apps.status.settings.admin_group" {
		t.Errorf("admin group source %q", got)
	}
}

// Mbin's groups still come from its config keys, unchanged.
func TestMbinsClientGroupsStillComeFromConfig(t *testing.T) {
	spec, _ := kinds.OIDCClient(config.KindMbin, "talk.example.org")
	app := config.App{Kind: config.KindMbin, Config: map[string]any{"OAUTH_OIDC_ADMIN_GROUP": "mods", "OAUTH_OIDC_MEMBER_GROUP": "members"}}
	admin, member := spec.Groups(app)
	if admin != "mods" || member != "members" {
		t.Errorf("groups admin=%q member=%q", admin, member)
	}
	if got := spec.AdminGroupSource("talk"); got != "apps.talk.config.OAUTH_OIDC_ADMIN_GROUP" {
		t.Errorf("admin group source %q", got)
	}
}
