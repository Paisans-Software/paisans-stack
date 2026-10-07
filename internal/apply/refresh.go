package apply

import (
	"fmt"
	"strings"
)

// reloadGateway tells a running Caddy to read its configuration again.
const reloadGateway = "docker compose -f /srv/infra/compose.yaml exec -T caddy caddy reload --config /etc/caddy/Caddyfile"

// infraRecreate reports whether the plan recreates the infrastructure stack,
// and whether that recreate is forced.
func infraRecreate(plan *Plan) (up, forced bool) {
	for _, a := range plan.Actions {
		if a.Stack == infraStack && a.Recreate {
			return !a.Force, a.Force
		}
	}
	return false, false
}

// runAction runs one stack's action, and then finishes what an `up -d` left
// undone: each service named in Refresh whose container `up -d` did not
// replace is restarted, and, when reload is set, a Caddy it did not replace
// is reloaded. A Caddy it did replace started on the new routing, so it is
// not reloaded as well.
//
// Whether it replaced one is read from the container IDs before and after,
// rather than from what `up -d` printed. Compose's progress lines are for
// people, change between releases, and go to a terminal that ssh may not
// give it; a container ID is what Docker itself says is the same container.
// Restarting every Refresh service unconditionally was the alternative, and
// it restarts a container `up -d` has just started, which for Patroni on the
// primary is a second failover for one apply.
//
// A restart, not a reload, for Refresh, because those are single file bind
// mounts (Eg: haproxy/haproxy.cfg), and apply replaces a file by renaming a
// new one over it: the running container keeps the inode it was started with
// (moby/moby#15793), and only a restart mounts the new one. Caddy's routing
// is a directory mount, which sees the new files, so a reload is enough.
func runAction(action Action, reload bool, t Transport) error {
	if !action.Recreate || action.Force || (len(action.Refresh) == 0 && !reload) {
		_, err := t.Run(action.Command())
		return err
	}
	before, err := containerIDs(action.Stack, t)
	if err != nil {
		return err
	}
	if _, err := t.Run(action.Command()); err != nil {
		return err
	}
	after, err := containerIDs(action.Stack, t)
	if err != nil {
		return err
	}
	kept := func(service string) bool {
		id := before[service]
		return id != "" && id == after[service]
	}
	var stale []string
	for _, service := range action.Refresh {
		if kept(service) {
			stale = append(stale, service)
		}
	}
	if len(stale) > 0 {
		if _, err := t.Run(Action{Stack: action.Stack, Services: stale}.Command()); err != nil {
			return err
		}
	}
	if reload && kept("caddy") {
		if _, err := t.Run(reloadGateway); err != nil {
			return fmt.Errorf("reloading the gateway: %w", err)
		}
	}
	return nil
}

// containerIDs maps each service of a stack to its container's ID, from
// `docker compose ps --all --format json`, the same read the health gate
// makes. A service with no container is absent from the map.
func containerIDs(stack string, t Transport) (map[string]string, error) {
	out, err := t.Run(fmt.Sprintf("docker compose -f /srv/%s/compose.yaml ps --all --format json", stack))
	if err != nil {
		return nil, fmt.Errorf("stack %s: could not read host state (which containers run): %w", stack, err)
	}
	list, err := parseContainers(out)
	if err != nil {
		return nil, fmt.Errorf("stack %s: unreadable `ps` output: %v", stack, err)
	}
	ids := map[string]string{}
	for _, c := range list {
		if id := strings.TrimSpace(c.ID); id != "" {
			ids[c.Service] = id
		}
	}
	return ids, nil
}
