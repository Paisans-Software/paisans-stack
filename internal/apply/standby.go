package apply

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// The deployment level half of Pocket ID's standby (README, "Pocket ID runs on
// every apps site, and one of them is active"). Each apps site's stack passes
// apply's own gate whether it is active or standing by, since standing by is
// healthy; what no one stack can see is whether exactly one of them is
// serving. This asks every site.

// How long to wait for an active instance to appear, and how often to look.
// Three minutes covers the slow case: an active instance that died without
// deregistering leaves its registration to age for 90 s (francis's deadline
// with HA off) before a standby's next retry, 15 s later, is admitted.
var (
	standbyWait = 3 * time.Minute
	standbyPoll = 5 * time.Second
)

// InstanceState is what one site's instance of a Pocket ID app is doing.
type InstanceState string

const (
	// Active means its /healthz answers on the site's mesh address.
	Active InstanceState = "active"
	// Standby means the wrapper's state file is present: another instance
	// holds the database and this one is waiting to retry.
	Standby InstanceState = "standby"
	// Down means neither: starting, restarting, or failed.
	Down InstanceState = "down"
	// Absent means the site has no stack for the app yet, as on the first
	// apply of a deployment that adds sites one at a time.
	Absent InstanceState = "absent"
	// Unreachable means the site could not be asked. It is reported and not
	// counted: it may hold an active instance, but nothing here can say.
	Unreachable InstanceState = "unreachable"
)

// Instance is one site's answer.
type Instance struct {
	Site   string
	State  InstanceState
	Detail string
}

// StandbyApps names the Pocket ID apps that run on more than one site, which
// are the ones with a standby to check.
func StandbyApps(cfg *config.Config) []string {
	var out []string
	sites := render.AppSites(cfg)
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID && len(sites[name]) > 1 {
			out = append(out, name)
		}
	}
	return out
}

// instanceCommand asks a site in one shell command, printing one word. The
// state file first, through `docker compose exec`, which fails for a
// container that is not running and so falls through; then /healthz on the
// mesh address, the same address and port the gateway dials.
func instanceCommand(d deployment.Deployment, app, address string, port int) string {
	compose := d.Compose(app)
	return fmt.Sprintf("if [ ! -f %[1]s ]; then echo absent; "+
		"elif docker compose -f %[1]s exec -T app test -f /tmp/paisans-standby >/dev/null 2>&1; then echo standby; "+
		"elif curl --silent --fail --max-time 5 --output /dev/null http://%[2]s:%[3]d/healthz; then echo active; "+
		"else echo down; fi", compose, address, port)
}

// LookAtInstances asks every site an app runs on what its instance is doing,
// in the order the gateway lists them.
func LookAtInstances(cfg *config.Config, app string, transports map[string]Transport) []Instance {
	port := render.AppPort(config.KindPocketID)
	var out []Instance
	for _, site := range render.AppSites(cfg)[app] {
		address := cfg.Sites[site].Address
		t, ok := transports[site]
		if !ok {
			out = append(out, Instance{Site: site, State: Unreachable, Detail: "no transport"})
			continue
		}
		answer, err := t.Run(instanceCommand(cfg.Deployment(), app, address, port))
		if err != nil {
			out = append(out, Instance{Site: site, State: Unreachable, Detail: firstLine(strings.TrimSpace(err.Error()))})
			continue
		}
		in := Instance{Site: site, State: InstanceState(strings.TrimSpace(answer))}
		switch in.State {
		case Active:
			in.Detail = fmt.Sprintf("/healthz answered on %s:%d", address, port)
		case Standby:
			in.Detail = "another instance holds the database"
		case Down:
			in.Detail = "running neither as active nor on standby"
		case Absent:
			in.Detail = fmt.Sprintf("no %s on this site yet", cfg.Deployment().Dir(app))
		default:
			in.State, in.Detail = Unreachable, fmt.Sprintf("unreadable answer %q", strings.TrimSpace(answer))
		}
		out = append(out, in)
	}
	return out
}

// OneActive is the rule: exactly one instance active. It returns whether
// waiting could fix what is wrong, and what is wrong, nil when nothing is. None active may be a takeover
// in progress; two active is the condition the standby exists to prevent,
// and waiting would only let both keep serving.
func OneActive(d deployment.Deployment, app string, list []Instance) (wait bool, err error) {
	var active []string
	for _, in := range list {
		if in.State == Active {
			active = append(active, in.Site)
		}
	}
	switch len(active) {
	case 1:
		return false, nil
	case 0:
		return true, fmt.Errorf("pocket-id %s: no site has an active instance", app)
	default:
		return false, fmt.Errorf("pocket-id %s: %d sites have an active instance (%s), and Pocket ID admits one per database. Stop all but one now, with `%s stop app` on the others, and read their logs: two answering means they do not share one database, or one of them was not refused", app, len(active), strings.Join(active, ", "), d.ComposeCmd(app))
	}
}

// DescribeInstances is one line per site, for an operator.
func DescribeInstances(list []Instance) string {
	var b strings.Builder
	for _, in := range list {
		fmt.Fprintf(&b, "  %-8s %-11s %s\n", in.Site, in.State, in.Detail)
	}
	return b.String()
}

// CheckOneActive waits until exactly one instance of a Pocket ID app is
// active, printing what it saw, and fails at once on two or more and after
// standbyWait on none. A caller runs it after anything that may have
// restarted an instance: an apply that acted on the stack, a switchover.
func CheckOneActive(cfg *config.Config, app string, transports map[string]Transport, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	attempts := int(standbyWait / standbyPoll)
	if attempts < 1 {
		attempts = 1
	}
	var list []Instance
	var err error
	wait := false
	for i := 0; i < attempts; i++ {
		list = LookAtInstances(cfg, app, transports)
		wait, err = OneActive(cfg.Deployment(), app, list)
		if err == nil {
			fmt.Fprintf(out, "pocket-id %s: one active instance\n%s", app, DescribeInstances(list))
			return nil
		}
		if !wait {
			break
		}
		if i < attempts-1 {
			sleep(standbyPoll)
		}
	}
	fmt.Fprintf(out, "pocket-id %s:\n%s", app, DescribeInstances(list))
	if wait {
		return fmt.Errorf("%w within %s. Sign in is down until one is. Read `%s logs app` on each site above", err, standbyWait, cfg.Deployment().ComposeCmd(app))
	}
	return err
}
