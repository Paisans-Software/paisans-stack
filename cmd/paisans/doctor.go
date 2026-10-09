package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/doctor"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/mesh"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/patroni"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// doctorTransport is how doctor reaches a site. Tests replace it.
var doctorTransport = func(t apply.SSHTransport) apply.Transport { return t }

// doctorNow is the workstation's clock, for the clock check. Tests replace it.
var doctorNow = time.Now

// doctorConnectTimeout bounds each ssh connection attempt, in seconds. doctor
// is run when something is down, and a host that is switched off would
// otherwise cost the operating system's TCP timeout on each of ssh's three
// attempts before the report could be printed.
const doctorConnectTimeout = 10

// runDoctor reads every site, or the ones --site names, and prints what is
// stuck and how to recover. It changes nothing and has no --execute: see
// internal/doctor for why. A site that does not answer is a finding, and
// every other check runs with the sites that did. It exits non zero when any
// finding is FAIL, so a script can tell a healthy deployment from one that
// needs a human.
//
// It takes no --ssh, like preflight: it reaches several sites, and one
// override cannot name them all.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	var only pathList
	fs.Var(&only, "site", "look at this site only (repeatable); every check then runs on the named sites alone")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since Docker and /srv are root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	_ = r
	if fs.NArg() > 0 {
		return fmt.Errorf("doctor takes flags only. Got extra argument(s): %s", strings.Join(fs.Args(), " "))
	}
	cfg, err := loadChecked(*configPath)
	if err != nil {
		return err
	}
	sites := cfg.SiteNames()
	if len(only) > 0 {
		for _, name := range only {
			if _, ok := cfg.Sites[name]; !ok {
				return fmt.Errorf("doctor: %s declares no site %q. Declared sites are %s", *configPath, name, strings.Join(cfg.SiteNames(), ", "))
			}
		}
		sites = only
	}
	transports := map[string]apply.Transport{}
	for _, name := range sites {
		t := siteTransport(name, cfg.Sites[name], "", *sudo)
		t.ConnectTimeout = doctorConnectTimeout
		transports[name] = doctorTransport(t)
	}

	report := doctor.Diagnose(cfg, gatherDoctor(cfg, sites, transports))
	fmt.Fprintln(os.Stdout)
	report.Print(os.Stdout)
	if report.Failed() {
		return fmt.Errorf("doctor: %d finding(s) marked FAIL", report.Count(doctor.Fail))
	}
	return nil
}

// gatherDoctor runs every read doctor makes and records each answer. Every
// command here is one of internal/doctor's read commands, patroni's
// ClusterCommand, apply.LookAtInstances' question, or the host check's
// inventory (hostcheck.Inspect); none writes.
func gatherDoctor(cfg *config.Config, sites []string, transports map[string]apply.Transport) doctor.Input {
	in := doctor.Input{Sites: sites}
	reached := map[string]bool{}

	for _, name := range sites {
		t := transports[name]
		r := doctor.SiteReach{Site: name, Destination: t.Describe()}
		if out, err := t.Run(doctor.ReachCommand); err != nil {
			r.Err = errText(out, err)
			r.Sudo = errors.Is(err, apply.ErrSudo)
		} else {
			reached[name] = true
		}
		in.Reach = append(in.Reach, r)
	}

	for _, name := range sites {
		if !reached[name] {
			continue
		}
		t := transports[name]
		p := doctor.MeshProbe{Site: name}
		if out, err := t.Run(mesh.LinkCommand(cfg.Deployment().Interface())); err != nil {
			p.LinkErr = errText(out, err)
		} else {
			p.Link = out
		}
		if probed, err := mesh.Probe(t); err != nil {
			p.ProbeErr = err.Error()
		} else {
			p.Probed = probed
		}
		in.Mesh = append(in.Mesh, p)
	}

	// etcd, from the first member that answers it, which also reads /sync.
	var etcdSite string
	for _, name := range cfg.Etcd.Members {
		if !reached[name] {
			continue
		}
		out, err := transports[name].Run(doctor.EtcdHealthCommand(cfg))
		if _, ok := doctor.ParseEtcdHealth(out); !ok {
			why := errText(out, err)
			if why == "" {
				why = "unreadable answer: " + firstOf(out)
			}
			in.Etcd.Tried = append(in.Etcd.Tried, fmt.Sprintf("%s: %s", name, why))
			continue
		}
		etcdSite = name
		in.Etcd.Site, in.Etcd.Health = name, out
		if out, err := transports[name].Run(doctor.EtcdVersionCommand); err != nil {
			in.Etcd.VersionErr = errText(out, err)
		} else {
			in.Etcd.Version = out
		}
		break
	}

	in.Patroni = doctor.PatroniProbe{Cluster: map[string]string{}, ClusterErr: map[string]string{}, Logs: map[string]string{}}
	if etcdSite != "" && len(cfg.Cluster.Sites) > 0 {
		if out, err := transports[etcdSite].Run(doctor.SyncCommand(cfg.Deployment())); err != nil {
			in.Patroni.SyncErr = errText(out, err)
		} else {
			in.Patroni.Sync = out
		}
	} else if len(cfg.Cluster.Sites) > 0 {
		in.Patroni.SyncErr = "no etcd member answered"
	}
	for _, name := range cfg.Cluster.Sites {
		if !reached[name] {
			continue
		}
		api := fmt.Sprintf("%s:%d", cfg.Sites[name].Address, render.PatroniAPIPort)
		if out, err := transports[name].Run(patroni.ClusterCommand(cfg.Deployment(), api)); err != nil {
			in.Patroni.ClusterErr[name] = errText(out, err)
		} else {
			in.Patroni.Cluster[name] = out
		}
	}
	if !doctor.HasLeader(in.Patroni.Cluster) {
		for _, name := range cfg.Cluster.Sites {
			if reached[name] {
				out, _ := transports[name].Run(doctor.PatroniLogCommand(cfg.Deployment()))
				in.Patroni.Logs[name] = out
			}
		}
	}

	for _, name := range sites {
		if !reached[name] {
			continue
		}
		in.Containers = append(in.Containers, gatherContainers(cfg.Deployment(), name, transports[name]))
	}

	// Pocket ID, asked only of the sites that answered: a site that did not
	// is listed as unreachable rather than asked again.
	answering := map[string]apply.Transport{}
	for name := range reached {
		answering[name] = transports[name]
	}
	for _, app := range apply.StandbyApps(cfg) {
		list := apply.LookAtInstances(cfg, app, answering)
		for i := range list {
			if list[i].State == apply.Unreachable && list[i].Detail == "no transport" {
				if _, asked := transports[list[i].Site]; asked {
					list[i].Detail = "did not answer ssh (see reach)"
				} else {
					list[i].Detail = "not asked (outside --site)"
				}
			}
		}
		in.Standby = append(in.Standby, doctor.StandbyProbe{App: app, Instances: list})
	}

	for _, name := range sites {
		if !reached[name] {
			continue
		}
		in.Leftovers = append(in.Leftovers, gatherLeftovers(cfg, name, transports[name]))
	}

	for _, name := range sites {
		if !reached[name] {
			continue
		}
		before := doctorNow()
		out, err := transports[name].Run(doctor.ClockCommand)
		after := doctorNow()
		sample := doctor.ClockSample{Site: name, Before: before, After: after, Out: out}
		if err != nil {
			sample.Err = errText(out, err)
		}
		in.Clocks = append(in.Clocks, sample)
	}
	return in
}

// gatherLeftovers takes one site's inventory with the host check's probes,
// all reads, and classifies what is left over and what relies on its Caddy.
func gatherLeftovers(cfg *config.Config, site string, t apply.Transport) doctor.LeftoverProbe {
	p := doctor.LeftoverProbe{Site: site}
	inv, err := hostcheck.Inspect(t, cfg.Deployment())
	if err != nil {
		p.Err = firstOf(err.Error())
		return p
	}
	if p.Report, err = ownership.Classify(cfg, site, inv, inv.ManifestFiles); err != nil {
		p.Err = err.Error()
	}
	return p
}

// gatherContainers lists one site's containers and looks closer at each one
// that is not running.
func gatherContainers(d deployment.Deployment, site string, t apply.Transport) doctor.SiteContainers {
	s := doctor.SiteContainers{Site: site}
	out, err := t.Run(doctor.ContainersCommand(d))
	if err != nil {
		s.PSErr = errText(out, err)
		return s
	}
	s.PS = out
	list, err := doctor.ParsePS(out)
	if err != nil {
		return s
	}
	for _, e := range list {
		if !e.Down() {
			continue
		}
		p := doctor.ContainerProbe{Entry: e}
		if out, err := t.Run(doctor.InspectCommand(e.Names)); err != nil {
			p.InspectErr = errText(out, err)
		} else {
			p.Inspect = out
		}
		p.Logs, _ = t.Run(doctor.LogCommand(e.Names))
		s.Down = append(s.Down, p)
	}
	return s
}

// errText is why a command failed: the last line it printed, which for a
// connection that never opened is ssh's own error and otherwise is usually
// the remote command's, or the error itself when nothing was printed.
func errText(out string, err error) string {
	if err == nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return firstOf(err.Error())
}

func firstOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
