package render

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/configmerge"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// settingOwner records that a `settings` key is the sanctioned input for a
// value a kind's templates render. When a passthrough key collides with one of
// these, the refusal names the setting rather than the template, because that
// is the way to change the value that somebody will review.
//
// Only keys a setting genuinely controls are here. A rendered key that merely
// depends on a setting among other things, such as Mbin's KBIN_STORAGE_URL,
// is not: naming a setting that only partly decides a value would send an
// operator to the wrong place. Element's base_url and server_name are fed by
// settings too, but they sit under "m.homeserver", which configmerge refuses
// as an ambiguous path before any collision is found, so an entry for them
// could never be reached.
//
// TestEverySettingThatOwnsAKeyReallyControlsIt renders each row with the
// setting changed and checks the key's line changes, so a row that goes stale
// fails a test rather than misdirecting an operator.
type settingOwner struct {
	Kind config.Kind
	// Key is the passthrough key, in that kind's `config` syntax.
	Key string
	// File is the stack file the key is rendered into.
	File    string
	Setting string
}

var settingOwners = []settingOwner{
	{config.KindElement, "brand", "config.json", "brand"},

	{config.KindMbin, "OAUTH_OIDC_ADMIN_GROUP", ".env", "admin_group"},
	{config.KindMbin, "OAUTH_OIDC_MEMBER_GROUP", ".env", "member_group"},
	{config.KindMbin, "S3_BUCKET", ".env", "s3_bucket"},
	{config.KindMbin, "S3_REGION", ".env", "s3_region"},

	{config.KindOAuth2Proxy, "OAUTH2_PROXY_ALLOWED_GROUPS", "compose.yaml", "members_group"},

	{config.KindOutline, "AWS_REGION", ".env", "s3_region"},
	{config.KindOutline, "AWS_S3_FORCE_PATH_STYLE", ".env", "s3_force_path_style"},
	{config.KindOutline, "AWS_S3_UPLOAD_BUCKET_NAME", ".env", "s3_bucket"},

	{config.KindPocketID, "FILE_BACKEND", ".env", "file_backend"},

	{config.KindWriteFreely, "app.federation", "config.ini", "federation"},
	{config.KindWriteFreely, "app.min_username_len", "config.ini", "min_username_len"},
	{config.KindWriteFreely, "app.private", "config.ini", "private"},
	{config.KindWriteFreely, "app.site_name", "config.ini", "site_name"},
	{config.KindWriteFreely, "app.theme", "config.ini", "theme"},
	{config.KindWriteFreely, "storage.s3_bucket", "config.ini", "s3_bucket"},
	{config.KindWriteFreely, "storage.s3_region", "config.ini", "s3_region"},
	{config.KindWriteFreely, "uploads.max_size_mb", "config.ini", "uploads_max_size_mb"},
}

// Every Pocket ID setting in kinds.PocketIDSettings owns its variable. They
// are taken from that table rather than listed again here, so a setting added
// there is an owner without a second edit. signup_default_groups is not among
// them: its variable carries recorded IDs, which the setting chooses only by
// way of apply. validate refuses a passthrough key for it, and for every other
// variable a setting renders, whether or not the setting is declared
// (pocket-id-config-key-has-a-setting).
func init() {
	for _, s := range kinds.PocketIDSettings {
		settingOwners = append(settingOwners, settingOwner{config.KindPocketID, s.Env, ".env", s.Key})
	}
}

// owningSetting is the setting that owns a kind's rendered key, if any.
func owningSetting(kind config.Kind, key string) (string, bool) {
	for _, o := range settingOwners {
		if o.Kind == kind && o.Key == key {
			return o.Setting, true
		}
	}
	return "", false
}

// templateOmission records a key a kind's template leaves out of its config
// file on purpose, because putting it back would undo a decision the toolkit
// made. Such a key is owned by the template exactly as a key it writes is, and
// a passthrough key that names it is refused as config-key-already-rendered.
//
// Only keys the template's own comments name as deliberately absent are here,
// so that this table records decisions rather than guesses. Synapse's header
// says the file "deliberately declares no identity provider of its own and no
// local password database". The six keys below are Synapse's blocks for
// those: oidc_providers, oidc_config (its deprecated single provider form),
// saml2_config, cas_config and jwt_config each describe a way to register or
// log in, and password_config the local password logins. Read in
// docs/usage/configuration/config_documentation.md at tag v1.160.0, the
// version the kind pins, on 2026-10-04. WriteFreely's [uploads] comment says
// "dir is deliberately absent".
type templateOmission struct {
	Kind config.Kind
	// Key is in that kind's `config` syntax. It matches itself and any path
	// beneath it, so oidc_config.enabled is caught by oidc_config.
	Key string
	// Why completes the refusal's "leaves <key> out of <file> on purpose: ".
	Why string
}

const synapseIdentity = "this homeserver is a resource server and Matrix Authentication Service owns every login, so an identity provider or a password database of its own would be a second way in that bypasses the group restriction at the identity provider"

var templateOmissions = []templateOmission{
	{config.KindSynapse, "cas_config", synapseIdentity},
	{config.KindSynapse, "jwt_config", synapseIdentity},
	{config.KindSynapse, "oidc_config", synapseIdentity},
	{config.KindSynapse, "oidc_providers", synapseIdentity},
	{config.KindSynapse, "password_config", synapseIdentity},
	{config.KindSynapse, "saml2_config", synapseIdentity},

	{config.KindWriteFreely, "uploads.dir", "uploads are stored in S3 through [storage], and dir names a path on one node that only the local image store reads, so it would do nothing here and suggest that it did"},
}

// omittedOnPurpose is the omission a passthrough key falls under, if any.
func omittedOnPurpose(kind config.Kind, key string) (templateOmission, bool) {
	for _, o := range templateOmissions {
		if o.Kind == kind && (key == o.Key || strings.HasPrefix(key, o.Key+".")) {
			return o, true
		}
	}
	return templateOmission{}, false
}

// mergers is the configmerge function for each format.
var mergers = map[kinds.Format]func(string, map[string]any) (string, error){
	kinds.ConfigEnv:  configmerge.Env,
	kinds.ConfigINI:  configmerge.INI,
	kinds.ConfigYAML: configmerge.YAML,
	kinds.ConfigJSON: configmerge.JSON,
}

// mergeConfig places an app's passthrough keys into the one file its kind
// reads its configuration from, and into no other. files is the app's own
// rendered set and sources maps each of its paths to the template it came
// from, so a refusal can say which template owns the key.
//
// It is called only when the app declares keys, so an app without them
// renders exactly what its templates wrote.
func mergeConfig(app plannedApp, keys map[string]any, stack string, files []File, sources map[string]string) error {
	format := kinds.ConfigFormat(app.Kind)
	merge, ok := mergers[format]
	if !ok {
		// Unreachable today: every kind renders a config file, so no rule in
		// validate refuses this first (that refusal was dropped for having
		// nothing to refuse). This is the guard for a future kind without one.
		return fmt.Errorf("apps.%s.config: kind %s renders no configuration file, so there is nowhere for a passthrough key to go", app.Name, app.Kind)
	}
	name := kinds.ConfigFile(app.Kind)
	target := -1
	for i, f := range files {
		if f.Path == stack+name {
			target = i
		}
	}
	if target < 0 {
		return fmt.Errorf("apps.%s.config: kind %s reads its configuration from %s, but its template set rendered no such file", app.Name, app.Kind, name)
	}

	for _, key := range sortedKeys(keys) {
		if o, ok := omittedOnPurpose(app.Kind, key); ok {
			return fmt.Errorf("config-key-already-rendered: apps.%s.config.%s: the template %s leaves %s out of %s/%s on purpose: %s. A key a template omits by decision is owned by the template as surely as one it writes, so a passthrough key may not put it back; that is a template change, and it is reviewable.",
				app.Name, key, strings.TrimPrefix(sources[files[target].Path], "templates/"), o.Key, app.Dir, name, o.Why)
		}
	}

	// Compose gives a service's `environment:` precedence over its env_file,
	// and interpolates ${VAR} from the .env beside it. An env key compose
	// already sets would be silently shadowed, and one it reads would change
	// the compose file rather than the application, so both are refused here
	// against the rendered compose.yaml.
	if format == kinds.ConfigEnv {
		for _, f := range files {
			if f.Path != stack+"compose.yaml" {
				continue
			}
			for _, key := range sortedKeys(keys) {
				set, err := composeSets(f.Content, key)
				if err != nil {
					return fmt.Errorf("apps.%s.config: reading the rendered compose.yaml: %w", app.Name, err)
				}
				if set {
					return alreadyRendered(app, key, "compose.yaml", sources[f.Path],
						" Compose gives a service's environment: precedence over env_file and interpolates ${...} from .env, so the value would be shadowed or would change the compose file rather than the application.")
				}
			}
		}
	}

	out, err := merge(files[target].Content, keys)
	var collision *configmerge.Collision
	if errors.As(err, &collision) {
		return alreadyRendered(app, collision.Key, name, sources[files[target].Path], "")
	}
	if err != nil {
		return fmt.Errorf("apps.%s.config: %w", app.Name, err)
	}
	files[target].Content = out
	return nil
}

// alreadyRendered is the refusal for a passthrough key the stack already
// writes. Two sources of truth for one value is how a deployment ends up with
// a setting nobody can locate, and how the toolkit's own decisions get
// quietly undone, so it stops the build rather than overwriting.
func alreadyRendered(app plannedApp, key, file, template, why string) error {
	where := app.Dir + "/" + file
	if setting, ok := owningSetting(app.Kind, key); ok {
		return fmt.Errorf("config-key-already-rendered: apps.%s.config.%s: %s already sets this key, and apps.%s.settings.%s is the sanctioned input for it. Set that instead and remove the key from config.%s",
			app.Name, key, where, app.Name, setting, why)
	}
	return fmt.Errorf("config-key-already-rendered: apps.%s.config.%s: %s already sets this key, written by the template %s. A passthrough key may only add what a template does not write: changing a template owned value is a settings key or a template change, and both are reviewable.%s",
		app.Name, key, where, strings.TrimPrefix(template, "templates/"), why)
}

// interpolation matches compose's variable syntax: $$ is an escaped dollar,
// and ${NAME...} or $NAME reads NAME.
var interpolation = regexp.MustCompile(`\$(\$|\{([A-Za-z_][A-Za-z0-9_]*)|([A-Za-z_][A-Za-z0-9_]*))`)

// composeSets reports whether a rendered compose file sets key under any
// service's environment:, in map or list form, or reads it by interpolation
// anywhere. It reads the parsed document rather than the text, so a name that
// appears only in a comment does not count: compose never reads one.
func composeSets(compose, key string) (bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return false, err
	}
	if len(doc.Content) == 0 {
		return false, nil
	}
	root := doc.Content[0]

	if services := mappingValue(root, "services"); services != nil && services.Kind == yaml.MappingNode {
		for i := 1; i < len(services.Content); i += 2 {
			env := mappingValue(services.Content[i], "environment")
			if env == nil {
				continue
			}
			switch env.Kind {
			case yaml.MappingNode:
				for j := 0; j < len(env.Content); j += 2 {
					if env.Content[j].Value == key {
						return true, nil
					}
				}
			case yaml.SequenceNode:
				for _, item := range env.Content {
					name, _, _ := strings.Cut(item.Value, "=")
					if name == key {
						return true, nil
					}
				}
			}
		}
	}

	return interpolates(root, key), nil
}

// mappingValue is the value under a key in a yaml mapping, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// interpolates reports whether any scalar under n reads key.
func interpolates(n *yaml.Node, key string) bool {
	if n.Kind == yaml.ScalarNode {
		for _, m := range interpolation.FindAllStringSubmatch(n.Value, -1) {
			if m[2] == key || m[3] == key {
				return true
			}
		}
		return false
	}
	for _, c := range n.Content {
		if interpolates(c, key) {
			return true
		}
	}
	return false
}
