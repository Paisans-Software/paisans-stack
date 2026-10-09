package main

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// listPlan is a dry run's plan: one item per step, in the order Execute
// takes them, titled as Execute titles its steps, so a dry run reads as the
// run it previews. Why each step is there, the files it writes and the
// checks around it are details, shown with --verbose. A refusal Execute
// would meet is a refusal here, so a dry run says at once that nothing would
// be applied.
//
// The site's section is the caller's: by the time the plan is listed, the
// checks that built it have already reported under it.
func listPlan(r ui.Reporter, plan *apply.Plan) {
	for _, note := range plan.Notes {
		r.Detail("%s", note)
	}
	// The disk and volume checks ran in Build, as the "disk space" step,
	// which already carries what they found as its details. An image volume
	// left unmounted is still refused here, since Execute will refuse it.
	if plan.Volumes != nil {
		for _, u := range plan.Volumes.Uncovered {
			r.Refuse("volume path not mounted: "+u.Path, u.Describe())
		}
	}

	var unchanged int
	var writes []string
	for _, change := range plan.Changes {
		switch change.Kind {
		case apply.Unchanged:
			unchanged++
			continue
		case apply.Conflict:
			// A conflict is not written; the refusal below names it.
			continue
		}
		kind := change.Kind.String()
		if change.Overwritten {
			kind = "overwrite"
		}
		writes = append(writes, kind+" "+change.Path)
	}
	if len(writes) > 0 {
		r.Item(fmt.Sprintf("write %d files", len(writes)))
		for _, w := range writes {
			r.Detail("%s", w)
		}
	}
	if unchanged > 0 {
		r.Detail("unchanged %s", plural(unchanged, "file"))
	}

	if plan.WireGuard != apply.WireGuardNone {
		r.Item(plan.WireGuard.Title(plan.Deployment))
		r.Detail("%s", plan.WireGuard.Describe(plan.Deployment))
	}

	bootstrapped := plan.Bootstrap == nil
	for _, action := range plan.Actions {
		if action.Stack != "infra" && !bootstrapped {
			listBootstrap(r, plan.Bootstrap)
			bootstrapped = true
		}
		r.Item(apply.ActionTitle(action))
		if action.Down {
			// The one action that removes the stack's network as well as its
			// containers, so it says so.
			r.Detail("down %s, so that its compose network is created as declared", action.Stack)
		}
		r.Detail("%s", action.Reason)
		r.Item("wait for " + action.Stack)
		r.Detail("%s: every container running, and healthy where it has a healthcheck, before anything after it moves", action.Stack)
		var prunes []string
		for _, prune := range plan.Prunes {
			if prune.Stack == action.Stack {
				prunes = append(prunes, prune.Ref)
			}
		}
		if len(prunes) > 0 {
			r.Item("prune old images")
			for _, ref := range prunes {
				r.Detail("%s, superseded, once %s is healthy and if no container still uses it", ref, action.Stack)
			}
		}
	}
	if !bootstrapped {
		listBootstrap(r, plan.Bootstrap)
	}

	if plan.HostSites {
		r.Item("ensure host sites directory")
		r.Detail("%s, if missing, for site blocks the host's owner adds; nothing in it is ever changed", render.HostSitesDir)
	}
	if plan.GatewayChanging && plan.ACMEModule != "" {
		r.Item("check gateway modules")
		r.Detail("the gateway's Caddy carries %s, before anything moves", plan.ACMEModule)
	}
	if plan.GatewayReload {
		r.Item("reload gateway")
		r.Detail("the gateway, after its assembled configuration validates")
	} else if plan.GatewayChanging {
		r.Item("validate gateway config")
		r.Detail("the assembled gateway configuration, before the gateway is replaced")
	}

	if conflicts := plan.Conflicts(); len(conflicts) > 0 {
		verb := "were"
		if len(conflicts) == 1 {
			verb = "was"
		}
		r.Refuse(fmt.Sprintf("%s %s edited on the host", plural(len(conflicts), "file"), verb), "Nothing will be applied until that is resolved.")
		for _, c := range conflicts {
			r.Detail("conflict %s", c.Path)
		}
	}
}

// listBootstrap is the database work, where it happens: after the
// infrastructure stack and before any app stack.
func listBootstrap(r ui.Reporter, b *apply.Bootstrap) {
	if len(b.EtcdUnstarted) > 0 {
		r.Item("stop after the infrastructure stack")
		r.Detail("etcd is being founded and %s runs no etcd yet, so no primary can appear. Apply %s, then this site again",
			strings.Join(b.EtcdUnstarted, ", "), strings.Join(b.EtcdUnstarted, ", then "))
		return
	}
	r.Item("wait for Patroni primary")
	r.Detail("for a Patroni primary at %s, up to 3 minutes; a replica leaves the rest to the leader's site", b.Patroni)
	if len(b.Databases) == 0 {
		return
	}
	// Titled as Execute titles the step, so the item and the step match.
	title := fmt.Sprintf("bootstrap %d databases", len(b.Databases))
	if len(b.Databases) == 1 {
		title = "bootstrap database " + b.Databases[0].Name
	}
	r.Item(title)
	for _, db := range b.Databases {
		r.Detail("database %s: role %s with its password, database owned by it, creating only what is missing", db.App, db.Role)
	}
}
