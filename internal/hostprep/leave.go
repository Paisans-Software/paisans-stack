package hostprep

import (
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// What `site remove` reads of what host prepare left, so a deployment leaving
// a host takes away its own rules and nothing else, told apart exactly as
// planRules tells them apart.

// AddedRulesProbe prints "ufw absent", or each of ufw's rules as `ufw show
// added` prints it, after "rule ", whether or not the firewall is active.
const AddedRulesProbe = `if ! command -v ufw >/dev/null 2>&1; then echo "ufw absent"; exit 0; fi; added=$(ufw show added) || exit 1; printf '%s\n' "$added" | sed -n 's/^ufw /rule /p'`

// AddedRule is one ufw rule, as `ufw delete` takes it.
type AddedRule struct {
	// Line is the rule without its leading `ufw`, comment included.
	Line    string
	Comment string
	// Owned is a rule whose comment starts with deployment d's owner tag,
	// paisans-<token>:, which host prepare writes on every rule it adds.
	Owned bool
	// SSH is d's SSH allow, by its comment.
	SSH bool
}

// ParseAddedRules reads AddedRulesProbe's output for deployment d. absent
// is true when the host has no ufw.
func ParseAddedRules(d deployment.Deployment, out string) (rules []AddedRule, absent bool) {
	tag := ownerTag(d)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "ufw absent" {
			return nil, true
		}
		rest, ok := strings.CutPrefix(line, "rule ")
		if !ok {
			continue
		}
		a, ok := parseAdded(rest)
		if !ok {
			continue
		}
		rules = append(rules, AddedRule{
			Line:    a.line,
			Comment: a.comment,
			Owned:   a.owned(tag),
			SSH:     a.comment == tag+" "+sshWhy,
		})
	}
	return rules, false
}

// OwnedKeysGlob matches every deployment's record of the keys host prepare
// added to user's authorized_keys, for a check that another deployment
// records the same key.
func OwnedKeysGlob(user string) string {
	return ownedDir + "/authorized_keys." + user + ".paisans-*.owned"
}

// OwnedKeysPrefixGlob matches the records of keys d added for any user.
func OwnedKeysPrefixGlob(d deployment.Deployment) string {
	return ownedDir + "/authorized_keys.*." + d.Prefix() + ".owned"
}

// OwnedKeysUser is the user a record OwnedKeysPath(d, user) names.
func OwnedKeysUser(d deployment.Deployment, path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, ownedDir+"/authorized_keys."), "."+d.Prefix()+".owned")
}

// ParseOwnedKeys reads a record OwnedKeysPath names: one fingerprint per
// line, with the key's comment after it.
func ParseOwnedKeys(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		fp, _, _ := strings.Cut(line, " ")
		out = append(out, fp)
	}
	return out
}
