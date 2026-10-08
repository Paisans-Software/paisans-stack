package doctor

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// StandbyProbe is every site's answer for one Pocket ID app that runs on more
// than one site, from apply.LookAtInstances: the same question apply's gate
// after an apply and failover test both ask.
type StandbyProbe struct {
	App       string
	Instances []apply.Instance
}

// Standby applies apply.OneActive's rule, once, without waiting: doctor
// reports what is, and a takeover in progress is reported with how long it
// takes rather than waited out.
//
// noPrimary is the patroni section's verdict that there is no primary, in
// which case no instance can become active, since each one needs the
// database to take over.
func Standby(list []StandbyProbe, noPrimary bool) []Finding {
	var out []Finding
	for _, p := range list {
		var parts []string
		unasked := false
		for _, in := range p.Instances {
			parts = append(parts, fmt.Sprintf("%s on %s", in.State, in.Site))
			if in.State == apply.Unreachable {
				unasked = true
			}
		}
		summary := strings.Join(parts, ", ")
		_, err := apply.OneActive(p.App, p.Instances)
		switch {
		case err == nil:
			out = append(out, Finding{Section: SectionPocketID, Level: OK, Line: fmt.Sprintf("%s: one active instance (%s)", p.App, summary)})
		case countActive(p.Instances) == 0:
			var more []string
			for _, line := range strings.Split(strings.TrimRight(apply.DescribeInstances(p.Instances), "\n"), "\n") {
				more = append(more, strings.TrimSpace(line))
			}
			if noPrimary {
				more = append(more, "There is no database primary (see patroni), and an instance needs the database to become active. It follows once there is one.")
			}
			if unasked {
				more = append(more, "A site that could not be asked may hold the active instance, but the gateway cannot reach it either if the site is down.")
			}
			more = append(more,
				"A standing by instance takes over at its next retry, every 15 s (PAISANS_STANDBY_RETRY in the rendered paisans-standby.sh). After an unclean stop the old instance's registration ages for about 90 s first, so allow up to about two minutes.",
				fmt.Sprintf("If none has taken over after that, read `docker compose -f /srv/%s/compose.yaml logs app` on each site above.", p.App))
			out = append(out, Finding{Section: SectionPocketID, Level: Fail, Line: fmt.Sprintf("%s: no active instance, so sign in is down (%s)", p.App, summary), More: more})
		default:
			out = append(out, Finding{Section: SectionPocketID, Level: Fail, Line: err.Error()})
		}
	}
	return out
}

func countActive(list []apply.Instance) int {
	n := 0
	for _, in := range list {
		if in.State == apply.Active {
			n++
		}
	}
	return n
}
