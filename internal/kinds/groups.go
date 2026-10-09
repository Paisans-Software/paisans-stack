package kinds

import (
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The two identity provider groups every member facing app's client is
// restricted to when its declaration names none. Both are the toolkit's
// defaults rather than fixed names: an app's settings may replace either, and
// `validate` holds a member gated app's member group to the gate's.
const (
	// MembersGroup is the group a person must be in to sign in to a member
	// facing app, and the one the gate's members instance admits.
	MembersGroup = "members"
	// AdminsGroup is the group whose members an app that maps administrators
	// makes administrators, and that every restricted client admits beside the
	// member group. It is the group the admin reconciler keeps Pocket ID's
	// administrators in (adminreconciler.Group), so that an app's
	// administrators are the ones somebody is watching.
	AdminsGroup = "admins"
)

// GateMembersGroupSetting is the gate app's setting naming the group its
// members instance admits (OAUTH2_PROXY_ALLOWED_GROUPS in
// templates/oauth2-proxy/compose.yaml.tmpl).
const GateMembersGroupSetting = "members_group"

// GateMembersGroup is the group a gate app's members instance admits: its
// members_group setting, or MembersGroup.
func GateMembersGroup(gate config.App) string {
	return groupSetting(gate.Settings, GateMembersGroupSetting, MembersGroup)
}

// groupSetting reads a group name from settings, taking fallback when the
// key is absent, blank or not a string. A blank name is treated as absent so
// that a typo cannot leave a client unrestricted; validate refuses the typo
// itself (oidc-member-group-not-a-name).
func groupSetting(settings map[string]any, key, fallback string) string {
	if s, ok := settings[key].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return fallback
}
