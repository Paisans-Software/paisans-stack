package kinds

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Pocket ID's application configuration, rendered from the app's settings.
//
// The pocket-id kind runs with UI_CONFIG_DISABLED=true (founder decision,
// 2026-10-08). Pocket ID then reads its whole application configuration from
// the environment at startup and ignores what its database holds
// (backend/internal/appconfig/service.go:33-40 and loadDbConfigFromEnv at
// :205-249, tag v2.14.0); a variable left unset takes the default in
// getDefaultConfig (appconfig/model.go:120-175). The admin page that would
// otherwise edit these values is read only and the update API refuses
// (service.go:120-123, :157-159), so the settings below are the one place
// they are changed, by a config edit and an apply that restarts Pocket ID.
//
// Each row is a setting under apps.<app>.settings and the variable it
// renders. Variable names are the struct tags in appconfig/model.go:16-69. A
// setting left out renders nothing, so Pocket ID's own default holds, which is
// why the table carries no defaults of its own.
//
// Values are checked against the rules Pocket ID applies to them at startup,
// which are the admin page's API rules (dto/app_config_dto.go:16-64, run by
// validateEnvConfig at service.go:252-279): a value it would refuse stops
// Pocket ID from starting, so `validate` refuses it first. Where the admin
// page's own form is stricter than its API (an app name of at least 2
// characters, a session of 1 to 43200 minutes, in
// frontend/src/routes/settings/admin/application-configuration/forms/
// app-config-general-form.svelte:40-46), the form's rule is used, because
// those are the values Pocket ID was built to be given.

// PocketIDSettingType is how a setting's value is written in paisans.yaml and
// rendered into the .env.
type PocketIDSettingType int

const (
	// SettingText is a YAML string, rendered as it is.
	SettingText PocketIDSettingType = iota
	// SettingInt is a YAML integer, rendered in decimal.
	SettingInt
	// SettingBool is a YAML boolean, rendered as true or false, the only two
	// spellings Pocket ID accepts (dto/validations.go:137-139).
	SettingBool
	// SettingChoice is a YAML string from a fixed list, spelled as Pocket ID
	// spells it.
	SettingChoice
)

// PocketIDSetting is one row of the table.
type PocketIDSetting struct {
	// Key is the setting under apps.<app>.settings.
	Key string
	// Env is the variable Pocket ID reads.
	Env  string
	Type PocketIDSettingType
	// Min and Max bound an integer's value, or a text's length in characters.
	// Zero Max means unbounded.
	Min, Max int
	// Choices are a SettingChoice's values.
	Choices []string
}

// PocketIDSettings is every setting the kind renders into its .env besides
// signup_default_groups, which is resolved rather than copied (see
// PocketIDSignupGroupsSetting). The order is the order they are rendered in,
// which is model.go's.
var PocketIDSettings = []PocketIDSetting{
	// binding:"required,min=1,max=30" in the API; the form asks for 2.
	{Key: "app_name", Env: "APP_NAME", Type: SettingText, Min: 2, Max: 30},
	// Minutes. integer_string in the API; 1 to 43200 in the form.
	{Key: "session_duration", Env: "SESSION_DURATION", Type: SettingInt, Min: 1, Max: 43200},
	// Where a signed in person lands. The form offers /settings/account
	// (the default) and /settings/apps; the API takes any value.
	{Key: "home_page_url", Env: "HOME_PAGE_URL", Type: SettingText, Min: 1},
	{Key: "emails_verified", Env: "EMAILS_VERIFIED", Type: SettingBool},
	// Any CSS colour, or `default`. The API does not check it; the form
	// checks it in the browser.
	{Key: "accent_color", Env: "ACCENT_COLOR", Type: SettingText, Min: 1},
	{Key: "disable_animations", Env: "DISABLE_ANIMATIONS", Type: SettingBool},
	{Key: "allow_own_account_edit", Env: "ALLOW_OWN_ACCOUNT_EDIT", Type: SettingBool},
	{Key: "allow_user_signups", Env: "ALLOW_USER_SIGNUPS", Type: SettingChoice, Choices: []string{"disabled", "withToken", "open"}},
	{Key: "require_user_email", Env: "REQUIRE_USER_EMAIL", Type: SettingBool},
	// The email toggles. Every one is off by default, and the admin page
	// refuses one turned on without an SMTP host, port and sender, which
	// validate refuses too (pocket-id-email-without-smtp).
	{Key: "email_login_notification", Env: "EMAIL_LOGIN_NOTIFICATION_ENABLED", Type: SettingBool},
	{Key: "email_one_time_access_as_unauthenticated", Env: "EMAIL_ONE_TIME_ACCESS_AS_UNAUTHENTICATED_ENABLED", Type: SettingBool},
	{Key: "email_one_time_access_as_admin", Env: "EMAIL_ONE_TIME_ACCESS_AS_ADMIN_ENABLED", Type: SettingBool},
	{Key: "email_api_key_expiration", Env: "EMAIL_API_KEY_EXPIRATION_ENABLED", Type: SettingBool},
	{Key: "email_verification", Env: "EMAIL_VERIFICATION_ENABLED", Type: SettingBool},
	{Key: "webauthn_user_verification", Env: "WEBAUTHN_USER_VERIFICATION", Type: SettingChoice, Choices: []string{"required", "preferred"}},
	{Key: "webauthn_allow_synced_passkeys", Env: "WEBAUTHN_ALLOW_SYNCED_PASSKEYS", Type: SettingBool},
	{Key: "webauthn_authenticator_attachment", Env: "WEBAUTHN_AUTHENTICATOR_ATTACHMENT", Type: SettingChoice, Choices: []string{"any", "platform", "cross-platform"}},
}

// PocketIDSignupGroupsSetting names the groups every new user is put in, by
// name. Pocket ID takes them as a JSON array of group IDs
// (SIGNUP_DEFAULT_USER_GROUP_IDS, model.go:26, applied in
// service/user_service.go:337-345, :365-401), and an ID exists only once the group does
// on the running instance, so apply resolves each name and records its ID
// before the .env can carry it. Pocket ID applies them to every user created
// without groups of their own, by sign up or by an administrator, and
// silently skips an ID it does not hold.
const (
	PocketIDSignupGroupsSetting = "signup_default_groups"
	PocketIDSignupGroupsEnv     = "SIGNUP_DEFAULT_USER_GROUP_IDS"
)

// PocketIDUIConfigDisabledEnv is the variable that locks the admin page and
// makes the environment the configuration. The kind always renders it true.
const PocketIDUIConfigDisabledEnv = "UI_CONFIG_DISABLED"

// PocketIDSMTPEnv are the SMTP variables the kind renders from the
// deployment's smtp block (kinds.SendsMail). Pocket ID has no sender name of
// its own: it sends as APP_NAME (email/module.go:212).
var PocketIDSMTPEnv = []string{"SMTP_HOST", "SMTP_PORT", "SMTP_TLS", "SMTP_USER", "SMTP_PASSWORD", "SMTP_FROM"}

// PocketIDOtherSettings are the pocket-id settings that are not in the table
// above but are still read: file_backend renders FILE_BACKEND.
var PocketIDOtherSettings = []string{"file_backend", PocketIDSignupGroupsSetting}

// PocketIDSettingByKey finds a row.
func PocketIDSettingByKey(key string) (PocketIDSetting, bool) {
	for _, s := range PocketIDSettings {
		if s.Key == key {
			return s, true
		}
	}
	return PocketIDSetting{}, false
}

// PocketIDEnvOwner is the setting or block that renders a Pocket ID variable,
// for a refusal that sends a passthrough key there instead.
func PocketIDEnvOwner(env string) (string, bool) {
	for _, s := range PocketIDSettings {
		if s.Env == env {
			return "settings." + s.Key, true
		}
	}
	switch env {
	case PocketIDSignupGroupsEnv:
		return "settings." + PocketIDSignupGroupsSetting, true
	case PocketIDUIConfigDisabledEnv:
		return "the kind itself, which always renders it true", true
	}
	for _, e := range PocketIDSMTPEnv {
		if e == env {
			return "the smtp block (the deployment's, or this app's own)", true
		}
	}
	return "", false
}

// PocketIDSettingProblem is one value Pocket ID would refuse.
type PocketIDSettingProblem struct {
	Key     string
	Message string
}

// PocketIDSettingProblems checks every declared setting in the table, and
// signup_default_groups, against Pocket ID's rules. Keys come back sorted.
func PocketIDSettingProblems(settings map[string]any) []PocketIDSettingProblem {
	var out []PocketIDSettingProblem
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := settings[key]
		if key == PocketIDSignupGroupsSetting {
			if msg := signupGroupsProblem(v); msg != "" {
				out = append(out, PocketIDSettingProblem{key, msg})
			}
			continue
		}
		s, ok := PocketIDSettingByKey(key)
		if !ok {
			continue
		}
		if msg := s.problem(v); msg != "" {
			out = append(out, PocketIDSettingProblem{key, msg})
		}
	}
	return out
}

func (s PocketIDSetting) problem(v any) string {
	switch s.Type {
	case SettingBool:
		if _, ok := v.(bool); !ok {
			return fmt.Sprintf("is %v, and %s takes true or false.", v, s.Env)
		}
	case SettingInt:
		n, ok := v.(int)
		if !ok {
			return fmt.Sprintf("is %v, and %s takes a whole number.", v, s.Env)
		}
		if n < s.Min || (s.Max > 0 && n > s.Max) {
			return fmt.Sprintf("is %d. Pocket ID takes %d to %d.", n, s.Min, s.Max)
		}
	case SettingChoice:
		str, ok := v.(string)
		if !ok || !contains(s.Choices, str) {
			return fmt.Sprintf("is %v. Pocket ID takes exactly one of %s, spelled as shown.", v, strings.Join(s.Choices, ", "))
		}
	case SettingText:
		str, ok := v.(string)
		if !ok {
			return fmt.Sprintf("is %v, and %s takes text.", v, s.Env)
		}
		n := utf8.RuneCountInString(str)
		if n < s.Min || (s.Max > 0 && n > s.Max) {
			if s.Max > 0 {
				return fmt.Sprintf("is %d characters long. Pocket ID takes %d to %d.", n, s.Min, s.Max)
			}
			return "is empty. Remove the key to take Pocket ID's default."
		}
		if msg := controlCharacter(str); msg != "" {
			return msg
		}
	}
	return ""
}

// signupGroupsProblem checks signup_default_groups: a list of group names,
// each one the toolkit could create (2 to 50 characters, the friendly name's
// limit in dto/user_group_dto.go:35-39, since a created group's friendly name
// is its name).
func signupGroupsProblem(v any) string {
	list, ok := v.([]any)
	if !ok {
		return fmt.Sprintf("is %v. Give a list of group names, Eg: [members].", v)
	}
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			return fmt.Sprintf("holds %v, which is not a group name.", item)
		}
		if n := utf8.RuneCountInString(name); n < 2 || n > 50 || strings.TrimSpace(name) != name {
			return fmt.Sprintf("holds %q. A group name here is 2 to 50 characters with no surrounding space, the most a group the toolkit creates can have.", name)
		}
		if msg := controlCharacter(name); msg != "" {
			return msg
		}
	}
	return ""
}

func controlCharacter(s string) string {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Sprintf("contains a control character (%U), which a .env cannot carry.", r)
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// PocketIDSignupGroups is the group names signup_default_groups declares, in
// order and without duplicates. A malformed value, which validate refuses,
// yields what can be read of it.
func PocketIDSignupGroups(settings map[string]any) []string {
	list, _ := settings[PocketIDSignupGroupsSetting].([]any)
	var out []string
	for _, item := range list {
		if name, ok := item.(string); ok && name != "" && !contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// PocketIDEnvVar is one rendered variable, its value not yet quoted.
type PocketIDEnvVar struct {
	Name  string
	Value string
}

// PocketIDEnv is the variables the table renders for declared settings, in
// the table's order. A value of a type the setting does not take is rendered
// as Go prints it; validate refuses it before anything renders.
func PocketIDEnv(settings map[string]any) []PocketIDEnvVar {
	var out []PocketIDEnvVar
	for _, s := range PocketIDSettings {
		v, ok := settings[s.Key]
		if !ok || v == nil {
			continue
		}
		var value string
		switch t := v.(type) {
		case bool:
			value = strconv.FormatBool(t)
		case int:
			value = strconv.Itoa(t)
		default:
			value = fmt.Sprint(t)
		}
		out = append(out, PocketIDEnvVar{Name: s.Env, Value: value})
	}
	return out
}

// PocketIDEmailEnabled reports which email toggles are on, by setting key.
func PocketIDEmailEnabled(settings map[string]any) []string {
	var out []string
	for _, s := range PocketIDSettings {
		if !strings.HasPrefix(s.Key, "email_") {
			continue
		}
		if on, _ := settings[s.Key].(bool); on {
			out = append(out, s.Key)
		}
	}
	return out
}
