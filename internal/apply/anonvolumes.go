package apply

import (
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// stackVolumes lists the volumes a stack's current containers mount, run
// just before a recreate replaces them.
func stackVolumes(d deployment.Deployment, stack string) string {
	return d.ComposeCmd(stack) + ` ps -aq | xargs -r docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}'`
}

// recordAnonymousVolumes returns the anonymous volumes a stack's containers
// mount now, before its recreate. A failure to read them is a warning: it
// costs the cleanup after the recreate, never the recreate itself.
func recordAnonymousVolumes(plan *Plan, stack string, t Transport) []string {
	out, err := t.Run(stackVolumes(plan.Deployment, stack))
	if err != nil {
		plan.say("  %-9s %s: the volumes its old containers used were not read, so none will be removed after it: %s\n", "warning", stack, firstLine(strings.TrimSpace(out)))
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); anonymousName.MatchString(line) {
			names = append(names, line)
		}
	}
	return names
}

// removeAbandonedVolumes removes those of a stack's previous anonymous
// volumes that nothing mounts now that it is healthy. With the guard in
// place none should exist; this is what keeps an image that slipped past it
// (a tag moved between the check and the pull) from leaking. A volume the new
// containers took over is still mounted, so not dangling, and stays.
func removeAbandonedVolumes(plan *Plan, stack string, previous []string, t Transport) {
	if len(previous) == 0 {
		return
	}
	out, err := t.Run("docker volume ls -qf dangling=true")
	if err != nil {
		plan.say("  %-9s %s: its old containers' anonymous volumes were left in place: listing dangling volumes: %s\n", "warning", stack, firstLine(strings.TrimSpace(out)))
		return
	}
	dangling := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		dangling[strings.TrimSpace(line)] = true
	}
	for _, name := range previous {
		if !dangling[name] {
			continue
		}
		if out, err := t.Run("docker volume rm " + shellQuote(name)); err != nil {
			plan.say("  %-9s %s: could not remove anonymous volume %s: %s\n", "warning", stack, name, firstLine(strings.TrimSpace(out)))
			continue
		}
		plan.say("  %-9s anonymous volume %s, left by %s's previous containers\n", "removed", name, stack)
	}
}
