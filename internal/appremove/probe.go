package appremove

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/patroni"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

// Host is how this package reaches a site. apply.SSHTransport satisfies it.
type Host interface {
	Run(command string) (string, error)
	RunInput(command, stdin string) (string, error)
	ReadFile(path string) (content string, found bool, err error)
	WriteFile(path, content string, mode uint32) error
	Describe() string
}

// Clients is the part of Pocket ID's API this package uses.
// *pocketid.Client satisfies it.
type Clients interface {
	OIDCClientByID(id string) (*pocketid.OIDCClient, error)
	FindOIDCClient(name string) (*pocketid.OIDCClient, error)
	DeleteOIDCClient(id string) error
}

// ProbeSite reads one site: its inventory (internal/hostcheck, reads
// only), the host's copy of each of the app's manifest files, and the app's
// stack directory.
func ProbeSite(cfg *config.Config, app, site string, h Host) (SiteState, error) {
	d := cfg.Deployment()
	st := SiteState{Site: site, Hashes: map[string]string{}}
	inv, err := hostcheck.Inspect(h, d)
	if err != nil {
		return st, fmt.Errorf("%s: %w", site, err)
	}
	st.Inventory = inv
	for _, e := range inv.ManifestFiles {
		if !AppFile(cfg, app, e.Path) {
			continue
		}
		content, found, err := h.ReadFile("/" + e.Path)
		if err != nil {
			return st, fmt.Errorf("%s: reading /%s: %w", site, e.Path, err)
		}
		if found {
			sum := sha256.Sum256([]byte(content))
			st.Hashes[e.Path] = hex.EncodeToString(sum[:])
		}
	}
	st.Dir, err = probeDir(h, d.Dir(app))
	if err != nil {
		return st, fmt.Errorf("%s: %w", site, err)
	}
	return st, nil
}

// DirCommand prints, for a directory that exists, a marker, its size in
// bytes, how many files and links are under it, and its top level entries.
// It prints nothing for one that does not.
func DirCommand(dir string) string {
	return fmt.Sprintf(`d=%s; if [ -d "$d" ]; then echo present; du -sb "$d" | cut -f1; find "$d" -mindepth 1 \( -type f -o -type l \) | wc -l; find "$d" -mindepth 1 -maxdepth 1 -printf '%%P\n' | sort; fi`, quote(dir))
}

func probeDir(h Host, dir string) (Dir, error) {
	out, err := h.Run(DirCommand(dir))
	if err != nil {
		return Dir{}, fmt.Errorf("reading %s: %w", dir, err)
	}
	dd := Dir{Path: dir}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "present" {
		if strings.TrimSpace(out) != "" {
			return Dir{}, fmt.Errorf("reading %s: an answer that is not the probe's: %s", dir, firstLine(out))
		}
		return dd, nil
	}
	if len(lines) < 3 {
		return Dir{}, fmt.Errorf("reading %s: a cut short answer", dir)
	}
	dd.Exists = true
	if dd.Bytes, err = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); err != nil {
		return Dir{}, fmt.Errorf("reading %s's size: %w", dir, err)
	}
	if dd.Files, err = strconv.Atoi(strings.TrimSpace(lines[2])); err != nil {
		return Dir{}, fmt.Errorf("reading %s's file count: %w", dir, err)
	}
	for _, l := range lines[3:] {
		if l = strings.TrimSpace(l); l != "" {
			dd.Entries = append(dd.Entries, l)
		}
	}
	return dd, nil
}

// ProbeClient reads the app's client at Pocket ID: by the ID the secrets
// file records, and by the app's name.
func ProbeClient(c Clients, app, provider, site, recordedID string) (ClientState, error) {
	st := ClientState{Provider: provider, Site: site, RecordedID: recordedID}
	var err error
	if recordedID != "" {
		if st.ByID, err = c.OIDCClientByID(recordedID); err != nil {
			return st, fmt.Errorf("looking up client %s by its recorded ID: %w", app, err)
		}
	}
	if st.ByName, err = c.FindOIDCClient(app); err != nil {
		return st, fmt.Errorf("looking up client %s: %w", app, err)
	}
	return st, nil
}

// psql reaches the leader's Postgres the way apply's database bootstrap
// does: through the Spilo container's own socket as the superuser, any
// psqlrc skipped, stopping at the first error.
func psql(d deployment.Deployment) string {
	return patroni.Exec(d) + " psql -X -q -U postgres -d postgres -v ON_ERROR_STOP=1"
}

// databaseProbeSQL lists the database and the role named name, one line
// each, unaligned and without headers.
func databaseProbeSQL(name string) string {
	lit := quoteLiteral(name)
	return "\\pset format unaligned\n\\pset tuples_only on\n" +
		"SELECT 'database', datname, pg_get_userbyid(datdba) FROM pg_database WHERE datname = " + lit + ";\n" +
		"SELECT 'role', rolname, '' FROM pg_roles WHERE rolname = " + lit + ";\n"
}

// ProbeDatabase finds the cluster's leader, asking each cluster site's
// Patroni in turn, and reads what it holds under the app's database name.
func ProbeDatabase(cfg *config.Config, app string, hosts map[string]Host) (*DatabaseState, error) {
	if len(cfg.Cluster.Sites) == 0 {
		return nil, nil
	}
	d := cfg.Deployment()
	st := &DatabaseState{Name: render.DBIdentifier(app)}
	var asked []string
	for _, site := range cfg.Cluster.Sites {
		h := hosts[site]
		if h == nil {
			continue
		}
		api := fmt.Sprintf("%s:%d", cfg.Sites[site].Address, render.PatroniAPIPort)
		out, err := h.Run(patroni.ClusterCommand(d, api))
		if err != nil {
			asked = append(asked, fmt.Sprintf("%s: %v", site, err))
			continue
		}
		c, err := patroni.Parse(out)
		if err != nil {
			asked = append(asked, fmt.Sprintf("%s: %v", site, err))
			continue
		}
		if m, ok := c.Leader(); ok {
			st.Leader = m.Name
			break
		}
		asked = append(asked, site+": no running leader")
	}
	if st.Leader == "" {
		return nil, fmt.Errorf("no Patroni leader found, so the app's database cannot be read:\n  %s", strings.Join(asked, "\n  "))
	}
	h := hosts[st.Leader]
	if h == nil {
		return nil, fmt.Errorf("Patroni's leader is %q, which is not one of cluster.sites", st.Leader)
	}
	out, err := h.RunInput(psql(d), databaseProbeSQL(st.Name))
	if err != nil {
		return nil, fmt.Errorf("%s: reading database %s: %w: %s", st.Leader, st.Name, err, firstLine(out))
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 3 || f[1] != st.Name {
			continue
		}
		switch f[0] {
		case "database":
			st.Database, st.Owner = true, f[2]
		case "role":
			st.Role = true
		}
	}
	return st, nil
}

// ProbeStorage reads Garage, on the first Garage site, for the app's
// recorded keys and every bucket they are authorized on.
func ProbeStorage(cfg *config.Config, secrets *config.Secrets, app string, h Host) (*StorageState, error) {
	if len(cfg.Storage.Garage.Sites) == 0 {
		return nil, nil
	}
	d := cfg.Deployment()
	anchor := cfg.Storage.Garage.Sites[0]
	st := &StorageState{Anchor: anchor, Address: cfg.Sites[anchor].Address, Buckets: map[string]garage.BucketState{}}
	pairs := [][2]string{{"s3_access_key_id", "s3_secret_access_key"}, {secretsgen.PreviousKeyID, secretsgen.PreviousSecretKey}}
	declared := map[string]bool{}
	for _, other := range cfg.AppNames() {
		for _, pair := range pairs {
			if id, _ := garage.SecretString(secrets, other, pair[0]); id != "" {
				declared[id] = true
			}
		}
	}
	for _, pair := range pairs {
		id, _ := garage.SecretString(secrets, app, pair[0])
		if id == "" {
			continue
		}
		if !garageKeyShape(id) {
			return nil, fmt.Errorf("secrets apps.%s.%s is not a Garage key ID", app, pair[0])
		}
		if declared[id] {
			st.Shared = append(st.Shared, id)
			continue
		}
		secret, _ := garage.SecretString(secrets, app, pair[1])
		kb, err := garage.ReadKeyBuckets(h, d, id)
		if err != nil {
			return nil, err
		}
		st.Keys = append(st.Keys, KeyState{ID: id, Secret: secret, Absent: kb.Absent, Buckets: kb.Buckets})
		for _, b := range kb.Buckets {
			if _, ok := st.Buckets[b]; ok {
				continue
			}
			bs, err := garage.ReadBucketState(h, d, b)
			if err != nil {
				return nil, err
			}
			st.Buckets[b] = bs
		}
	}
	return st, nil
}

// garageKeyShape is a Garage access key ID: GK and 24 hex digits
// (src/model/key_table.rs, Key::new, v1.0.1). It goes into a command line.
func garageKeyShape(id string) bool {
	return len(id) == 26 && strings.HasPrefix(id, "GK") && isLowerHex(id[2:])
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return s != ""
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
