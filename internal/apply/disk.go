package apply

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultMinFree is the free space an apply wants on Docker's data root before
// it acts on a stack that will pull an image. One Mbin application image is
// about 1.4 GiB, and an upgrade pulls the new one while the old one is still
// on disk, so 3 GiB leaves room for that and for the logs and database files
// that keep growing beside it.
const DefaultMinFree int64 = 3 << 30

// DiskCheck is the free space probe on a host about to pull images. It is
// read in Build, so a dry run shows the numbers, and enforced in Execute
// before anything is written.
type DiskCheck struct {
	// Dir is Docker's data root, where pulled layers land.
	Dir string
	// Free is what `df` reports available there, in bytes.
	Free int64
	// Need is the threshold: the default, or --min-free.
	Need int64
	// Pulls are the rendered images the host does not have, which the stack
	// actions will pull.
	Pulls []string
	// Reclaimable is `docker system df`'s summary, one line per type. It is
	// read only when Free is short of Need, to say where space could come from.
	Reclaimable []string
}

// Short reports whether the host has less free space than this apply needs.
func (d *DiskCheck) Short() bool { return d != nil && d.Free < d.Need }

// Describe is the plan's line for the check.
func (d *DiskCheck) Describe() string {
	verdict := "enough"
	if d.Short() {
		verdict = "too little, so the apply refuses before writing anything"
	}
	return fmt.Sprintf("disk: %s free on %s, %s required before pulling %d image(s) (%s): %s",
		FormatSize(d.Free), d.Dir, FormatSize(d.Need), len(d.Pulls), strings.Join(d.Pulls, ", "), verdict)
}

// ParseSize reads a size such as 3G, 2GiB, 500M or a plain byte count. Every
// suffix is binary (G is 2^30), which is what `df -h` and `docker system df`
// mean by the same letters, so a number read off either can be typed back.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "i")
	mult := int64(1)
	if t != "" {
		switch strings.ToUpper(t[len(t)-1:]) {
		case "K":
			mult = 1 << 10
		case "M":
			mult = 1 << 20
		case "G":
			mult = 1 << 30
		case "T":
			mult = 1 << 40
		}
		if mult > 1 {
			t = t[:len(t)-1]
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size. Give a number of bytes or one with a K, M, G or T suffix, Eg: 2G", s)
	}
	return int64(n * float64(mult)), nil
}

// FormatSize prints bytes in GiB or MiB, to one decimal.
func FormatSize(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// composeImages reads the images a rendered compose file names, sorted and
// without repeats. The file is ours, rendered a moment ago, so a parse
// failure is a bug rather than a host problem.
func composeImages(content string) ([]string, error) {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, service := range doc.Services {
		if service.Image != "" && !seen[service.Image] {
			seen[service.Image] = true
			out = append(out, service.Image)
		}
	}
	sort.Strings(out)
	return out, nil
}

// siteImages maps each stack of the plan to the images its compose file
// renders.
func siteImages(changes []Change) (map[string][]string, error) {
	out := map[string][]string{}
	for _, c := range changes {
		if c.Stack == "" || c.Path != "/srv/"+c.Stack+"/compose.yaml" {
			continue
		}
		images, err := composeImages(c.content)
		if err != nil {
			return nil, fmt.Errorf("reading the images %s names: %w", c.Path, err)
		}
		out[c.Stack] = images
	}
	return out, nil
}

// imageProbe asks the host which images it has, and their IDs, in one round
// trip. It prints a line for every image rather than exiting non zero, so that
// "absent" cannot be confused with ssh failing.
func imageProbe(refs []string) string {
	quoted := make([]string, len(refs))
	for i, ref := range refs {
		quoted[i] = shellQuote(ref)
	}
	return fmt.Sprintf(`for r in %s; do if id=$(docker image inspect --format '{{.Id}}' "$r" 2>/dev/null); then echo "present $id $r"; else echo "absent $r"; fi; done`,
		strings.Join(quoted, " "))
}

// probeImages returns each ref's image ID, empty for one the host does not
// have. Every ref must be answered for: an output that skips one is refused
// rather than read as "present", which would skip the disk check exactly when
// it could not tell.
func probeImages(refs []string, t Transport) (map[string]string, error) {
	out := map[string]string{}
	if len(refs) == 0 {
		return out, nil
	}
	text, err := t.Run(imageProbe(refs))
	if err != nil {
		return nil, fmt.Errorf("asking which images the host has: %w", err)
	}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 3 && fields[0] == "present":
			out[fields[2]] = fields[1]
		case len(fields) == 2 && fields[0] == "absent":
			out[fields[1]] = ""
		}
	}
	for _, ref := range refs {
		if _, ok := out[ref]; !ok {
			return nil, fmt.Errorf("asking which images the host has: no answer for %s in:\n%s", ref, text)
		}
	}
	return out, nil
}

// probeDisk reads the free space on Docker's data root, and, when it is short,
// what `docker system df` says could be reclaimed.
func probeDisk(need int64, pulls []string, t Transport) (*DiskCheck, error) {
	dir, err := t.Run("docker info --format '{{.DockerRootDir}}'")
	if err != nil {
		return nil, fmt.Errorf("asking Docker for its data root: %w", err)
	}
	dir = strings.TrimSpace(dir)
	if dir == "" || !strings.HasPrefix(dir, "/") {
		return nil, fmt.Errorf("asking Docker for its data root: got %q", dir)
	}
	df, err := t.Run("df -B1 --output=avail " + shellQuote(dir))
	if err != nil {
		return nil, fmt.Errorf("reading free space on %s: %w", dir, err)
	}
	lines := strings.Fields(df)
	if len(lines) == 0 {
		return nil, fmt.Errorf("reading free space on %s: no output", dir)
	}
	free, err := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("reading free space on %s: unreadable `df` output %q", dir, df)
	}
	check := &DiskCheck{Dir: dir, Free: free, Need: need, Pulls: pulls}
	if check.Short() {
		summary, err := t.Run("docker system df --format '{{.Type}}: {{.Reclaimable}} reclaimable of {{.Size}}'")
		if err != nil {
			check.Reclaimable = []string{"`docker system df` failed: " + firstLine(strings.TrimSpace(summary))}
		} else {
			for _, line := range strings.Split(strings.TrimSpace(summary), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					check.Reclaimable = append(check.Reclaimable, line)
				}
			}
		}
	}
	return check, nil
}

// diskRefusal is Execute's error for a host short of space.
func diskRefusal(plan *Plan) error {
	d := plan.Disk
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s free on %s, and this apply needs at least %s before it pulls %s. Nothing was written and nothing was started.",
		plan.Site, FormatSize(d.Free), d.Dir, FormatSize(d.Need), strings.Join(d.Pulls, ", "))
	if len(d.Reclaimable) > 0 {
		fmt.Fprintf(&b, "\n\nWhat `docker system df` says could be reclaimed:\n  %s", strings.Join(d.Reclaimable, "\n  "))
	}
	fmt.Fprintf(&b, "\n\nFree space on the host (images no container uses are the usual place: `docker image ls`, then `docker image rm <id>`), or lower the threshold for this run with --min-free <size> (Eg: 2G) if the pull is known to fit.")
	return fmt.Errorf("%s", b.String())
}

// allRefs is every image the site renders, sorted and without repeats.
func allRefs(images map[string][]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, refs := range images {
		for _, ref := range refs {
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	sort.Strings(out)
	return out
}

// probeImages reads which of the site's rendered images the host already has,
// and, when a stack action will pull one, the free space on Docker's data
// root. Only a recreate pulls: `docker compose up -d` fetches a missing image,
// and `restart` reuses the container it has. Nothing is probed when no stack
// moves, so a plan with nothing to do asks the host nothing more.
func (p *Plan) probeImages(need int64, t Transport) error {
	images, err := siteImages(p.Changes)
	if err != nil {
		return err
	}
	p.images = images
	if len(p.Actions) == 0 {
		return nil
	}
	ids, err := probeImages(allRefs(images), t)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var pulls []string
	for _, action := range p.Actions {
		if !action.Recreate {
			continue
		}
		for _, ref := range images[action.Stack] {
			if ids[ref] == "" && !seen[ref] {
				seen[ref] = true
				pulls = append(pulls, ref)
			}
		}
	}
	if len(pulls) == 0 {
		return nil
	}
	p.Disk, err = probeDisk(need, pulls, t)
	return err
}
