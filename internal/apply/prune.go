package apply

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Prune is one image an apply would remove once the stack it was superseded
// in is healthy.
type Prune struct {
	Stack string
	// Ref is how the image is shown: repository:tag, or repository@id for an
	// image whose tag moved on.
	Ref string
	ID  string
}

// listedImage is the part of one `docker image ls --format json` line the
// prune reads. The keys are docker/cli's image formatter
// (cli/command/formatter/image.go).
type listedImage struct {
	ID         string `json:"ID"`
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
}

// imageList is the command prune reads, untruncated so an ID compares with
// what `docker image inspect` and `docker container inspect` print.
const imageList = "docker image ls --no-trunc --format json"

// containerImages lists what every container, running or stopped, was created
// from: by name as `ps` shows it, and by ID, because a container whose tag
// moved on shows its image's short ID or the old name, and either has to keep
// that image.
const containerImages = "docker ps -a --format '{{.Image}}' && docker ps -aq --no-trunc | xargs -r docker container inspect --format '{{.Image}}'"

func parseImages(out string) ([]listedImage, error) {
	var list []listedImage
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var image listedImage
		if err := json.Unmarshal([]byte(line), &image); err != nil {
			return nil, fmt.Errorf("unreadable `docker image ls` line %q: %w", line, err)
		}
		list = append(list, image)
	}
	return list, nil
}

// repository is a reference without its tag or digest, normalised the way
// `docker image ls` shows it: Docker Hub's `docker.io/` and `library/`
// prefixes dropped.
func repository(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "index.docker.io/")
	return strings.TrimPrefix(ref, "library/")
}

// normalRef is repository:tag, normalised like repository, with a missing tag
// read as latest. A digest is dropped: `docker image ls` shows the tag.
func normalRef(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	tag := "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		tag = ref[i+1:]
	}
	return repository(ref) + ":" + tag
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// sameID compares image IDs that may be full or short, with or without the
// algorithm. Twelve hex characters is the shortest form Docker prints.
func sameID(a, b string) bool {
	a, b = strings.TrimPrefix(a, "sha256:"), strings.TrimPrefix(b, "sha256:")
	if len(a) < 12 || len(b) < 12 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// superseded picks the images to prune for the stacks named, in order. An
// image qualifies when it is from a repository one of those stacks renders,
// is not an image any stack of this site renders (by ID or by
// repository:tag), and is not in inUse. Comparing against the whole site
// rather than the one stack keeps a shared repository safe: two apps pinning
// different postgres tags never prune each other's.
func superseded(stacks []string, images map[string][]string, ids map[string]string, listed []listedImage, inUse []string) []Prune {
	keepRefs := map[string]bool{}
	var keepIDs []string
	for _, refs := range images {
		for _, ref := range refs {
			keepRefs[normalRef(ref)] = true
			if id := ids[ref]; id != "" {
				keepIDs = append(keepIDs, id)
			}
		}
	}
	used := func(image listedImage) bool {
		ref := image.Repository + ":" + image.Tag
		for _, u := range inUse {
			if sameID(u, image.ID) || normalRef(u) == ref {
				return true
			}
		}
		return false
	}
	taken := map[string]bool{}
	var out []Prune
	for _, stack := range stacks {
		repos := map[string]bool{}
		for _, ref := range images[stack] {
			repos[repository(ref)] = true
		}
		for _, image := range listed {
			if !repos[repository(image.Repository)] || taken[image.ID] || image.ID == "" {
				continue
			}
			if keepRefs[normalRef(image.Repository+":"+image.Tag)] || used(image) {
				continue
			}
			kept := false
			for _, id := range keepIDs {
				if sameID(id, image.ID) {
					kept = true
				}
			}
			if kept {
				continue
			}
			taken[image.ID] = true
			ref := image.Repository + ":" + image.Tag
			if image.Tag == "" || image.Tag == "<none>" {
				ref = image.Repository + "@" + shortID(image.ID)
			}
			out = append(out, Prune{Stack: stack, Ref: ref, ID: image.ID})
		}
	}
	return out
}

// pruneStack removes the images one healthy stack superseded. Everything is
// read again rather than taken from the plan, because the action just pulled
// the new image and moved its containers off the old one. Any failure is a
// warning: the stack is already healthy, and an image left behind costs disk,
// not service.
func pruneStack(plan *Plan, stack string, t Transport) {
	warn := func(format string, args ...any) {
		plan.warn(stack+": old images left in place", fmt.Sprintf(format, args...))
	}
	ids, err := probeImages(allRefs(plan.images), t)
	if err != nil {
		warn("%v", err)
		return
	}
	out, err := t.Run(imageList)
	if err != nil {
		warn("listing images: %v", err)
		return
	}
	listed, err := parseImages(out)
	if err != nil {
		warn("%v", err)
		return
	}
	used, err := t.Run(containerImages)
	if err != nil {
		warn("listing what containers use: %v", err)
		return
	}
	var inUse []string
	for _, line := range strings.Split(used, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			inUse = append(inUse, line)
		}
	}
	for _, p := range superseded([]string{stack}, plan.images, ids, listed, inUse) {
		if out, err := t.Run("docker image rm " + shellQuote(p.ID)); err != nil {
			warn("could not remove %s (%s): %s", p.Ref, shortID(p.ID), firstLine(strings.TrimSpace(out)))
			continue
		}
		plan.say("pruned %s", p.Ref)
	}
}
