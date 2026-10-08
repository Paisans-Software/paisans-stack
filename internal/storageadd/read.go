package storageadd

// The readers of Garage's own output, for `site remove`, which takes a node
// out of the layout this package builds and so reads the same answers.

// LayoutZones reads `garage layout show`: the current layout version, and
// each node with a role by its short ID, with its zone. `storage add` names
// a node's zone after its site.
func LayoutZones(out string) (version int, zones map[string]string, err error) {
	l, err := parseLayout(out)
	if err != nil {
		return 0, nil, err
	}
	return l.version, l.rows, nil
}

// HealthyNodes reads `garage status`: the short IDs under its healthy
// heading, and whether a failed heading follows.
func HealthyNodes(out string) (healthy map[string]bool, failed bool) { return parseStatus(out) }

// ResyncQueue reads `garage stats`: the block resync queue's length, and how
// many blocks have resync errors.
func ResyncQueue(out string) (queue, errs int, err error) { return parseResync(out) }

// StableLayout is the line `garage layout history` prints once every node
// has synced the newest layout and no metadata is moving.
const StableLayout = stableLayout
