package main

import (
	"slices"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// convergeState is what decides the plan of `paisans apply` without --site:
// whether init has work, and which etcd members have been founded (hold
// infra/etcd-initial). See docs/specs/2026-10-09-apply-converge.md.
type convergeState struct {
	NeedsInit bool
	Founded   map[string]bool
}

// convergeStep is one existing command the plan runs.
type convergeStep struct {
	Phase string
	// Title is what the operator reads, the command as typed less its
	// common flags.
	Title string
	Why   string
	// Args is the command and its own flags; the run adds --config,
	// --secrets, --execute and --sudo.
	Args []string
	// Founding is an apply whose founding stop is expected: pass two
	// applies it again.
	Founding bool
}

func step(phase, why string, founding bool, args ...string) convergeStep {
	title := args[0]
	for _, a := range args[1:] {
		title += " " + a
	}
	return convergeStep{Phase: phase, Title: title, Why: why, Args: args, Founding: founding}
}

func applyStep(phase, why, site string, founding bool) convergeStep {
	return step(phase, why, founding, "apply", "--site", site)
}

// convergePlan is every step that takes cfg to a complete, running stack from
// what st read, in the order the deployment needs them.
func convergePlan(cfg *config.Config, st convergeState) []convergeStep {
	var steps []convergeStep
	if st.NeedsInit {
		steps = append(steps, step("configuration", "the deployment id, the mesh subnet or a generated secret is missing", false, "init"))
	}
	for _, s := range cfg.SiteNames() {
		steps = append(steps, step("hosts", "Docker, WireGuard and the firewall; a prepared host plans nothing", false, "host", "prepare", "--site", s))
	}

	members := cfg.Etcd.Members
	founded := false
	for _, m := range members {
		founded = founded || st.Founded[m]
	}
	if len(members) > 0 && !founded {
		for _, m := range members {
			if cfg.Sites[m].Has(config.RoleWitness) {
				steps = append(steps, applyStep("founding", "a witness is founded first: nothing waits on it", m, true))
			}
		}
		for _, m := range members {
			if !cfg.Sites[m].Has(config.RoleWitness) {
				steps = append(steps, applyStep("founding", "founds its etcd member; it may stop until the other members run, and pass two resumes it", m, true))
			}
		}
	} else {
		for _, m := range members {
			if !st.Founded[m] {
				steps = append(steps, step("joining", "the cluster runs and this member has not joined it", false, "site", "add", m))
			}
		}
	}

	monitors := cfg.MonitorSites()
	ordered := func(include func(string) bool) []string {
		var first, last []string
		for _, s := range cfg.SiteNames() {
			switch {
			case !include(s):
			case slices.Contains(monitors, s):
				last = append(last, s)
			default:
				first = append(first, s)
			}
		}
		return append(first, last...)
	}
	for _, s := range ordered(func(s string) bool { return !slices.Contains(members, s) }) {
		steps = append(steps, applyStep("other sites", "not an etcd member; monitor sites last", s, false))
	}

	switch garage := cfg.Storage.Garage.Sites; len(garage) {
	case 0:
	case 1:
		steps = append(steps, step("storage", "Garage's layout, each app's bucket and key", false, "storage", "init", "--site", garage[0]))
	default:
		steps = append(steps, step("storage", "Garage's layout across its sites, each app's bucket and key", false, "storage", "add"))
	}

	for _, s := range ordered(func(string) bool { return true }) {
		steps = append(steps, applyStep("pass two", "resumes a founding stop and moves what earlier steps changed; an applied site plans nothing", s, false))
	}
	steps = append(steps, step("dns", "creates the records that are missing; one pointing elsewhere stops it", false, "dns", "init"))
	return steps
}
