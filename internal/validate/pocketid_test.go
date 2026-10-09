package validate_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// withAuth is valid.yaml with the Pocket ID app's settings, config and smtp
// replaced, so each case below changes exactly one thing.
func withAuth(t *testing.T, settings, cfgKeys map[string]any, smtp *config.SMTP) *config.Config {
	t.Helper()
	cfg := load(t, "valid")
	app := cfg.Apps["auth"]
	app.Settings = map[string]any{"file_backend": "database"}
	for k, v := range settings {
		app.Settings[k] = v
	}
	app.Config = cfgKeys
	app.SMTP = smtp
	cfg.Apps["auth"] = app
	return cfg
}

// findings is every finding for a rule, as "LEVEL key".
func findings(r validate.Result, rule string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Rule == rule {
			out = append(out, f.Level.String()+" "+f.Key)
		}
	}
	return out
}

// A value Pocket ID would refuse at startup is refused here, by the key that
// holds it; a value it accepts is not.
func TestPocketIDSettingsAreCheckedAgainstPocketIDsRules(t *testing.T) {
	good := map[string]any{
		"app_name":                                 "Example ID",
		"session_duration":                         10080,
		"home_page_url":                            "/settings/apps",
		"allow_user_signups":                       "withToken",
		"signup_default_groups":                    []any{"provisional", "members"},
		"require_user_email":                       true,
		"emails_verified":                          false,
		"allow_own_account_edit":                   true,
		"accent_color":                             "#3b82f6",
		"disable_animations":                       true,
		"email_login_notification":                 true,
		"email_one_time_access_as_unauthenticated": true,
		"email_one_time_access_as_admin":           true,
		"email_api_key_expiration":                 true,
		"email_verification":                       true,
		"webauthn_user_verification":               "preferred",
		"webauthn_allow_synced_passkeys":           false,
		"webauthn_authenticator_attachment":        "cross-platform",
	}
	if r := validate.Check(withAuth(t, good, nil, nil)); r.Refused() || r.Has("pocket-id-setting-unknown") {
		t.Fatalf("a configuration Pocket ID accepts was refused or warned about:\n%v", r.Findings)
	}

	for _, tc := range []struct {
		key   string
		value any
	}{
		{"app_name", "X"},
		{"app_name", "A name that is far longer than thirty characters"},
		{"app_name", 42},
		{"session_duration", 0},
		{"session_duration", 43201},
		{"session_duration", "60"},
		{"home_page_url", ""},
		{"allow_user_signups", "withtoken"},
		{"allow_user_signups", true},
		{"require_user_email", "yes"},
		{"email_verification", "true"},
		{"webauthn_user_verification", "discouraged"},
		{"accent_color", "red\nAPP_NAME=x"},
		{"signup_default_groups", "members"},
		{"signup_default_groups", []any{"m"}},
		{"signup_default_groups", []any{" members"}},
		{"signup_default_groups", []any{7}},
	} {
		r := validate.Check(withAuth(t, map[string]any{tc.key: tc.value}, nil, nil))
		got := findings(r, "pocket-id-setting-invalid")
		if len(got) != 1 || got[0] != "REFUSE apps.auth.settings."+tc.key {
			t.Errorf("%s: %#v gave %v", tc.key, tc.value, got)
		}
	}
}

// A setting the kind does not read renders nothing, which for this kind is
// most likely a misspelling, so it is said.
func TestAnUnknownPocketIDSettingIsWarnedAbout(t *testing.T) {
	r := validate.Check(withAuth(t, map[string]any{"sesion_duration": 60}, nil, nil))
	if got := findings(r, "pocket-id-setting-unknown"); len(got) != 1 || got[0] != "WARN apps.auth.settings.sesion_duration" {
		t.Errorf("got %v", got)
	}
}

// A passthrough key for a variable a setting or the smtp block renders is
// refused even when that setting is absent, so the value has one home.
func TestAPocketIDConfigKeyWithASettingIsRefused(t *testing.T) {
	for _, key := range []string{"APP_NAME", "SIGNUP_DEFAULT_USER_GROUP_IDS", "UI_CONFIG_DISABLED", "SMTP_HOST", "SMTP_FROM"} {
		r := validate.Check(withAuth(t, nil, map[string]any{key: "x"}, nil))
		if got := findings(r, "pocket-id-config-key-has-a-setting"); len(got) != 1 || got[0] != "REFUSE apps.auth.config."+key {
			t.Errorf("%s: got %v", key, got)
		}
	}
	// What the toolkit does not model still goes through config.
	r := validate.Check(withAuth(t, nil, map[string]any{"SMTP_SKIP_CERT_VERIFY": "false", "LDAP_ENABLED": "false"}, nil))
	if r.Has("pocket-id-config-key-has-a-setting") {
		t.Errorf("an unmodelled variable was refused: %v", r.Findings)
	}
}

// Pocket ID reads the smtp block now, so an override on it is accepted and
// inherits field by field, and its email toggles need a host and a sender.
func TestPocketIDReadsTheSMTPBlock(t *testing.T) {
	r := validate.Check(withAuth(t, nil, nil, &config.SMTP{FromAddress: "id@example.org"}))
	if r.Has("smtp-on-a-kind-without-mail") || r.Refused() {
		t.Errorf("an smtp override on pocket-id was refused: %v", r.Findings)
	}

	noMail := withAuth(t, map[string]any{"email_verification": true}, nil, nil)
	noMail.SMTP = config.SMTP{}
	r = validate.Check(noMail)
	if got := findings(r, "pocket-id-email-without-smtp"); len(got) != 1 || got[0] != "REFUSE apps.auth.settings.email_verification" {
		t.Errorf("an email toggle without a mail server: %v", got)
	}
	if !strings.Contains(r.Findings[0].Message, "no host and no from_address") {
		t.Errorf("the refusal does not say what is missing: %s", r.Findings[0].Message)
	}

	r = validate.Check(withAuth(t, map[string]any{"email_verification": true}, nil, nil))
	if r.Has("pocket-id-email-without-smtp") {
		t.Errorf("refused although valid.yaml's smtp block has a host and a sender")
	}
}

// SMTP_FROM must be an address alone, or Pocket ID does not start. A host
// with no sender starts and sends with an empty From, which is a warning.
func TestPocketIDsSenderIsAnEmailAddress(t *testing.T) {
	r := validate.Check(withAuth(t, nil, nil, &config.SMTP{FromAddress: "Example <id@example.org>"}))
	if got := findings(r, "pocket-id-smtp-from-not-an-email"); len(got) != 1 || got[0] != "REFUSE apps.auth.smtp.from_address" {
		t.Errorf("a named sender: %v", got)
	}
	cfg := withAuth(t, nil, nil, nil)
	cfg.SMTP.FromAddress = ""
	r = validate.Check(cfg)
	if got := findings(r, "pocket-id-smtp-without-sender"); len(got) != 1 || got[0] != "WARN smtp.from_address" {
		t.Errorf("no sender: %v", got)
	}
}

// Pocket ID sends as its app name. A sender name on its own smtp block is
// not used, and is said; the deployment's, which other kinds use, is not.
func TestASenderNameOnPocketIDIsWarnedAbout(t *testing.T) {
	r := validate.Check(withAuth(t, nil, nil, &config.SMTP{FromName: "Example ID"}))
	if got := findings(r, "pocket-id-smtp-from-name"); len(got) != 1 || got[0] != "WARN apps.auth.smtp.from_name" {
		t.Errorf("got %v", got)
	}
	cfg := withAuth(t, nil, nil, nil)
	cfg.SMTP.FromName = "Example Community"
	if validate.Check(cfg).Has("pocket-id-smtp-from-name") {
		t.Error("the deployment's own from_name was warned about")
	}
}
