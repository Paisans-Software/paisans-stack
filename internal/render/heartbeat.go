package render

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The per site heartbeat, the reverse of the per site ping. The monitor pings
// a site over the mesh; the site's host pushes to the monitor's public
// hostname over the internet, once a minute, from a small service in its
// infrastructure stack. The two paths fail differently, which is what tells
// the failures apart: a ping down with the heartbeat fresh is the mesh, a
// ping up with the heartbeat stale is the site's own way out, every ping down
// with every heartbeat fresh is the monitor's own tunnel, and both down is
// the host. README, "Monitoring", has the table; docs/specs/
// 2026-10-07-uptime-monitoring.md, the amendment of 2026-10-09, the design.

// HeartbeatImage is what the heartbeat service runs: curl's own image, which
// is Alpine with curl and a shell and nothing else, pinned by tag like every
// other infrastructure image in infra-compose.yaml.tmpl. 8.11.1 is the
// release of 2024-12-11 as published on Docker Hub; see kinds.ImageVolumes
// for what it declares.
const HeartbeatImage = "docker.io/curlimages/curl:8.11.1"

// heartbeatGraceSeconds is added to the interval by the fork before a
// heartbeat is stale (src/lib/checker.js evaluateHeartbeat at 1.1.0-oidc.4:
// tolerated = interval_seconds + heartbeat_grace_seconds). With the seed's
// interval of 60 the site is stale after 180 s, which is one push missed and
// the next one late: a push is a curl with a 10 s timeout retried twice, so
// one that fails outright takes about 33 s, and a cycle is one minute from
// its start whatever the pushes took. A grace of 60 would be stale at 120,
// which one missed push plus that delay reaches.
const heartbeatGraceSeconds = 120

// heartbeatTokenShape is the only token the toolkit renders: 32 lowercase
// hex characters, what secretsgen generates and what the fork's seed takes
// (src/lib/sitePayload.js insertSite at 1.1.0-oidc.4). The fork invents its
// own token in place of one it does not accept, silently, so a token of any
// other shape is a host pushing at a URL the monitor answers 404 to, which it
// reports as the site being down. Refused here, by name, instead.
var heartbeatTokenShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// heartbeatName is the seed's name for a site's heartbeat, built like
// pingName from the site key so a renamed hostname updates it in place.
func heartbeatName(site string) string { return site + " — heartbeat" }

// heartbeatTarget is one monitor a site pushes to.
type heartbeatTarget struct {
	App      string
	Hostname string
}

// heartbeatTargets is every monitor a site pushes to: each uptime app pinned
// to another site, in app order. A monitor on the site itself is left out,
// since a push from the host it runs on can only ever arrive, and so proves
// nothing; and with one monitor its own site therefore pushes nowhere. A
// second monitor on a second site watches the first's site through this,
// which is what notices the first monitor's host dying.
func heartbeatTargets(cfg *config.Config, site string) []heartbeatTarget {
	var out []heartbeatTarget
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if app.Kind != config.KindUptime || app.Placement.Site == site {
			continue
		}
		out = append(out, heartbeatTarget{App: name, Hostname: app.Hostname})
	}
	return out
}

// heartbeatToken is a site's token from the secrets, refused by name when it
// is missing or not what the fork accepts. Missing sends the operator to
// init, which generates one, as a missing rpc_secret does.
func (p *planner) heartbeatToken(site string) (string, error) {
	token := p.secrets.Sites[site].HeartbeatToken
	if token == "" {
		return "", fmt.Errorf("secrets sites.%s.heartbeat_token: required for a deployment with a monitor, which expects every other site to push a heartbeat. Run `paisans init` to generate it", site)
	}
	if !heartbeatTokenShape.MatchString(token) {
		return "", fmt.Errorf("secrets sites.%s.heartbeat_token: is not 32 lowercase hex characters, the one shape the monitor's seed accepts. It would replace it with a token of its own, and the host would push at a URL the monitor answers 404 to. Run `paisans init` after removing it, which generates one in the right shape", site)
	}
	return token, nil
}

// HeartbeatURL is where a site pushes to one monitor: the fork's ping route
// at the monitor's public hostname, through DNS and whatever serves that
// hostname, and never the mesh. The monitor's Caddy snippet, and the
// snippets `paisans ingress show` prints for an operator's own web server,
// let /ping/* through.
func HeartbeatURL(hostname, token string) string {
	return "https://" + hostname + "/ping/" + token
}

// heartbeatURLs is the space separated list the heartbeat service reads from
// HEARTBEAT_URLS, one URL per monitor off this site with the site's one
// token. One token serves every monitor because the fork keeps tokens unique
// within one instance only (src/lib/sitePayload.js insertSite).
func (p *planner) heartbeatURLs(site string, targets []heartbeatTarget) (string, error) {
	token, err := p.heartbeatToken(site)
	if err != nil {
		return "", err
	}
	urls := make([]string, 0, len(targets))
	for _, t := range targets {
		urls = append(urls, HeartbeatURL(t.Hostname, token))
	}
	return strings.Join(urls, " "), nil
}
