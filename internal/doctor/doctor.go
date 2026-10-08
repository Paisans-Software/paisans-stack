// Package doctor is `paisans doctor`: it reads a running deployment and says
// what is stuck, why, and how to recover, one line per finding.
//
// It never changes anything. There is no --execute: every command it sends a
// host is a read (a list, a status, a log tail), and the recovery it suggests
// is printed for a human to run. That is deliberate, because doctor is run in
// the middle of an outage, and the one recovery it describes that touches
// member data (a forced failover) loses commits. A tool that did that on its
// own would be choosing which data a community keeps.
//
// The package holds no transport. cmd/paisans runs the commands named here on
// each site and hands the outputs to Diagnose, so every judgement below is a
// pure function of what the hosts said, and is tested with what real hosts
// said.
package doctor

import (
	"fmt"
	"io"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// Level is how bad a finding is.
type Level int

const (
	// OK is a check that passed.
	OK Level = iota
	// Skip is a check that could not be made and says why. It is not a
	// failure: nothing was found wrong, and nothing was found right either.
	Skip
	// Warn is a risk the operator should know about, not an outage.
	Warn
	// Fail is something broken now. Any Fail makes doctor exit non zero.
	Fail
)

func (l Level) String() string {
	switch l {
	case Skip:
		return "skip"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	}
	return "ok"
}

// Sections, in the order they are printed. Reach is first because every
// other check runs only on the sites it found; the rest go from the bottom of
// the stack up, so the first failure printed is usually the cause of the
// ones after it.
const (
	SectionReach      = "reach"
	SectionEtcd       = "etcd"
	SectionPatroni    = "patroni"
	SectionContainers = "containers"
	SectionPocketID   = "pocket-id"
	SectionClocks     = "clocks"
)

var sections = []string{SectionReach, SectionEtcd, SectionPatroni, SectionContainers, SectionPocketID, SectionClocks}

// Finding is one line of the report, with any explanation, recovery advice or
// evidence under it.
type Finding struct {
	Section string
	Level   Level
	Line    string
	More    []string
}

// Report is every finding, and how many sites were asked and answered.
type Report struct {
	Sites    int
	Reached  int
	Findings []Finding
}

// Failed reports whether any finding is a Fail.
func (r Report) Failed() bool { return r.Count(Fail) > 0 }

// Count is the number of findings at a level.
func (r Report) Count(l Level) int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == l {
			n++
		}
	}
	return n
}

// Print writes the report: a header, each section with its findings, and the
// promise that nothing was changed, which is also true when doctor fails.
func (r Report) Print(w io.Writer) {
	fmt.Fprintf(w, "doctor: %d site(s), %d reached\n", r.Sites, r.Reached)
	for _, section := range sections {
		var printed bool
		for _, f := range r.Findings {
			if f.Section != section {
				continue
			}
			if !printed {
				fmt.Fprintln(w, section)
				printed = true
			}
			fmt.Fprintf(w, "  %-4s  %s\n", f.Level, f.Line)
			for _, more := range f.More {
				fmt.Fprintf(w, "        %s\n", more)
			}
		}
	}
	fmt.Fprintln(w, "Changes nothing.")
}

// Input is everything the hosts said, gathered by cmd/paisans.
type Input struct {
	// Sites are the sites asked, in order: every declared site, or the ones
	// --site named.
	Sites      []string
	Reach      []SiteReach
	Etcd       EtcdProbe
	Patroni    PatroniProbe
	Containers []SiteContainers
	Standby    []StandbyProbe
	Clocks     []ClockSample
}

// Reached is the set of sites that answered ssh, and can be asked the rest.
func (in Input) Reached() map[string]bool {
	out := map[string]bool{}
	for _, r := range in.Reach {
		if r.Answered() {
			out[r.Site] = true
		}
	}
	return out
}

// Asked is the set of sites doctor was told to look at.
func (in Input) Asked() map[string]bool {
	out := map[string]bool{}
	for _, s := range in.Sites {
		out[s] = true
	}
	return out
}

// destination is how a site was reached, for advice that names a host.
func (in Input) destination(site string) string {
	for _, r := range in.Reach {
		if r.Site == site {
			return r.Destination
		}
	}
	return ""
}

// Diagnose turns what the hosts said into a report.
func Diagnose(cfg *config.Config, in Input) Report {
	reached := in.Reached()
	var findings []Finding
	findings = append(findings, Reach(cfg, in.Reach)...)
	findings = append(findings, Etcd(cfg, in)...)
	findings = append(findings, Patroni(cfg, in)...)
	findings = append(findings, Containers(in.Containers, LeaderName(in.Patroni.Cluster))...)
	// Only a cluster that answered can be said to have no primary.
	noPrimary := len(in.Patroni.Cluster) > 0 && !HasLeader(in.Patroni.Cluster)
	findings = append(findings, Standby(in.Standby, noPrimary)...)
	findings = append(findings, Clocks(in.Clocks)...)
	return Report{Sites: len(in.Sites), Reached: len(reached), Findings: findings}
}

// lastLines is the last n non empty lines of out.
func lastLines(out string, n int) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimRight(line, " \r\t"); strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// shellQuote wraps a value in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
