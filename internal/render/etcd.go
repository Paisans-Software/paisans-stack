package render

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// EtcdInitialPath is where each etcd host records the flags its member was
// first started with, relative to a site's / in the rendered tree.
//
// etcd reads --initial-cluster and --initial-cluster-state only on a member's
// first start, when its data directory is empty, and ignores both forever
// after (etcd v3.5, "Clustering Guide", and server/etcdmain/etcd.go, which
// skips bootstrap when the member directory exists). So the flags are inert on
// a running member, and the only thing a change to them can do is make `apply`
// see a changed compose.yaml and recreate a healthy member for nothing. A
// cluster that grows would otherwise change every member's flags on every
// growth step.
//
// The record is a rendered file like any other: it is in the manifest, apply
// writes it, and it is read back from the host and handed to the next render,
// which renders the same flags again. It is not mounted into any container,
// so writing it is never a reason to act on the stack (see apply's
// isRecord).
func EtcdInitialPath(d deployment.Deployment) string { return d.RelPath("infra", "etcd-initial") }

// etcdCompactionRetention is how much key history etcd keeps, with
// --auto-compaction-mode=periodic (flag names and semantics from etcd v3.5.16,
// server/etcdmain/help.go). etcd keeps every revision of every key until it is
// compacted, and auto compaction is off by default. Patroni rewrites its
// leader key on every loop, so an uncompacted store grows without bound until
// it reaches the backend quota, raises NOSPACE and refuses writes, at which
// point Patroni cannot renew the leader lock and the primary demotes itself.
// An hour is far more history than anything here reads: Patroni watches
// current values, and nothing in the toolkit asks for an old revision.
const etcdCompactionRetention = "1h"

// etcdQuotaBackendBytes is etcd's own default, 2 GiB (DefaultQuotaBytes in
// v3.5.16's server/etcdserver/quota.go), made explicit. Patroni's state is a
// few kilobytes of keys, so with compaction the store stays far below either
// number. A smaller quota was rejected: compaction frees pages for reuse but
// the bbolt file never shrinks without a defrag, which nothing schedules, so a
// tight quota is a NOSPACE alarm waiting on a burst. Explicit rather than left
// at 0 so that a future etcd changing its default does not move it silently.
const etcdQuotaBackendBytes = 2 << 30

// EtcdInitial is how one etcd member was first started.
type EtcdInitial struct {
	// State is etcd's --initial-cluster-state: "new" for a member that
	// founded the cluster, "existing" for one admitted to a running one.
	State string
	// Cluster is --initial-cluster: name=peer-url pairs, comma separated.
	// For a founder it is the founding set. For a member that joined, it is
	// the membership as it stood right after that member was added, which is
	// what etcd requires: a joining member whose --initial-cluster lists a
	// different number of members than the running cluster has is refused
	// with "member count is unequal" (etcd v3.5.16,
	// server/etcdserver/api/membership/cluster.go,
	// ValidateClusterAndAssignIDs).
	Cluster string
}

// EtcdInitialState values.
const (
	EtcdStateNew      = "new"
	EtcdStateExisting = "existing"
)

// Option adjusts a render with facts read from the live deployment. Render
// itself reads nothing but the configuration and the secrets; a caller that
// has read a host passes what it found.
type Option func(*planner)

// WithEtcdInitial renders site's etcd member with the flags it was born with,
// rather than the ones the configuration would give a founding member today.
// A site with no such option renders as a founder of a cluster whose members
// are etcd.members, which is right for a fresh deployment and is what every
// member rendered before the record existed.
func WithEtcdInitial(site string, initial EtcdInitial) Option {
	return func(p *planner) {
		if p.etcdInitial == nil {
			p.etcdInitial = map[string]EtcdInitial{}
		}
		p.etcdInitial[site] = initial
	}
}

// etcdInitialFor is the record a site renders: the one it was handed, or a
// founder's.
func (p *planner) etcdInitialFor(site string) EtcdInitial {
	if in, ok := p.etcdInitial[site]; ok {
		return in
	}
	return EtcdInitial{State: EtcdStateNew, Cluster: p.etcdInitialCluster()}
}

// FormatEtcdInitial is the record's content.
func FormatEtcdInitial(in EtcdInitial) string {
	return fmt.Sprintf(`# Rendered by paisans. Do not edit: `+"`paisans apply`"+` overwrites this file.
#
# The flags this etcd member was first started with. etcd ignores both once
# its data directory exists, so they never change after the first start, and
# every later render repeats them from here rather than from etcd.members.
initial-cluster-state=%s
initial-cluster=%s
`, in.State, in.Cluster)
}

// ParseEtcdInitial reads a record. ok is false when content holds no complete
// record, so a caller falls back rather than rendering empty flags.
func ParseEtcdInitial(content string) (EtcdInitial, bool) {
	var in EtcdInitial
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "initial-cluster-state="); ok {
			in.State = v
		} else if v, ok := strings.CutPrefix(line, "initial-cluster="); ok {
			in.Cluster = v
		}
	}
	return in, in.State != "" && in.Cluster != ""
}

// ParseEtcdFlags reads the two flags from a compose file rendered before the
// record existed, so a member deployed then keeps exactly the flags it runs
// with. ok is false when the file has no etcd service.
func ParseEtcdFlags(compose string) (EtcdInitial, bool) {
	var in EtcdInitial
	for _, line := range strings.Split(compose, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if v, ok := strings.CutPrefix(line, "--initial-cluster-state="); ok {
			in.State = v
		} else if v, ok := strings.CutPrefix(line, "--initial-cluster="); ok {
			in.Cluster = v
		}
	}
	return in, in.State != "" && in.Cluster != ""
}

// EtcdPeerURL is a member's peer URL on the mesh, as every rendered flag
// names it.
func EtcdPeerURL(address string) string { return fmt.Sprintf("http://%s:%d", address, etcdPeerPort) }
