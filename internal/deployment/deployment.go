// Package deployment is a deployment's identity and every name and path
// derived from it. It is the one place those are spelled, so that a compose
// project, a bind mount, a firewall comment and a registry entry can never
// disagree about which deployment they belong to.
//
// A deployment is identified by its id, a random UUID written into
// paisans.yaml by `paisans init` and never changed afterwards. Names carry the
// token, the id's first four hex digits, because a name is read by people and
// a full UUID in every container name is noise. The token is a label for
// humans; the id is the identity. Code that decides whether a Docker object
// belongs to this deployment matches the id in a label, never a name prefix.
// The host registry (internal/registry) is what keeps two deployments on one
// host from sharing a token, and so from sharing a name or a root.
package deployment

import (
	"crypto/rand"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Label is the Docker label every rendered service and network carries, with
// the deployment's id as its value.
const Label = "community.paisans.deployment"

// Base is the directory every deployment's root sits under on a host.
const Base = "/srv/paisans"

// TokenLength is how many hex digits of the id the token keeps.
const TokenLength = 4

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidID reports whether s is a version 4 UUID in canonical, lowercase form.
// Lowercase only, because the id is compared as a string everywhere (a label
// value, a registry key) and two spellings of one id would be two deployments.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// NewID returns a fresh version 4 UUID from crypto/rand.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a deployment id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Deployment is one deployment, by id. Its zero value has an empty token and
// is never valid to render or apply; config.Load refuses a file without an id.
type Deployment struct {
	ID string
}

// Token is the first TokenLength hex digits of the id.
func (d Deployment) Token() string {
	if len(d.ID) < TokenLength {
		return d.ID
	}
	return d.ID[:TokenLength]
}

// Prefix is what every name this deployment owns starts with: paisans-<token>.
func (d Deployment) Prefix() string { return "paisans-" + d.Token() }

// Project is the compose project name of one stack, paisans-<token>-<stack>.
// Containers are named after it by compose, Eg: paisans-f2a9-talk-app-1.
func (d Deployment) Project(stack string) string { return d.Prefix() + "-" + stack }

// Root is the absolute directory everything apply renders for this deployment
// lands under on a host, /srv/paisans/<token>.
func (d Deployment) Root() string { return Base + "/" + d.Token() }

// Rel is Root without its leading slash: the form a rendered file's path
// takes, since rendered paths are relative to the host's /.
func (d Deployment) Rel() string { return strings.TrimPrefix(d.Root(), "/") }

// Dir is one stack's directory, /srv/paisans/<token>/<stack>.
func (d Deployment) Dir(stack string) string { return d.Root() + "/" + stack }

// Compose is one stack's compose file.
func (d Deployment) Compose(stack string) string { return d.Dir(stack) + "/compose.yaml" }

// ComposeCmd is `docker compose -f <stack's compose file>`, the start of every
// command run against one stack.
func (d Deployment) ComposeCmd(stack string) string {
	return "docker compose -f " + d.Compose(stack)
}

// Path joins elements under the root, Eg: Path("infra", "patroni.env").
func (d Deployment) Path(elem ...string) string {
	return path.Join(append([]string{d.Root()}, elem...)...)
}

// RelPath is Path in rendered form, without the leading slash.
func (d Deployment) RelPath(elem ...string) string {
	return strings.TrimPrefix(d.Path(elem...), "/")
}

// Manifest is the toolkit's record of every file apply wrote for this
// deployment on a host.
func (d Deployment) Manifest() string { return d.Path(".paisans-manifest.json") }

// LabelFilter is the `docker ... --filter` argument selecting this
// deployment's objects by label.
func (d Deployment) LabelFilter() string { return "label=" + Label + "=" + d.ID }

// SplitRel splits a rendered path into its stack and the remainder within
// that stack, when it lies under some deployment's root:
// srv/paisans/f2a9/talk/.env is ("f2a9", "talk", ".env", true). A path
// outside every root, such as etc/wireguard/wg0.conf, is not.
func SplitRel(rel string) (token, stack, rest string, ok bool) {
	base := strings.TrimPrefix(Base, "/") + "/"
	if !strings.HasPrefix(rel, base) {
		return "", "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(rel, base), "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
