package validate

import (
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// The pocket-id kind's application configuration is rendered from its
// settings and the smtp block, with UI_CONFIG_DISABLED=true (see
// kinds.PocketIDSettings). Pocket ID validates that configuration when it
// starts and refuses to start on a value it does not accept, so every rule
// here refuses what would otherwise be found as a crash looping container
// after an apply, on every apps site at once.

// pocketIDApps is every pocket-id app, in AppNames order.
func (c *checker) pocketIDApps() []string {
	var out []string
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Kind == config.KindPocketID {
			out = append(out, name)
		}
	}
	return out
}

// pocketIDSettingInvalid refuses a setting whose value Pocket ID would refuse
// at startup, or one the admin page's form would not have let anybody save.
func (c *checker) pocketIDSettingInvalid() {
	for _, name := range c.pocketIDApps() {
		for _, p := range kinds.PocketIDSettingProblems(c.cfg.Apps[name].Settings) {
			c.refuse("pocket-id-setting-invalid", fmt.Sprintf("apps.%s.settings.%s", name, p.Key),
				"%s Pocket ID reads its whole configuration from the rendered .env and will not start on a value it refuses.", p.Message)
		}
	}
}

// pocketIDSettingUnknown warns about a pocket-id setting the toolkit does not
// read. A setting no template asks for renders nothing, which for this kind
// most likely means a misspelt Pocket ID option that its admin page can no
// longer set either.
func (c *checker) pocketIDSettingUnknown() {
	for _, name := range c.pocketIDApps() {
		for _, key := range sortedKeys(c.cfg.Apps[name].Settings) {
			if _, ok := kinds.PocketIDSettingByKey(key); ok || slices.Contains(kinds.PocketIDOtherSettings, key) {
				continue
			}
			c.warn("pocket-id-setting-unknown", fmt.Sprintf("apps.%s.settings.%s", name, key),
				"is not a setting the pocket-id kind reads, so it renders nothing. The settings are listed in README.md, \"Pocket ID's configuration is rendered from paisans.yaml\"; an option the toolkit does not model goes under config, by its variable name.")
		}
	}
}

// pocketIDConfigKeyHasASetting refuses a passthrough key for a variable the
// kind renders from a setting or from the smtp block, whether or not that
// setting is declared. Rendering refuses a key the .env already holds; this
// also refuses one whose setting is absent today, because the day somebody
// declares the setting the two would collide, and until then the value has
// two places it could be changed.
func (c *checker) pocketIDConfigKeyHasASetting() {
	for _, name := range c.pocketIDApps() {
		for _, key := range sortedKeys(c.cfg.Apps[name].Config) {
			owner, ok := kinds.PocketIDEnvOwner(key)
			if !ok {
				continue
			}
			c.refuse("pocket-id-config-key-has-a-setting", fmt.Sprintf("apps.%s.config.%s", name, key),
				"is rendered by %s. Set it there and remove it from config, so the value has one place to be changed.", owner)
		}
	}
}

// pocketIDEmailWithoutSMTP refuses an email toggle turned on without the
// SMTP host and sender it needs. Pocket ID's admin page refuses the same
// combination (app-config-email-form.svelte:53-88 at tag v2.14.0); at startup
// it is not checked, and every mail the toggle promises then fails.
func (c *checker) pocketIDEmailWithoutSMTP() {
	for _, name := range c.pocketIDApps() {
		on := kinds.PocketIDEmailEnabled(c.cfg.Apps[name].Settings)
		if len(on) == 0 {
			continue
		}
		smtp := c.cfg.SMTPFor(name)
		var missing []string
		if smtp.Host == "" {
			missing = append(missing, "host")
		}
		if smtp.FromAddress == "" {
			missing = append(missing, "from_address")
		}
		if len(missing) == 0 {
			continue
		}
		c.refuse("pocket-id-email-without-smtp", fmt.Sprintf("apps.%s.settings.%s", name, on[0]),
			"turns on mail from Pocket ID (%s), but its smtp settings resolve no %s. Declare them in the top level smtp block or in apps.%s.smtp, or turn the mail off.",
			strings.Join(on, ", "), strings.Join(missing, " and no "), name)
	}
}

// pocketIDSMTPSender checks the sender Pocket ID is given. SMTP_FROM must be
// an email address (binding:"omitempty,email", dto/app_config_dto.go:30), so
// anything else stops it from starting. A host with no sender at all is only
// risky: Pocket ID starts, and every mail it sends has an empty From.
func (c *checker) pocketIDSMTPSender() {
	for _, name := range c.pocketIDApps() {
		smtp := c.cfg.SMTPFor(name)
		if smtp.Host == "" {
			continue
		}
		key := "smtp.from_address"
		if o := c.cfg.Apps[name].SMTP; o != nil && o.FromAddress != "" {
			key = fmt.Sprintf("apps.%s.smtp.from_address", name)
		}
		if smtp.FromAddress == "" {
			c.warn("pocket-id-smtp-without-sender", key,
				"is not set, so %s's mail would be sent with an empty sender. Pocket ID starts, but a mail server is likely to refuse it. Set from_address.", name)
			continue
		}
		if addr, err := mail.ParseAddress(smtp.FromAddress); err != nil || addr.Address != smtp.FromAddress {
			c.refuse("pocket-id-smtp-from-not-an-email", key,
				"is %q, which %s renders as SMTP_FROM, and Pocket ID will not start unless that is an email address alone, Eg: hello@example.org.", smtp.FromAddress, name)
		}
	}
}

// pocketIDSMTPFromName warns about a sender name set on the Pocket ID app's
// own smtp block. Pocket ID has no such setting: its mail goes out under
// APP_NAME (email/module.go:212), so the name is not used. The deployment's
// own from_name is not warned about, because the other kinds that send mail
// use it.
func (c *checker) pocketIDSMTPFromName() {
	for _, name := range c.pocketIDApps() {
		o := c.cfg.Apps[name].SMTP
		if o == nil || o.FromName == "" {
			continue
		}
		c.warn("pocket-id-smtp-from-name", fmt.Sprintf("apps.%s.smtp.from_name", name),
			"is set, and Pocket ID has no sender name setting: its mail is sent under its app name. Set apps.%s.settings.app_name to change it, and remove from_name here.", name)
	}
}
