// Package patroni reads a Patroni cluster's state the way every command that
// reaches one does: through the Spilo container on a data site, never from a
// client installed on the host or the workstation.
//
// It holds no policy. preflight asks it who leads and how big the data is;
// failover asks who leads, who streams and how far behind.
package patroni

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Exec is how every Patroni command reaches the container: the
// infrastructure stack's compose file, which is where apply put it.
const Exec = "docker compose -f /srv/infra/compose.yaml exec -T patroni"

// ClusterCommand asks the Patroni REST API at api (host:port, on the mesh)
// for GET /cluster. curl runs inside the Spilo container, which installs it
// (zalando/spilo, postgres-appliance/build_scripts/prepare.sh), so the host
// needs nothing beyond Docker.
func ClusterCommand(api string) string {
	return fmt.Sprintf("%s curl -s --max-time 5 http://%s/cluster", Exec, api)
}

// Member is one entry of /cluster's members list. The keys and their values
// are Patroni v4.1.0's (patroni/utils.py, cluster_as_json): role is leader,
// standby_leader, sync_standby, quorum_standby or replica; state is the
// replication state for a member that is not the leader (streaming) and
// Postgres's state otherwise (running); lag is a byte count, or the string
// "unknown" when the member reported no position, and is absent for the
// leader.
type Member struct {
	Name  string          `json:"name"`
	Role  string          `json:"role"`
	State string          `json:"state"`
	Lag   json.RawMessage `json:"lag"`
}

// LagBytes is the member's replication lag in bytes, and false when Patroni
// reported none it could compute.
func (m Member) LagBytes() (int64, bool) {
	raw := strings.TrimSpace(string(m.Lag))
	if raw == "" || raw == "null" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// LagText is the lag as an operator would read it.
func (m Member) LagText() string {
	if n, ok := m.LagBytes(); ok {
		return fmt.Sprintf("%d bytes", n)
	}
	return "unknown"
}

// Cluster is the part of /cluster this toolkit reads.
type Cluster struct {
	Members []Member `json:"members"`
}

// Parse reads a /cluster document.
func Parse(document string) (Cluster, error) {
	var c Cluster
	if err := json.Unmarshal([]byte(strings.TrimSpace(document)), &c); err != nil {
		return Cluster{}, fmt.Errorf("unreadable /cluster document: %w", err)
	}
	return c, nil
}

// Leader returns the member holding the leader key while running, and false
// when there is none yet.
func (c Cluster) Leader() (Member, bool) {
	for _, m := range c.Members {
		if m.Role == "leader" && m.State == "running" {
			return m, true
		}
	}
	return Member{}, false
}

// Member returns the named member.
func (c Cluster) Member(name string) (Member, bool) {
	for _, m := range c.Members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// HasSyncStandby reports whether any member is a Sync Standby.
func (c Cluster) HasSyncStandby() bool {
	for _, m := range c.Members {
		if m.Role == "sync_standby" {
			return true
		}
	}
	return false
}

// DatabaseSizeCommand sums every database's size on the member it runs on,
// in bytes. It goes over the container's own unix socket as the superuser,
// which Spilo's pg_hba trusts locally (configure_spilo.py, `local all all
// trust`), so no password is involved. -A -t prints the bare number.
const DatabaseSizeCommand = Exec + ` psql -X -A -t -U postgres -d postgres -c 'SELECT sum(pg_database_size(datname))::bigint FROM pg_database'`

// ParseSize reads DatabaseSizeCommand's output.
func ParseSize(out string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unreadable database size %q", strings.TrimSpace(out))
	}
	return n, nil
}
