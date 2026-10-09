package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcaddy"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// edgeStep is apply's work on the web server already in front of a monitor
// in ingress mode external: found and planned with the rest of the site,
// read only, and carried out after the site's stacks, once the operator has
// said yes on the terminal.
type edgeStep struct {
	site string
	// finding is the container holding 443/tcp, nil when none does.
	finding *hostcaddy.Finding
	// plan is the edit, nil when there is no Caddy to add to.
	plan  *hostcaddy.Plan
	sites []hostcaddy.Site
	// approved is --approve-external-proxy: yes, asked of nobody.
	approved bool
	// skipped is set when --only leaves out every app the step serves.
	skipped bool
	// held is why the step wrote nothing, for the end of the run, and
	// failed whether that makes the run fail: nobody could be asked.
	held   string
	failed bool
}

// edgeProbe is how many times, and how far apart, the check after a reload
// asks for each hostname. Tests shorten it.
var (
	edgeProbeAttempts = 6
	edgeProbeWait     = 10 * time.Second
	edgeSleep         = time.Sleep
)

// confirmOnTerminal asks a yes or no question on the controlling terminal,
// after writing what it is about. Only "y" or "yes" is yes. Tests replace
// it.
var confirmOnTerminal = func(show, question string) (bool, error) {
	tty, err := openTTY()
	if err != nil {
		return false, err
	}
	defer tty.Close()
	fmt.Fprint(tty, show)
	fmt.Fprintf(tty, "%s [y/N] ", question)
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// planEdge finds and reads the web server holding 443/tcp on a monitor in
// ingress mode external, and plans the site block for each app the site
// serves. It changes nothing. It also returns the render options the app
// needs to be reached by that server: joined to the server's network when
// the server runs on one of its own, and otherwise as the last apply left it
// when the server cannot be read as Caddy at all, so a server that is
// stopped for a moment does not take the app off its network.
func planEdge(cfg *config.Config, site string, report *hostcheck.Report, t apply.Transport, only []string, approved bool) (*edgeStep, []render.Option, error) {
	if !hostcaddy.Applies(cfg, site) {
		return nil, nil, nil
	}
	f, err := hostcaddy.Detect(report, t)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", site, err)
	}
	sites, err := hostcaddy.Wanted(cfg, site, f)
	if err != nil {
		return nil, nil, err
	}
	step := &edgeStep{site: site, finding: f, sites: sites, approved: approved}
	if len(only) > 0 {
		step.skipped = !slices.ContainsFunc(sites, func(s hostcaddy.Site) bool { return slices.Contains(only, s.App) })
	}

	var options []render.Option
	if proxy, ok := hostcaddy.Proxy(f); ok {
		for _, s := range sites {
			options = append(options, render.WithHostProxy(s.App, proxy))
		}
	} else if f == nil || f.Unsupported != "" {
		if options, err = apply.HostProxyOptions(cfg, map[string]apply.Transport{site: t}); err != nil {
			return nil, nil, err
		}
	}
	if f != nil {
		if step.plan, err = hostcaddy.PlanChange(f, cfg.Deployment(), sites, t); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", site, err)
		}
	}
	return step, options, nil
}

// pending reports whether the step would write anything.
func (e *edgeStep) pending() bool {
	return e != nil && !e.skipped && e.plan.Pending()
}

// print writes the step for the plan.
func (e *edgeStep) print(w io.Writer) {
	if e == nil {
		return
	}
	if e.finding == nil {
		fmt.Fprintf(w, "\nthe host's own web server (ingress mode external):\n")
		fmt.Fprintf(w, "  none      no container on this host holds 443/tcp, so there is no Caddy to add a site block to.\n")
		fmt.Fprintf(w, "            A web server installed on the host itself is configured by hand: `paisans ingress show --app <name>` has the block for it\n")
		return
	}
	e.plan.Print(w)
	if e.skipped && e.plan.Pending() {
		fmt.Fprintf(w, "  --only leaves out every app this serves, so this is not done in this run\n")
	}
}

// execute asks the operator, then writes, validates and reloads, and checks
// each hostname through the server. A run with nobody to ask writes nothing
// and says so; a no from the operator does the same. Either is reported with
// the held back apps at the end of the run.
func (e *edgeStep) execute(t apply.Transport, w io.Writer) error {
	if !e.pending() {
		return nil
	}
	f := e.finding
	question := fmt.Sprintf("Write this to %s and reload %s, a server this toolkit does not run?", e.plan.Path, f.Name())
	if !e.approved {
		if !terminalAvailable() {
			e.held = fmt.Sprintf("the site block was not added to %s's Caddy: there is no terminal to ask on. Run apply again on a terminal, or with --approve-external-proxy, or add it yourself", f.Container.Name)
			e.failed = true
			return nil
		}
		show := fmt.Sprintf("\n%s's Caddy is not this toolkit's. apply would change it as follows:\n\n%s\n", f.Container.Name, e.plan.Diff())
		yes, err := confirmOnTerminal(show, question)
		if err != nil {
			e.held = fmt.Sprintf("the site block was not added to %s's Caddy: reading the answer failed (%v)", f.Container.Name, err)
			e.failed = true
			return nil
		}
		if !yes {
			e.held = fmt.Sprintf("the site block was not added to %s's Caddy, because the answer was no. Add it yourself, or run apply again and answer yes", f.Container.Name)
			return nil
		}
	}
	fmt.Fprintf(w, "\nadding the site block to %s\n", f.Name())
	if err := hostcaddy.Execute(e.plan, t, now(), w); err != nil {
		return fmt.Errorf("%s: %w", e.site, err)
	}
	hostcaddy.Probe(e.plan, t, w, edgeProbeAttempts, edgeProbeWait, edgeSleep)
	return nil
}

// now is the clock a backup is named by; tests replace it.
var now = time.Now

// result reports what the step held back, beside the held back apps, and
// is an error when the run had nobody to ask: an unattended apply that left
// the monitor unserved must not read as a success.
func (e *edgeStep) result(w io.Writer) error {
	if e == nil || e.held == "" {
		return nil
	}
	fmt.Fprintf(w, "\nheld back from this apply:\n  %s:\n", e.site)
	for _, line := range strings.Split(strings.TrimRight(e.plan.Diff(), "\n"), "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
	fmt.Fprintf(w, "  %s\n", e.held)
	if e.failed {
		return fmt.Errorf("apply: the rest of %s was applied, but %s", e.site, e.held)
	}
	return nil
}
