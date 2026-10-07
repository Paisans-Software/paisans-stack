package apply

import (
	"fmt"
	"strings"
)

// runAction runs one stack's action, and then restarts what an `up -d` left
// stale: each service named in Refresh whose container `up -d` did not
// replace.
//
// Whether it replaced one is read from the container IDs before and after,
// rather than from what `up -d` printed. Compose's progress lines are for
// people, change between releases, and go to a terminal that ssh may not
// give it; a container ID is what Docker itself says is the same container.
// Restarting every Refresh service unconditionally was the alternative, and
// it restarts a container `up -d` has just started, which for Patroni on the
// primary is a second failover for one apply.
//
// A restart, not a reload, because these are single file bind mounts (Eg:
// haproxy/haproxy.cfg), and apply replaces a file by renaming a new one over
// it: the running container keeps the inode it was started with
// (moby/moby#15793), and only a restart mounts the new one.
func runAction(action Action, t Transport) error {
	if !action.Recreate || action.Force || len(action.Refresh) == 0 {
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
	var stale []string
	for _, service := range action.Refresh {
		if id := before[service]; id != "" && id == after[service] {
			stale = append(stale, service)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	_, err = t.Run(Action{Stack: action.Stack, Services: stale}.Command())
	return err
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
