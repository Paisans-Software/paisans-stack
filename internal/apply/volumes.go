package apply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// VolumeCheck is the guard against image declared volumes nothing mounts.
//
// An image's VOLUME is a path Docker gives every new container an anonymous
// volume for, unless the container mounts something at exactly that path.
// Compose replaces a container on every recreate, the new one gets a new
// volume, and the old one is left behind with no label saying whose it was.
// A real apps site collected 18 that way under Mbin's /app/var/, about 830 MB.
//
// So every image of every stack this apply acts on is inspected, and a
// declared path the service's rendered compose file does not mount (a bind,
// a named volume or a tmpfs, at exactly that path) refuses the apply before
// anything is written. Build inspects the images the host has; the ones it
// does not are owed, and Execute pulls and inspects them before its first
// write. kinds.ImageVolumes is the same check made at test time for the
// images the toolkit ships; this one is what covers an operator's own
// `images` override, and an image whose tag moved.
type VolumeCheck struct {
	// Checked are the references inspected so far.
	Checked []string
	// Owed are references the host does not have yet, which Execute pulls and
	// inspects before writing anything.
	Owed []string
	// Uncovered is every declared path nothing mounts. Any entry refuses.
	Uncovered []UncoveredVolume

	// services is each acted on stack's services, for Execute's owed check.
	services map[string]map[string]kinds.ServiceMounts
}

// UncoveredVolume is one declared VOLUME path that one service does not mount.
type UncoveredVolume struct {
	Stack   string
	Service string
	Image   string
	Path    string
}

// Describe is the refusal's line for it, with the fix.
func (u UncoveredVolume) Describe() string {
	return fmt.Sprintf("%s/%s runs %s, which declares VOLUME %s, and the compose file mounts nothing at that path. Docker would give every new container an anonymous volume there and abandon it on the next recreate. Mount it in the kind's compose template: a bind under the stack's own directory for state, a tmpfs for throwaway",
		u.Stack, u.Service, u.Image, u.Path)
}

// volumeProbe asks the host what each image declares, one line per image. It
// prints rather than exits non zero for an image it does not have, so that
// cannot be confused with ssh failing. printf rather than echo for the JSON,
// since dash's echo would read a backslash in it as an escape.
func volumeProbe(refs []string) string {
	quoted := make([]string, len(refs))
	for i, ref := range refs {
		quoted[i] = shellQuote(ref)
	}
	return fmt.Sprintf(`for r in %s; do if v=$(docker image inspect --format '{{json .Config.Volumes}}' "$r" 2>/dev/null); then printf 'volumes %%s %%s\n' "$r" "$v"; else echo "absent $r"; fi; done`,
		strings.Join(quoted, " "))
}

// probeVolumes returns each reference's declared volume paths. Every
// reference must be answered for, and present: Build asks only about images
// the image probe found, and Execute only after pulling.
func probeVolumes(refs []string, t Transport) (map[string][]string, error) {
	out := map[string][]string{}
	if len(refs) == 0 {
		return out, nil
	}
	text, err := t.Run(volumeProbe(refs))
	if err != nil {
		return nil, fmt.Errorf("asking which volumes the images declare: %w", err)
	}
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "volumes ")
		if !ok {
			continue
		}
		ref, raw, _ := strings.Cut(rest, " ")
		var declared map[string]struct{}
		if err := json.Unmarshal([]byte(raw), &declared); err != nil {
			return nil, fmt.Errorf("asking which volumes %s declares: unreadable answer %q", ref, raw)
		}
		paths := []string{}
		for p := range declared {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		out[ref] = paths
	}
	for _, ref := range refs {
		if _, ok := out[ref]; !ok {
			return nil, fmt.Errorf("asking which volumes the images declare: no answer for %s in:\n%s", ref, text)
		}
	}
	return out, nil
}

// uncoveredVolumes compares what each image declares with what each service
// mounts, for the images in declared. A service whose image is not in it is
// skipped, which is how Build leaves the owed images to Execute.
func uncoveredVolumes(services map[string]map[string]kinds.ServiceMounts, declared map[string][]string) []UncoveredVolume {
	var out []UncoveredVolume
	for _, stack := range sortedStacks(services) {
		names := make([]string, 0, len(services[stack]))
		for name := range services[stack] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			s := services[stack][name]
			paths, ok := declared[s.Image]
			if !ok {
				continue
			}
			for _, p := range kinds.Uncovered(paths, s.Targets) {
				out = append(out, UncoveredVolume{Stack: stack, Service: name, Image: s.Image, Path: p})
			}
		}
	}
	return out
}

func sortedStacks(m map[string]map[string]kinds.ServiceMounts) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stackServices reads the services of each named stack from its rendered
// compose file.
func stackServices(changes []Change, stacks map[string]bool) (map[string]map[string]kinds.ServiceMounts, error) {
	out := map[string]map[string]kinds.ServiceMounts{}
	for _, c := range changes {
		if !stacks[c.Stack] || !isStackCompose(c) {
			continue
		}
		services, err := kinds.ComposeMounts(c.content)
		if err != nil {
			return nil, fmt.Errorf("reading the mounts %s renders: %w", c.Path, err)
		}
		out[c.Stack] = services
	}
	return out, nil
}

// probeVolumeCheck builds the plan's VolumeCheck from the image IDs the image
// probe found: present images are inspected now, absent ones are owed.
func (p *Plan) probeVolumeCheck(ids map[string]string, t Transport) error {
	acted, recreated := map[string]bool{}, map[string]bool{}
	for _, action := range p.Actions {
		acted[action.Stack] = true
		recreated[action.Stack] = recreated[action.Stack] || action.Recreate
	}
	services, err := stackServices(p.Changes, acted)
	if err != nil {
		return err
	}
	// An image the host lacks is owed only by a stack being recreated: a
	// restart creates no container, so it cannot make a volume, and pulling
	// for it would be a pull the disk check never weighed.
	checked, owed := map[string]bool{}, map[string]bool{}
	for stack, list := range services {
		for _, s := range list {
			switch {
			case s.Image == "":
			case ids[s.Image] != "":
				checked[s.Image] = true
			case recreated[stack]:
				owed[s.Image] = true
			}
		}
	}
	check := &VolumeCheck{services: services, Checked: sortedKeys(checked), Owed: sortedKeys(owed)}
	declared, err := probeVolumes(check.Checked, t)
	if err != nil {
		return err
	}
	check.Uncovered = uncoveredVolumes(services, declared)
	p.Volumes = check
	return nil
}

// settleOwedVolumes pulls the images Build could not inspect and inspects
// them, before anything is written. A pull here is one `up -d` would make a
// moment later anyway, and the disk check has already passed for it.
func settleOwedVolumes(plan *Plan, t Transport) error {
	v := plan.Volumes
	if v == nil || len(v.Owed) == 0 {
		return nil
	}
	for _, ref := range v.Owed {
		if out, err := t.Run("docker pull --quiet " + shellQuote(ref)); err != nil {
			return fmt.Errorf("%s: pulling %s to read the volumes it declares, so nothing was written and nothing was started:\n%s", plan.Site, ref, out)
		}
	}
	declared, err := probeVolumes(v.Owed, t)
	if err != nil {
		return fmt.Errorf("%s: %w", plan.Site, err)
	}
	v.Checked = append(v.Checked, v.Owed...)
	sort.Strings(v.Checked)
	v.Owed = nil
	v.Uncovered = append(v.Uncovered, uncoveredVolumes(v.services, declared)...)
	return nil
}

// volumeRefusal is Execute's error for any uncovered path.
func volumeRefusal(plan *Plan) error {
	lines := make([]string, len(plan.Volumes.Uncovered))
	for i, u := range plan.Volumes.Uncovered {
		lines[i] = u.Describe()
	}
	return fmt.Errorf("%s: %d declared image volume(s) are not mounted, so nothing was written and nothing was started:\n  %s",
		plan.Site, len(lines), strings.Join(lines, "\n  "))
}

// Summary is the check's result in a few words (Eg: 2 images checked).
func (v *VolumeCheck) Summary() string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d images checked", len(v.Checked))
}

// Describe is the plan's line for the check, shown as a step's detail with
// --verbose.
func (v *VolumeCheck) Describe() string {
	line := fmt.Sprintf("volumes: %d image(s) inspected", len(v.Checked))
	if len(v.Owed) > 0 {
		line += fmt.Sprintf(", %d pulled and inspected before anything is written (%s)", len(v.Owed), strings.Join(v.Owed, ", "))
	}
	if len(v.Uncovered) > 0 {
		return line + fmt.Sprintf(": %d declared path(s) not mounted, so the apply refuses", len(v.Uncovered))
	}
	return line + ": every declared path is mounted"
}
