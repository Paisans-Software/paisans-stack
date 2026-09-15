package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/keys"
	sopsyaml "github.com/getsops/sops/v3/stores/yaml"
	"github.com/getsops/sops/v3/version"
	"gopkg.in/yaml.v3"
)

// SOPSConfigName is the file that records who may decrypt. It sits beside the
// secrets, is committed, and adding or removing an admin is an edit to it
// followed by a re-encrypt rather than a rotation of every secret.
const SOPSConfigName = ".sops.yaml"

// WriteSecrets serialises secrets and writes them to path, encrypted to the
// given age recipients.
//
// Writing is separated from generating on purpose. Generation decides what a
// deployment needs; this decides who can read it afterwards, and those are
// different questions with different failure modes.
//
// With no recipients the file is written in plaintext and the caller is
// expected to say so loudly. That is not a convenience for real deployments: it
// exists because fixtures and a first look at the tool must not require an age
// key, and because refusing to write anything at all would leave an operator
// with generated secrets that went nowhere.
func WriteSecrets(path string, s *Secrets, recipients []string) error {
	plain, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("serialising secrets: %w", err)
	}
	out := plain
	if len(recipients) > 0 {
		// The header is left off an encrypted file deliberately. sops encrypts
		// comments as well as values, so a header there would be an unreadable
		// ENC[...] block at the top of the file, which is worse than no header:
		// it looks like data.
		out, err = encryptSOPS(plain, recipients)
		if err != nil {
			return fmt.Errorf("encrypting %s: %w", path, err)
		}
	} else {
		out = append([]byte(secretsHeader), plain...)
	}

	// 0600 because this is the plaintext form on a workstation whenever it is
	// not encrypted, and because an encrypted file still says which secrets
	// exist.
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

const secretsHeader = `# Generated and maintained by paisans. Values are never printed and never
# logged; this file is the only place they exist.
#
# Re-running init fills in what is missing and changes nothing that is already
# set. Regenerating a WireGuard key would break every peer that trusted the old
# one, and regenerating a database password would leave an application locked
# out of a role that still has the old one.
`

// encryptSOPS encrypts a plaintext YAML document to the given age recipients.
//
// The sops and age libraries are embedded rather than shelled out to, so
// neither binary has to be installed. The consequence worth knowing is that a
// file written here is an ordinary sops file: `sops secrets.enc.yaml` edits it,
// and `sops updatekeys` re-encrypts it after a recipient changes.
func encryptSOPS(plain []byte, recipients []string) ([]byte, error) {
	store := &sopsyaml.Store{}
	branches, err := store.LoadPlainFile(plain)
	if err != nil {
		return nil, err
	}
	masterKeys, err := sopsage.MasterKeysFromRecipients(strings.Join(recipients, ","))
	if err != nil {
		return nil, fmt.Errorf("reading age recipients: %w", err)
	}
	var group sops.KeyGroup
	for _, key := range masterKeys {
		group = append(group, keys.MasterKey(key))
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			KeyGroups:    []sops.KeyGroup{group},
			Version:      version.Version,
			LastModified: time.Now().UTC(),
		},
	}
	dataKey, errs := tree.GenerateDataKey()
	if len(errs) > 0 {
		return nil, fmt.Errorf("generating a data key: %v", errs)
	}
	cipher := aes.NewCipher()
	mac, err := tree.Encrypt(dataKey, cipher)
	if err != nil {
		return nil, err
	}
	encryptedMAC, err := cipher.Encrypt(mac, dataKey, tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	tree.Metadata.MessageAuthenticationCode = encryptedMAC
	return store.EmitEncryptedFile(tree)
}

// Recipients reads the age public keys that may decrypt, from a .sops.yaml
// beside the given file.
//
// Only the `age` field of each creation rule is read, and every rule's
// recipients are taken together rather than matched against a path regex. That
// is narrower than sops itself, and deliberately so: this toolkit writes one
// secrets file per deployment, so a configuration that says different admins
// may read different files is describing something the toolkit cannot do, and
// silently picking the first matching rule would hide that.
func Recipients(dir string) ([]string, error) {
	path := filepath.Join(dir, SOPSConfigName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc struct {
		CreationRules []struct {
			Age string `yaml:"age"`
		} `yaml:"creation_rules"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var out []string
	seen := map[string]bool{}
	for _, rule := range doc.CreationRules {
		for _, recipient := range strings.Split(rule.Age, ",") {
			recipient = strings.TrimSpace(recipient)
			if recipient == "" || seen[recipient] {
				continue
			}
			seen[recipient] = true
			out = append(out, recipient)
		}
	}
	return out, nil
}
