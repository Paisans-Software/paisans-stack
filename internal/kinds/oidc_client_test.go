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

// Mbin's groups are settings with the toolkit's defaults: a declaration that
// names neither gets a client restricted to members and admins, and the fork
// reads the same two names, so a member-only Mbin needs nothing declared.
func TestMbinsClientIsRestrictedToMembersAndAdminsByDefault(t *testing.T) {
	spec, _ := kinds.OIDCClient(config.KindMbin, "talk.example.org")
	admin, member := spec.Groups(config.App{Kind: config.KindMbin})
	if admin != kinds.AdminsGroup || member != kinds.MembersGroup {
		t.Errorf("groups admin=%q member=%q, want %s and %s", admin, member, kinds.AdminsGroup, kinds.MembersGroup)
	}
	if !spec.ReadsGroups {
		t.Error("the fork reads the groups claim, so the spec must say so")
	}
	chosen := config.App{Kind: config.KindMbin, Settings: map[string]any{"admin_group": "mods", "member_group": "crew"}}
	admin, member = spec.Groups(chosen)
	if admin != "mods" || member != "crew" {
		t.Errorf("groups admin=%q member=%q", admin, member)
	}
	if got := spec.AdminGroupSource("talk"); got != "apps.talk.settings.admin_group" {
		t.Errorf("admin group source %q", got)
	}
	// A blank or non-string value is as good as absent: the default holds
	// rather than an unrestricted client slipping through on a typo.
	blank := config.App{Kind: config.KindMbin, Settings: map[string]any{"admin_group": "  ", "member_group": false}}
	admin, member = spec.Groups(blank)
	if admin != kinds.AdminsGroup || member != kinds.MembersGroup {
		t.Errorf("blank settings gave admin=%q member=%q", admin, member)
	}
}

// Outline and WriteFreely read no groups claim, so their clients are
// restricted at Pocket ID to the member group and admins, and nothing else:
// the restriction is the whole control. Neither sends a code challenge in the
// configuration the toolkit renders, and a Pocket ID client with PKCE on
// refuses a request without one, so PKCE is off.
func TestOutlineAndWriteFreelyClientsAreRestrictedAndSendNoChallenge(t *testing.T) {
	cases := []struct {
		kind     config.Kind
		callback string
	}{
		{config.KindOutline, "https://docs.example.org/auth/oidc.callback"},
		{config.KindWriteFreely, "https://docs.example.org/oauth/callback/generic"},
	}
	for _, tc := range cases {
		spec, ok := kinds.OIDCClient(tc.kind, "docs.example.org")
		if !ok {
			t.Errorf("the toolkit does not know a %s client", tc.kind)
			continue
		}
		if spec.CallbackURL != tc.callback {
			t.Errorf("%s callback %s, want %s", tc.kind, spec.CallbackURL, tc.callback)
		}
		if spec.PKCE {
			t.Errorf("%s: PKCE on, but the app sends no code challenge", tc.kind)
		}
		if spec.ReadsGroups {
			t.Errorf("%s: says it reads the groups claim, and it does not", tc.kind)
		}
		if spec.LaunchURL != "https://docs.example.org" {
			t.Errorf("%s launch URL %s", tc.kind, spec.LaunchURL)
		}
		admin, member := spec.Groups(config.App{Kind: tc.kind})
		if admin != kinds.AdminsGroup || member != kinds.MembersGroup {
			t.Errorf("%s groups admin=%q member=%q", tc.kind, admin, member)
		}
		_, member = spec.Groups(config.App{Kind: tc.kind, Settings: map[string]any{"member_group": "crew"}})
		if member != "crew" {
			t.Errorf("%s member group %q with member_group set", tc.kind, member)
		}
	}
}

// Every kind that can sit behind the gate and has a client gets a restricted
// one from an empty declaration. An unrestricted client on a member facing
// app would admit anyone with a Pocket ID account.
func TestEveryGateableClientIsRestrictedByDefault(t *testing.T) {
	for _, kind := range []config.Kind{config.KindMbin, config.KindOutline, config.KindWriteFreely, config.KindElement, config.KindUptime} {
		spec, ok := kinds.OIDCClient(kind, "app.example.org")
		if !ok || !kinds.GateFor(kind).Gateable {
			continue
		}
		app := config.App{Kind: kind}
		if kind == config.KindUptime {
			app.Settings = map[string]any{"admin_group": "admins"}
		}
		if _, member := spec.Groups(app); member == "" {
			t.Errorf("%s: an empty declaration gives an unrestricted client", kind)
		}
	}
}

// The gate's members group is the one setting every member gated app's
// member group has to agree with, with the same default.
func TestGateMembersGroupDefaultsToMembers(t *testing.T) {
	if got := kinds.GateMembersGroup(config.App{Kind: config.KindOAuth2Proxy}); got != kinds.MembersGroup {
		t.Errorf("default %q", got)
	}
	if got := kinds.GateMembersGroup(config.App{Kind: config.KindOAuth2Proxy, Settings: map[string]any{"members_group": "crew"}}); got != "crew" {
		t.Errorf("declared %q", got)
	}
}
