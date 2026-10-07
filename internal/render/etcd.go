package render

import (
	"fmt"
	"strings"
)

// EtcdInitialPath is where each etcd host records the flags its member was
// first started with, relative to a site's root in the rendered tree.
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
const EtcdInitialPath = "srv/infra/etcd-initial"

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
func EtcdPeerURL(address string) string { return fmt.Sprintf("http://%s:2380", address) }
