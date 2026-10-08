package apply

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// VolumePrune is what `paisans prune` would do on one site: every dangling
// volume, and whether it goes.
//
// A dangling volume is one no container, running or stopped, mounts. On a
// site host that is almost always an anonymous volume a replaced container
// left behind: an image declared a VOLUME its compose file did not mount,
// and Docker made a new volume for every container. The apply time guard
// (VolumeCheck) stops new ones; this clears what accumulated before it, or
// what any other path left.
type VolumePrune struct {
	Site      string
	Transport string
	Volumes   []DanglingVolume
}

// DanglingVolume is one volume no container uses.
type DanglingVolume struct {
	Name string
	// Size is what `du -sb` reports for its mountpoint, in bytes.
	Size int64
	// Entries are its top level names, at most ten, so an operator can see
	// what it held (Mbin's leaked volumes hold cache/ and log/).
	Entries []string
	// Remove is the verdict, and Reason says why, either way.
	Remove bool
	Reason string
}

// Removals are the volumes the prune would remove.
func (p *VolumePrune) Removals() []DanglingVolume {
	var out []DanglingVolume
	for _, v := range p.Volumes {
		if v.Remove {
			out = append(out, v)
		}
	}
	return out
}

// PruneHeader is printed above the plan. It says what the verdicts rest on,
// because "anonymous and unused means ours" is an assumption about the host,
// and an operator should read it before agreeing to it.
const PruneHeader = "A host may carry several deployments and things that are no deployment's at all, so a volume is removed only when it carries this deployment's label, community.paisans.deployment with this deployment's id. Every other volume is kept: one labelled with another id is that deployment's, and one with no such label, anonymous ones included, cannot be attributed to any deployment. apply removes the anonymous volumes its own recreates abandon, read from its own containers before they are replaced."

// danglingProbe lists every dangling volume with its size, labels and top
// level entries, one tab separated line each, and ends with a marker so a
// cut short answer is not read as a short list. Volume names cannot contain
// whitespace (Docker allows [a-zA-Z0-9][a-zA-Z0-9_.-]), so the word split
// over the list is safe.
const danglingProbe = `set -e; vs=$(docker volume ls -qf dangling=true); for v in $vs; do m=$(docker volume inspect --format '{{.Mountpoint}}' "$v") || continue; l=$(docker volume inspect --format '{{json .Labels}}' "$v") || continue; s=$(du -sb "$m" 2>/dev/null | cut -f1); e=$(ls -A "$m" 2>/dev/null | head -n 10 | paste -sd ' ' -); printf 'volume\t%s\t%s\t%s\t%s\n' "$v" "${s:-0}" "$l" "$e"; done; echo end`

// composeProject is the label Compose puts on every volume it creates.
const composeProject = "com.docker.compose.project"

// anonymousName is the name Docker gives an anonymous volume.
var anonymousName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BuildVolumePrune reads a site's dangling volumes and decides each one for
// deployment d. It changes nothing.
func BuildVolumePrune(d deployment.Deployment, site string, t Transport) (*VolumePrune, error) {
	out, err := t.Run(danglingProbe)
	if err != nil {
		return nil, fmt.Errorf("%s: listing dangling volumes: %w\n%s", site, err, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "end") {
		return nil, fmt.Errorf("%s: listing dangling volumes: the answer was cut short:\n%s", site, out)
	}
	p := &VolumePrune{Site: site, Transport: t.Describe()}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 4 || fields[0] != "volume" {
			continue
		}
		v := DanglingVolume{Name: fields[1]}
		v.Size, _ = strconv.ParseInt(fields[2], 10, 64)
		var labels map[string]string
		if err := json.Unmarshal([]byte(fields[3]), &labels); err != nil {
			return nil, fmt.Errorf("%s: volume %s has unreadable labels %q", site, v.Name, fields[3])
		}
		if len(fields) > 4 && strings.TrimSpace(fields[4]) != "" {
			v.Entries = strings.Fields(fields[4])
		}
		v.Remove, v.Reason = pruneVerdict(d, labels)
		p.Volumes = append(p.Volumes, v)
	}
	return p, nil
}

// pruneVerdict decides one dangling volume from its labels. Ownership is the
// deployment label and nothing else: a name or a compose project name is
// chosen by whoever created the volume, and two deployments on one host
// share every naming convention.
func pruneVerdict(d deployment.Deployment, labels map[string]string) (bool, string) {
	switch owner, ok := labels[deployment.Label]; {
	case ok && owner == d.ID:
		return true, "labelled with this deployment's id"
	case ok:
		return false, "labelled with deployment " + owner + ", which is not this one"
	}
	if project, ok := labels[composeProject]; ok {
		return false, "labelled by compose project " + project + " but with no deployment id, so it is not this deployment's"
	}
	return false, "carries no deployment label, so no deployment can be shown to own it"
}

// ExecuteVolumePrune removes every volume the prune decided to. `docker
// volume rm` refuses a volume a container has started using since, which is
// the right answer, so a failure is collected and reported rather than
// stopping the rest.
func ExecuteVolumePrune(p *VolumePrune, t Transport) error {
	var failed []string
	for _, v := range p.Removals() {
		if out, err := t.Run("docker volume rm " + shellQuote(v.Name)); err != nil {
			failed = append(failed, v.Name+": "+firstLine(strings.TrimSpace(out)))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s: %d volume(s) were not removed:\n  %s", p.Site, len(failed), strings.Join(failed, "\n  "))
	}
	return nil
}
