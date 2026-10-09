package render_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

const authEnvA = "home-a/srv/paisans/f2a9/auth/.env"
const authEnvB = "home-b/srv/paisans/f2a9/auth/.env"

// renderAuth renders the fixture with the Pocket ID app's settings and smtp
// block replaced, and returns its .env on home-a.
func renderAuth(t *testing.T, settings map[string]any, smtp *config.SMTP, secrets *config.Secrets) string {
	t.Helper()
	cfg := fixture(t)
	app := cfg.Apps["auth"]
	app.Settings = settings
	app.SMTP = smtp
	cfg.Apps["auth"] = app
	if secrets == nil {
		secrets = fixtureSecrets(t)
	}
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return planFiles(plan)[authEnvA]
}

// The admin page is locked on every Pocket ID, settings or none, and a
// setting left out renders nothing, so Pocket ID's own default holds.
func TestPocketIDAlwaysRendersItsConfigurationLocked(t *testing.T) {
	env := renderAuth(t, map[string]any{"file_backend": "database"}, nil, nil)
	if envValue(env, "UI_CONFIG_DISABLED") != "true" {
		t.Errorf("UI_CONFIG_DISABLED is %q", envValue(env, "UI_CONFIG_DISABLED"))
	}
	for _, name := range []string{"APP_NAME", "SESSION_DURATION", "ALLOW_USER_SIGNUPS", "SIGNUP_DEFAULT_USER_GROUP_IDS", "EMAILS_VERIFIED"} {
		if strings.Contains(env, "\n"+name+"=") {
			t.Errorf("%s is rendered although no setting declares it", name)
		}
	}
}

// Each setting renders its variable, as Pocket ID spells the value, and a
// value compose would read specially is quoted so it arrives as written.
func TestPocketIDSettingsRenderTheirVariables(t *testing.T) {
	env := renderAuth(t, map[string]any{
		"file_backend":                   "database",
		"app_name":                       "Example $ID",
		"session_duration":               10080,
		"home_page_url":                  "/settings/apps",
		"allow_user_signups":             "withToken",
		"require_user_email":             true,
		"email_one_time_access_as_admin": true,
		"email_api_key_expiration":       false,
	}, nil, nil)
	for name, want := range map[string]string{
		"APP_NAME":                               `"Example \$ID"`,
		"SESSION_DURATION":                       "10080",
		"HOME_PAGE_URL":                          "/settings/apps",
		"ALLOW_USER_SIGNUPS":                     "withToken",
		"REQUIRE_USER_EMAIL":                     "true",
		"EMAIL_ONE_TIME_ACCESS_AS_ADMIN_ENABLED": "true",
		"EMAIL_API_KEY_EXPIRATION_ENABLED":       "false",
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s=%s, want %s", name, got, want)
		}
	}
}

// Signup groups render as the IDs recorded for them, in the declared order,
// as the JSON array Pocket ID reads. A name with no ID yet is left out and
// named in a comment, and with none resolved the variable is not rendered.
func TestPocketIDSignupGroupsRenderTheirRecordedIDs(t *testing.T) {
	secrets := fixtureSecrets(t)
	secrets.PocketIDGroups = map[string]string{"members": "id-m", "provisional": "id-p"}
	env := renderAuth(t, map[string]any{"signup_default_groups": []any{"provisional", "later", "members"}}, nil, secrets)
	if got := envValue(env, "SIGNUP_DEFAULT_USER_GROUP_IDS"); got != `"[\"id-p\",\"id-m\"]"` {
		t.Errorf("SIGNUP_DEFAULT_USER_GROUP_IDS=%s", got)
	}
	if !strings.Contains(env, "# signup_default_groups names later, with no ID recorded") {
		t.Errorf("the unresolved group is not named:\n%s", env)
	}

	secrets.PocketIDGroups = nil
	env = renderAuth(t, map[string]any{"signup_default_groups": []any{"provisional"}}, nil, secrets)
	if strings.Contains(env, "\nSIGNUP_DEFAULT_USER_GROUP_IDS=") {
		t.Errorf("rendered with no group resolved:\n%s", env)
	}
}

// Pocket ID inherits the deployment's smtp block field by field, takes its
// own password before the shared one, and has no sender name to give.
func TestPocketIDInheritsTheSMTPBlock(t *testing.T) {
	secrets := fixtureSecrets(t)
	secrets.Apps["auth"]["smtp_password"] = "own#pass$word"
	env := renderAuth(t, nil, &config.SMTP{Security: config.SMTPTLS, FromAddress: "id@example.org", FromName: "unused"}, secrets)
	for name, want := range map[string]string{
		"SMTP_HOST":     "smtp.example.org",
		"SMTP_PORT":     "465",
		"SMTP_TLS":      "tls",
		"SMTP_USER":     "robot@example.org",
		"SMTP_PASSWORD": `"own#pass\$word"`,
		"SMTP_FROM":     "id@example.org",
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s=%s, want %s", name, got, want)
		}
	}
	if strings.Contains(env, "unused") {
		t.Error("the sender name was rendered somewhere")
	}

	cfg := fixture(t)
	cfg.SMTP = config.SMTP{}
	plan, err := render.Build(cfg, fixtureSecrets(t))
	if err != nil {
		t.Fatal(err)
	}
	if env := planFiles(plan)[authEnvA]; strings.Contains(env, "\nSMTP_") {
		t.Errorf("SMTP rendered with no host declared:\n%s", env)
	}
}

// Every instance of a Pocket ID on more than one apps site renders the same
// configuration: only the site it names and the HAProxy it connects through
// differ.
func TestEveryPocketIDInstanceRendersTheSameConfiguration(t *testing.T) {
	files := planFiles(build(t))
	strip := func(env string) string {
		var out []string
		for _, line := range strings.Split(env, "\n") {
			if strings.HasPrefix(line, "PAISANS_SITE=") || strings.HasPrefix(line, "DB_CONNECTION_STRING=") {
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	a, b := files[authEnvA], files[authEnvB]
	if a == "" || b == "" {
		t.Fatal("the fixture's Pocket ID does not render on both home sites")
	}
	if strip(a) != strip(b) {
		t.Errorf("the two instances differ beyond the site and the database host:\n%s\n----\n%s", a, b)
	}
}
