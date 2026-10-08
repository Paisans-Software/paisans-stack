package apply

import (
	"fmt"
	"strings"
)

func seenInScope(changes []Change, rel string) bool {
	for _, c := range changes {
		if strings.TrimPrefix(c.Path, remoteRoot) == rel {
			return true
		}
	}
	return false
}

// Rollback puts back what a scoped plan replaced, and records it as this
// toolkit's again, so the next apply sees the old file as its own rather than
// as a host edit.
//
// Only a file the plan updated is restored. A created file had nothing
// before it, and is left: removing the WireGuard file from a new site would take down an
// interface no existing site is talking to any more, which gains nothing. The
// mesh is handed the restored file the same way it was handed the new one, so
// a peer added with `wg syncconf` is removed with `wg syncconf`, without
// taking the interface down.
func Rollback(plan *Plan, t Transport) error {
	if !plan.scoped {
		return fmt.Errorf("%s: only a scoped plan can be rolled back. A whole apply acts on stacks, and a restored file does not undo a recreated container", plan.Site)
	}
	restored := false
	for i, change := range plan.Changes {
		if change.Kind != Update {
			continue
		}
		if err := t.WriteFile(change.Path, change.before, change.Mode); err != nil {
			return fmt.Errorf("%s: restoring %s: %w", plan.Site, change.Path, err)
		}
		plan.Changes[i].content = change.before
		restored = true
	}
	if !restored {
		return nil
	}
	if err := writeManifest(plan, t); err != nil {
		return err
	}
	if command := plan.WireGuard.Command(plan.Deployment); command != "" && plan.WireGuard != WireGuardStart {
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf("%s: handing %s its restored configuration:\n%s", plan.Site, plan.Deployment.Interface(), out)
		}
	}
	return nil
}
