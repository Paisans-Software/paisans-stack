package apply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// How long a stack has to come up healthy after its action, and how often to
// ask. Five minutes covers a first start that runs migrations or initdb behind
// a healthcheck's start period, without leaving an operator watching a frozen
// terminal for longer than it takes to read the logs themselves.
var (
	healthWait = 5 * time.Minute
	healthPoll = 5 * time.Second
)

// container is the part of one `docker compose ps --format json` entry the
// health gate reads. The keys are the method names of Compose's
// ContainerContext (docker/compose, cmd/formatter/container.go), which
// `--format json` renders through docker/cli's `{{json .}}`; Health is empty
// unless the container is running and defines a healthcheck
// (pkg/compose/ps.go, containerHealthAndExitCode).
type container struct {
	ID       string `json:"ID"`
	Service  string `json:"Service"`
	Name     string `json:"Name"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

// parseContainers reads either shape Compose has printed. Current Compose
// prints one JSON object per line, one per container (cmd/formatter/
// container.go, NewContainerFormat); older Compose v2 releases printed the
// whole list as one JSON array (cmd/formatter/formatter.go, Print, case JSON
// over a slice). Both carry the same keys.
func parseContainers(out string) ([]container, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []container
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var list []container
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c container
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, nil
}

// verdict is what one look at a stack's containers says.
type verdict int

const (
	// waiting means not every container is up yet, and nothing has failed.
	waiting verdict = iota
	// healthy means every container runs and every healthcheck passes.
	healthy
	// failed means a container exited, is being restarted, or reports
	// unhealthy. Waiting longer would not help: Docker is already restarting
	// it, or its own check has given up.
	failed
)

// judge decides a stack's state, and names the services behind anything
// other than healthy. A stack with no containers at all is waiting: after an
// `up -d` or a restart there must be at least one.
func judge(list []container) (verdict, []string) {
	if len(list) == 0 {
		return waiting, nil
	}
	var bad, slow []string
	for _, c := range list {
		switch {
		case c.State == "exited" || c.State == "dead" || c.State == "restarting":
			bad = append(bad, fmt.Sprintf("%s (%s, exit code %d)", c.Service, c.State, c.ExitCode))
		case c.Health == "unhealthy":
			bad = append(bad, c.Service+" (unhealthy)")
		case c.State != "running":
			slow = append(slow, fmt.Sprintf("%s (%s)", c.Service, c.State))
		case c.Health == "" || c.Health == "none" || c.Health == "healthy":
			// A container without a healthcheck counts once it runs.
		default:
			slow = append(slow, fmt.Sprintf("%s (%s)", c.Service, c.Health))
		}
	}
	if len(bad) > 0 {
		return failed, bad
	}
	if len(slow) > 0 {
		return waiting, slow
	}
	return healthy, nil
}

// waitHealthy is the gate after each stack's action: every container of the
// compose project running, and every one with a healthcheck reporting
// healthy. `up -d` and `restart` return as soon as the containers start, so
// without it an apply reported success over an Mbin that could not reach its
// database.
//
// `--all` because a container that exited is exactly what this gate is for,
// and plain `ps` lists only running ones.
func waitHealthy(plan *Plan, stack string, t Transport) error {
	return waitStack(plan.Site, stack, t, applyStopped)
}

// applyStopped is what a failed gate tells the operator inside an apply.
const applyStopped = "The apply stopped here and nothing after it was started; the next apply resumes at this stack and recreates it"

// WaitHealthy is the same gate for a command outside apply that has just
// started a stack: it waits as apply does, and on failure names the services
// with the tail of their logs, then stopped, which says what the caller did
// and did not do.
func WaitHealthy(site, stack string, t Transport, stopped string) error {
	return waitStack(site, stack, t, stopped)
}

func waitStack(site, stack string, t Transport, stopped string) error {
	compose := fmt.Sprintf("docker compose -f /srv/%s/compose.yaml", stack)
	attempts := int(healthWait / healthPoll)
	if attempts < 1 {
		attempts = 1
	}
	var named []string
	var last string
	for i := 0; i < attempts; i++ {
		out, err := t.Run(compose + " ps --all --format json")
		if err != nil {
			last = err.Error()
		} else if list, perr := parseContainers(out); perr != nil {
			last = fmt.Sprintf("unreadable `ps` output: %v", perr)
		} else {
			v, names := judge(list)
			switch v {
			case healthy:
				return nil
			case failed:
				return unhealthy(site, stack, compose, "is not healthy", names, stopped, t)
			}
			named, last = names, ""
			if len(list) == 0 {
				last = "no containers"
			}
		}
		if i < attempts-1 {
			sleep(healthPoll)
		}
	}
	if last != "" && len(named) == 0 {
		return fmt.Errorf("%s: stack %s did not come up healthy within %s (%s). %s", site, stack, healthWait, last, stopped)
	}
	return unhealthy(site, stack, compose, fmt.Sprintf("did not come up healthy within %s", healthWait), named, stopped, t)
}

// unhealthy builds the gate's error, with the tail of each named service's
// log, since the log is the first thing anyone would ask for.
func unhealthy(site, stack, compose, what string, names []string, stopped string, t Transport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: stack %s %s: %s. %s",
		site, stack, what, strings.Join(names, ", "), stopped)
	seen := map[string]bool{}
	var services []string
	for _, name := range names {
		service, _, _ := strings.Cut(name, " ")
		if !seen[service] {
			seen[service] = true
			services = append(services, service)
		}
	}
	sort.Strings(services)
	for _, service := range services {
		out, err := t.Run(fmt.Sprintf("%s logs --no-color --tail 30 %s", compose, shellQuote(service)))
		if err != nil && out == "" {
			out = err.Error()
		}
		fmt.Fprintf(&b, "\n\n--- %s, last 30 log lines ---\n%s", service, strings.TrimRight(out, "\n"))
	}
	return fmt.Errorf("%s", b.String())
}

// StackHealthy is the health gate's judgement from one look, for a command
// that checks a running stack rather than one it just started: nil when every
// container of /srv/<stack> runs and passes its healthcheck, and otherwise an
// error naming the services that do not. It does not wait; a caller that
// expects a stack to recover polls it.
func StackHealthy(stack string, t Transport) error {
	compose := fmt.Sprintf("docker compose -f /srv/%s/compose.yaml", stack)
	out, err := t.Run(compose + " ps --all --format json")
	if err != nil {
		return fmt.Errorf("stack %s: %v", stack, err)
	}
	list, err := parseContainers(out)
	if err != nil {
		return fmt.Errorf("stack %s: unreadable `ps` output: %v", stack, err)
	}
	switch v, names := judge(list); {
	case v == healthy:
		return nil
	case len(list) == 0:
		return fmt.Errorf("stack %s: no containers", stack)
	default:
		return fmt.Errorf("stack %s: %s", stack, strings.Join(names, ", "))
	}
}
