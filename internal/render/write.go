package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestName is the record of what was rendered and what it hashed to.
const ManifestName = "manifest.json"

// Manifest lets a later `apply` tell an unchanged artifact from one somebody
// edited on the host. A rendered .env is a build artifact and nothing edits it
// in place, so a file whose hash no longer matches is a local modification and
// must be resolved rather than clobbered.
//
// It records the hash of what was rendered, not of what is on the host: apply
// compares the two.
type Manifest struct {
	// Version is this manifest's format, not the configuration's.
	Version int            `json:"version"`
	Files   []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

// Write writes the plan under dir, creating directories as needed, and adds a
// manifest per site.
//
// Rendering is deterministic: the same input produces byte identical output,
// which is what makes a diff between two renders meaningful. Nothing here
// records a timestamp for that reason.
func Write(plan *Plan, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	bySite := map[string][]ManifestFile{}
	for _, file := range plan.Files {
		full := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(file.Content), os.FileMode(file.Mode)); err != nil {
			return fmt.Errorf("writing %s: %w", full, err)
		}
		site, rest := splitSite(file.Path)
		sum := sha256.Sum256([]byte(file.Content))
		bySite[site] = append(bySite[site], ManifestFile{
			Path:   rest,
			SHA256: hex.EncodeToString(sum[:]),
			Mode:   fmt.Sprintf("%04o", file.Mode),
		})
	}

	sites := make([]string, 0, len(bySite))
	for site := range bySite {
		sites = append(sites, site)
	}
	sort.Strings(sites)

	for _, site := range sites {
		entries := bySite[site]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		data, err := json.MarshalIndent(Manifest{Version: 1, Files: entries}, "", "  ")
		if err != nil {
			return fmt.Errorf("building the manifest for %s: %w", site, err)
		}
		data = append(data, '\n')
		path := filepath.Join(dir, site, ManifestName)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

func splitSite(path string) (site, rest string) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return "", path
	}
	return parts[0], parts[1]
}
