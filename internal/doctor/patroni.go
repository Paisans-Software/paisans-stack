package doctor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/patroni"
)

// maxLag is the replication lag past which a streaming replica is reported.
// 16 MiB is one WAL segment at Postgres's default size: a replica a whole
// segment behind is not keeping up, while a few kilobytes is a busy moment
// that is gone on the next look.
const maxLag = 16 << 20

// PatroniLogCommand is the evidence for a member that will not promote: the
// last lines of its Patroni log that speak of its health, lag, timeline or
// the leader lock. `|| true` because grep exits 1 when nothing matches, and
// "nothing matched" is an answer.
const PatroniLogCommand = "docker compose -f /srv/infra/compose.yaml logs --tail 200 patroni 2>&1 | grep -iE 'healthiest|lag|timeline|lock' | tail -n 5 || true"

// failoverCommand is the forced failover a human may choose to run. It is
// never run by doctor. The config path is the one the failover test uses,
// the file Spilo runs Patroni with (see failover.SwitchoverCommand).
func failoverCommand(candidate string) string {
	return "docker compose -f /srv/infra/compose.yaml exec patroni patronictl -c /home/postgres/postgres.yml failover --candidate " + candidate + " --force"
}

// PatroniProbe is what each database site's Patroni said.
type PatroniProbe struct {
	// Cluster is each reachable cluster site's /cluster answer, and
	// ClusterErr why a site's did not come.
	Cluster    map[string]string
	ClusterErr map[string]string
	// Sync is the /sync key's value, read from etcd, and SyncErr why it
	// could not be. Empty with no error means the key is absent.
	Sync    string
	SyncErr string
	// Logs is PatroniLogCommand's output per site, gathered only when there
	// is no leader.
	Logs map[string]string
}

// SyncState is Patroni's /sync key: the leader that wrote it and the members
// it waits for. sync_standby is a comma separated list, or null.
type SyncState struct {
	Leader      string  `json:"leader"`
	SyncStandby *string `json:"sync_standby"`
	Quorum      int     `json:"quorum"`
}

// ParseSync reads the /sync key. An empty value is an empty state.
func ParseSync(value string) (SyncState, error) {
	var s SyncState
	value = strings.TrimSpace(value)
	if value == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(value), &s); err != nil {
		return s, fmt.Errorf("unreadable /sync value %q", firstLine(value))
	}
	return s, nil
}

// Standbys is the members /sync names as synchronous standbys.
func (s SyncState) Standbys() []string {
	if s.SyncStandby == nil {
		return nil
	}
	var out []string
	for _, name := range strings.Split(*s.SyncStandby, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Names reports whether /sync names a member, as leader or standby, the way
// Patroni asks it: SyncState.matches(name, check_leader=True), compared
// without case, and false for an empty state (Patroni v4.1.0,
// patroni/dcs/__init__.py).
func (s SyncState) Names(member string) bool {
	if s.Leader == "" {
		return false
	}
	for _, name := range append(s.Standbys(), s.Leader) {
		if strings.EqualFold(name, member) {
			return true
		}
	}
	return false
}

// HasLeader reports whether any answer names a running leader, which is how
// cmd/paisans decides whether to gather the log evidence.
func HasLeader(answers map[string]string) bool {
	for _, out := range answers {
		if c, err := patroni.Parse(out); err == nil {
			if _, ok := c.Leader(); ok {
				return true
			}
		}
	}
	return false
}

// LeaderName is the running leader any answer names, empty when none does.
func LeaderName(answers map[string]string) string {
	for _, out := range answers {
		if c, err := patroni.Parse(out); err == nil {
			if m, ok := c.Leader(); ok {
				return m.Name
			}
		}
	}
	return ""
}

// Patroni reports the leader, each replica, and, when there is no leader,
// why, and what to do about it.
func Patroni(cfg *config.Config, in Input) []Finding {
	if len(cfg.Cluster.Sites) == 0 {
		return nil
	}
	reached, asked := in.Reached(), in.Asked()
	var askable []string
	for _, name := range cfg.Cluster.Sites {
		if reached[name] {
			askable = append(askable, name)
		}
	}
	if len(askable) == 0 {
		var why []string
		for _, name := range cfg.Cluster.Sites {
			if !asked[name] {
				why = append(why, name+": not asked (outside --site)")
			} else {
				why = append(why, name+": did not answer ssh (see reach)")
			}
		}
		return []Finding{{Section: SectionPatroni, Level: Fail, Line: "no database site reached, so the cluster could not be read", More: why}}
	}

	c, from, problems := firstCluster(in.Patroni, askable)
	if from == "" {
		return []Finding{{Section: SectionPatroni, Level: Fail, Line: "Patroni did not answer on any database site",
			More: append(problems, "Its container may be stopped or restarting: see the containers section, and `docker compose -f /srv/infra/compose.yaml logs patroni` on each site.")}}
	}
	sync, syncErr := ParseSync(in.Patroni.Sync)
	if in.Patroni.SyncErr != "" {
		syncErr = fmt.Errorf("%s", firstLine(in.Patroni.SyncErr))
	}

	if leader, ok := c.Leader(); ok {
		return withLeader(cfg, in, c, leader, from)
	}
	return noLeader(cfg, in, c, sync, syncErr)
}

// firstCluster is the first /cluster that parsed. Any member's API
// describes the whole cluster.
func firstCluster(p PatroniProbe, sites []string) (patroni.Cluster, string, []string) {
	var problems []string
	for _, name := range sites {
		out, ok := p.Cluster[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s", name, firstLine(p.ClusterErr[name])))
			continue
		}
		c, err := patroni.Parse(out)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		return c, name, problems
	}
	return patroni.Cluster{}, "", problems
}

func withLeader(cfg *config.Config, in Input, c patroni.Cluster, leader patroni.Member, from string) []Finding {
	out := []Finding{{Section: SectionPatroni, Level: OK, Line: fmt.Sprintf("leader: %s, running on timeline %d (asked %s)", leader.Name, leader.Timeline, from)}}
	reached := in.Reached()
	for _, name := range cfg.Cluster.Sites {
		if name == leader.Name {
			continue
		}
		m, ok := c.Member(name)
		if !ok {
			why := "its Patroni is not running or not registered: see the containers section"
			if !reached[name] {
				why = "its site did not answer ssh: see reach"
			}
			out = append(out, Finding{Section: SectionPatroni, Level: Warn, Line: fmt.Sprintf("%s: not a cluster member, so %s has no replica there; %s", name, leader.Name, why)})
			continue
		}
		line := fmt.Sprintf("%s: %s, %s, lag %s", m.Name, m.Role, m.State, m.LagText())
		lag, known := m.LagBytes()
		switch {
		case m.State != "streaming" && m.State != "running":
			out = append(out, Finding{Section: SectionPatroni, Level: Warn, Line: line + ": not streaming from the leader",
				More: []string{fmt.Sprintf("Read `docker compose -f /srv/infra/compose.yaml logs patroni` on %s.", m.Name)}})
		case !known || lag > maxLag:
			out = append(out, Finding{Section: SectionPatroni, Level: Warn, Line: line + ": more than 16 MiB behind, or unknown",
				More: []string{"A replica this far behind may lose writes if it is promoted now, and Patroni will not promote it past maximum_lag_on_failover."}})
		default:
			out = append(out, Finding{Section: SectionPatroni, Level: OK, Line: line})
		}
	}
	if cfg.Cluster.Synchronous && len(cfg.Cluster.Sites) > 1 && !c.HasSyncStandby() {
		more := []string{
			fmt.Sprintf("%s commits without waiting for any replica. While it does, a replica is not named in /sync, and Patroni will not promote it if %s fails (see the advice doctor prints for a cluster with no primary).", leader.Name, leader.Name),
			"Bring the other cluster members back to streaming; one becomes the Sync Standby again on its own.",
		}
		out = append(out, Finding{Section: SectionPatroni, Level: Warn, Line: "no synchronous standby, although cluster.synchronous is true", More: more})
	}
	return out
}

func noLeader(cfg *config.Config, in Input, c patroni.Cluster, sync SyncState, syncErr error) []Finding {
	var replicas []patroni.Member
	for _, m := range c.Members {
		if m.State == "running" || m.State == "streaming" {
			replicas = append(replicas, m)
		}
	}
	if len(replicas) == 0 {
		var states []string
		for _, m := range c.Members {
			states = append(states, fmt.Sprintf("%s %s %s", m.Name, m.Role, m.State))
		}
		more := []string{"No member is running Postgres, so none can be promoted."}
		if len(states) > 0 {
			more = append(more, "members: "+strings.Join(states, "; "))
		}
		more = append(more, "See the containers section, and `docker compose -f /srv/infra/compose.yaml logs patroni` on each database site.")
		return []Finding{{Section: SectionPatroni, Level: Fail, Line: "no primary, and no member running", More: more}}
	}

	// The stuck case: /sync was written by a leader that is gone, and names
	// none of the members that are left. Patroni v4.1.0, patroni/ha.py,
	// is_healthiest_node: while synchronous mode is active (the /sync key has
	// a leader), `if not self.is_quorum_commit_mode() and not
	// self.cluster.sync.matches(self.state_handler.name, True): return
	// False`, so such a member never races for the leader lock, and logs
	// "following a different leader because i am not the healthiest node".
	// That is Patroni refusing to lose data, because the old leader may have
	// committed alone after the member stopped being its standby.
	var stuck []patroni.Member
	if cfg.Cluster.Synchronous && syncErr == nil && sync.Leader != "" {
		if _, live := c.Member(sync.Leader); !live {
			for _, m := range replicas {
				if !sync.Names(m.Name) {
					stuck = append(stuck, m)
				}
			}
		}
	}
	if len(stuck) == len(replicas) {
		return []Finding{stuckFinding(cfg, in, sync, stuck)}
	}
	return []Finding{genericNoLeader(cfg, in, c, replicas, sync, syncErr)}
}

func names(members []patroni.Member) []string {
	var out []string
	for _, m := range members {
		out = append(out, m.Name)
	}
	return out
}

func stuckFinding(cfg *config.Config, in Input, sync SyncState, stuck []patroni.Member) Finding {
	x := sync.Leader
	r := stuck[0].Name
	all := strings.Join(names(stuck), " and ")
	verb := "is a replica"
	if len(stuck) > 1 {
		verb = "are replicas"
	}
	standby := "none"
	if s := sync.Standbys(); len(s) > 0 {
		standby = strings.Join(s, ", ")
	}

	more := []string{
		fmt.Sprintf("what happened: /sync names %s as the leader and %s as its synchronous standby, so %s %s not named.", x, standby, all, isAre(len(stuck))),
	}
	if !cfg.Cluster.SynchronousStrict {
		more = append(more, fmt.Sprintf("With cluster.synchronous_strict false, %s went on committing alone once %s stopped answering it, and took %s out of /sync. Then %s stopped too.", x, all, all, x))
	}
	more = append(more,
		fmt.Sprintf("While synchronous mode is active Patroni lets only a member /sync names race for the leader lock (Patroni v4.1.0, patroni/ha.py, is_healthiest_node), because %s may be missing commits %s made after %s stopped being its synchronous standby. So %s logs \"following a different leader because i am not the healthiest node\" and waits.", all, x, all, all),
		"recover:",
	)
	more = append(more, startAdvice(cfg, in, x, all)...)
	more = append(more,
		fmt.Sprintf("  2. Only if %s is lost for good: promote %s by hand. On %s%s:", x, r, r, via(in, r)),
		"       "+failoverCommand(r),
		"     Patroni promotes the candidate a failover names even when /sync does not (Patroni v4.1.0, patroni/ha.py, manual_failover_process_no_leader: `if failover.candidate == self.state_handler.name: return True`), and --force skips patronictl's \"Are you sure you want to failover to the asynchronous node\" (patroni/ctl.py).",
	)
	if len(stuck) > 1 {
		more = append(more, "     With more than one replica left, choose the one furthest ahead: the highest lsn in `patronictl list`.")
	}
	more = append(more, warnings(x, r)...)
	more = append(more, evidence(in, names(stuck))...)
	return Finding{Section: SectionPatroni, Level: Fail,
		Line: fmt.Sprintf("no primary: %s %s and will not promote while %s, which led, is gone", all, verb, x),
		More: more}
}

func isAre(n int) string {
	if n > 1 {
		return "are"
	}
	return "is"
}

// via names the host a command runs on, when it is known.
func via(in Input, site string) string {
	if d := in.destination(site); d != "" {
		return " (ssh " + d + ")"
	}
	return ""
}

// startAdvice is the first, lossless recovery: bring the old leader back.
func startAdvice(cfg *config.Config, in Input, x, all string) []string {
	how := fmt.Sprintf("Start %s", x)
	switch _, declared := cfg.Sites[x]; {
	case !declared:
		how = fmt.Sprintf("Start the Patroni member %s, which no site in this configuration is named", x)
	case in.Reached()[x]:
		how = fmt.Sprintf("Start %s's Patroni: its host answers ssh, but its Patroni is not in the cluster. `docker compose -f /srv/infra/compose.yaml up -d patroni` on %s%s starts it; the containers section says why it stopped", x, x, via(in, x))
	case in.destination(x) != "":
		how = fmt.Sprintf("Start %s (ssh %s)", x, in.destination(x))
	}
	return []string{
		fmt.Sprintf("  1. %s. When it returns it takes the leader lock again, and %s from it. No data is lost. This is the recovery to choose whenever %s can come back.", how, catchUp(all), x),
	}
}

func catchUp(all string) string {
	if strings.Contains(all, " and ") {
		return all + " catch up"
	}
	return all + " catches up"
}

// warnings is the block every forced failover is printed with.
func warnings(old, candidate string) []string {
	return []string{
		fmt.Sprintf("     WARNING: every commit %s made after %s stopped being its synchronous standby is lost: sign ins, posts, the metadata of uploads.", old, candidate),
		fmt.Sprintf("     WARNING: if %s returns later its data has diverged, and Patroni must rewind or reinitialise it, which discards those commits for good.", old),
		"     WARNING: this changes member data. It is a human's decision, and never something to run unattended.",
	}
}

func evidence(in Input, sites []string) []string {
	var out []string
	for _, site := range sites {
		lines := lastLines(in.Patroni.Logs[site], 5)
		if len(lines) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("evidence, %s's Patroni log:", site))
		for _, line := range lines {
			out = append(out, "  "+line)
		}
	}
	return out
}

func genericNoLeader(cfg *config.Config, in Input, c patroni.Cluster, replicas []patroni.Member, sync SyncState, syncErr error) Finding {
	var missing []string
	for _, name := range cfg.Cluster.Sites {
		if _, ok := c.Member(name); !ok {
			missing = append(missing, name)
		}
	}
	var states []string
	for _, m := range replicas {
		states = append(states, fmt.Sprintf("%s %s %s, lag %s", m.Name, m.Role, m.State, m.LagText()))
	}
	more := []string{
		"what happened: a replica promotes only when Patroni judges it the healthiest node. It will not when it is further behind the last known primary position than maximum_lag_on_failover (Patroni v4.1.0, patroni/ha.py, is_lagging), when its watchdog is unusable, when etcd has no quorum (see the etcd section), or when synchronous mode is active and /sync does not name it.",
		"members: " + strings.Join(states, "; "),
	}
	switch {
	case syncErr != nil:
		more = append(more, fmt.Sprintf("/sync could not be read: %v", syncErr))
	case sync.Leader != "":
		standby := "none"
		if s := sync.Standbys(); len(s) > 0 {
			standby = strings.Join(s, ", ")
		}
		more = append(more, fmt.Sprintf("/sync: leader %s, synchronous standby %s", sync.Leader, standby))
	}
	more = append(more, "recover:")
	if len(missing) > 0 {
		var hosts []string
		for _, name := range missing {
			hosts = append(hosts, name+via(in, name))
		}
		more = append(more, fmt.Sprintf("  1. Start the members that are not in the cluster: %s. A replica that is behind catches up from a returning leader, and no data is lost.", strings.Join(hosts, ", ")))
	} else {
		more = append(more, "  1. Read the evidence below, and `docker compose -f /srv/infra/compose.yaml logs patroni` on each database site, for why each replica holds back.")
	}
	r := replicas[0].Name
	old := "the old primary"
	if sync.Leader != "" {
		old = sync.Leader
	}
	more = append(more,
		fmt.Sprintf("  2. Only if %s is lost for good and none of the above helps: promote %s by hand. On %s%s:", old, r, r, via(in, r)),
		"       "+failoverCommand(r),
	)
	more = append(more, warnings(old, r)...)
	more = append(more, evidence(in, names(replicas))...)
	return Finding{Section: SectionPatroni, Level: Fail,
		Line: fmt.Sprintf("no primary: no member holds the leader lock (%s)", strings.Join(names(replicas), ", ")),
		More: more}
}
