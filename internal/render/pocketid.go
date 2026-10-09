package render

import (
	"encoding/json"
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/configmerge"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// pocketIDConfig is the pocket-id kind's application configuration, quoted
// for its .env. See kinds.PocketIDSettings for why it is rendered at all.
type pocketIDConfig struct {
	// Env is every declared setting's variable, then the resolved signup
	// groups, in kinds.PocketIDSettings order.
	Env []kinds.PocketIDEnvVar
	// Unresolved names the signup groups with no ID recorded under
	// pocket_id_groups yet, which the .env says in a comment. apply resolves
	// them on a site running Pocket ID and renders the .env again.
	Unresolved []string
}

// pocketIDConfigFor renders an app's settings and its recorded signup group
// IDs. It depends only on the configuration and the secrets file, never on
// which site it is rendered for, so every instance of a Pocket ID on more
// than one apps site renders the same .env apart from PAISANS_SITE and the
// database host.
func (p *planner) pocketIDConfigFor(name string, app config.App) (pocketIDConfig, error) {
	var out pocketIDConfig
	for _, v := range kinds.PocketIDEnv(app.Settings) {
		quoted, err := configmerge.EnvValue(fmt.Sprintf("apps.%s.settings (%s)", name, v.Name), v.Value)
		if err != nil {
			return pocketIDConfig{}, err
		}
		out.Env = append(out.Env, kinds.PocketIDEnvVar{Name: v.Name, Value: quoted})
	}
	groups := kinds.PocketIDSignupGroups(app.Settings)
	if len(groups) == 0 {
		return out, nil
	}
	ids := []string{}
	for _, group := range groups {
		id := p.secrets.PocketIDGroups[group]
		if id == "" {
			out.Unresolved = append(out.Unresolved, group)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return pocketIDConfig{}, err
	}
	quoted, err := configmerge.EnvValue("pocket_id_groups", string(raw))
	if err != nil {
		return pocketIDConfig{}, err
	}
	out.Env = append(out.Env, kinds.PocketIDEnvVar{Name: kinds.PocketIDSignupGroupsEnv, Value: quoted})
	return out, nil
}

// envquote quotes a value for a .env as configmerge does for a passthrough
// key, for a template placing a value an operator supplied, such as an SMTP
// password from a mail provider, which may hold `$` or `#`.
func envquote(v string) (string, error) { return configmerge.EnvValue("value", v) }
