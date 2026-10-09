package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsyaml "github.com/getsops/sops/v3/stores/yaml"
	"gopkg.in/yaml.v3"
)

// Secrets is the decrypted content of secrets.enc.yaml. Nothing here is ever
// written back out except into a rendered artifact.
//
// Three kinds live in one file: generated at init and never seen, pasted
// because someone else issued them, and captured after a running service
// minted them.
type Secrets struct {
	Version     int                       `yaml:"version"`
	Cluster     ClusterSecrets            `yaml:"cluster"`
	Storage     StorageSecrets            `yaml:"storage"`
	Sites       map[string]SiteSecrets    `yaml:"sites"`
	Apps        map[string]map[string]any `yaml:"apps"`
	External    map[string]string         `yaml:"external"`
	OIDCClients map[string]OIDCClient     `yaml:"oidc_clients"`
	// PocketIDGroups is the ID each group named in the Pocket ID app's
	// signup_default_groups has at the deployment's Pocket ID, keyed by
	// group name. apply resolves and records them, as it records a client's
	// ID under oidc_clients, so that render is a pure function of the files
	// and every Pocket ID instance renders the same IDs. Not a secret, but
	// it is state the toolkit wrote about a live instance, which is what
	// this file holds beside the credentials.
	PocketIDGroups map[string]string `yaml:"pocket_id_groups,omitempty"`

	// Path is where these were read from, for error messages.
	Path string `yaml:"-"`
	// Encrypted records whether the file was sops encrypted. A plaintext file
	// is accepted so that tests and examples work without a key, and the
	// caller is expected to say so out loud.
	Encrypted bool `yaml:"-"`
}

type ClusterSecrets struct {
	SuperuserPassword string `yaml:"superuser_password"`
	AdminPassword     string `yaml:"admin_password"`
	StandbyPassword   string `yaml:"standby_password"`
}

type StorageSecrets struct {
	Garage GarageSecrets `yaml:"garage"`
}

// GarageSecrets is Garage's own credentials: the admin API token and the RPC
// secret nodes use to talk to each other. S3 credentials are not here. They
// are per app, under Secrets.Apps, because one shared key would let any one
// app read, rewrite and delete every other app's objects.
type GarageSecrets struct {
	AdminToken string `yaml:"admin_token"`
	RPCSecret  string `yaml:"rpc_secret"`
}

type SiteSecrets struct {
	WireGuardPrivateKey string `yaml:"wireguard_private_key"`
	// HeartbeatToken is what the site's host pushes to every monitor with,
	// and what each monitor's seed names for the site: 32 lowercase hex
	// characters, generated at init. README, "Monitoring".
	HeartbeatToken string `yaml:"heartbeat_token"`
}

type OIDCClient struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

// LoadSecrets reads a secrets file, decrypting it in process when it is sops
// encrypted. The sops and age libraries are embedded deliberately: neither
// binary is a dependency of a workstation, and the fewer install steps stand
// between an operator and a working deployment the better.
//
// Decryption happens here, on a workstation, and never on a host. What that
// buys is blast radius: root on one host yields that host's rendered secrets,
// not another site's.
func LoadSecrets(path string) (*Secrets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	plain := data
	encrypted := looksEncrypted(data)
	if encrypted {
		plain, err = decryptSOPS(data)
		if err != nil {
			return nil, fmt.Errorf("decrypting %s: %w", path, err)
		}
	}
	var s Secrets
	if err := yaml.Unmarshal(plain, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	s.Path = path
	s.Encrypted = encrypted
	if s.Version != 1 {
		return nil, fmt.Errorf("%s: version: expected 1, got %d", path, s.Version)
	}
	return &s, nil
}

// looksEncrypted reports whether a file carries sops metadata. It is a cheap
// check on the serialised form rather than a decode, because a plaintext
// fixture is a legitimate input and must not have to satisfy the sops schema.
func looksEncrypted(data []byte) bool {
	var probe struct {
		SOPS map[string]any `yaml:"sops"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return len(probe.SOPS) > 0
}

func decryptSOPS(data []byte) ([]byte, error) {
	store := &sopsyaml.Store{}
	tree, err := store.LoadEncryptedFile(data)
	if err != nil {
		return nil, err
	}
	key, err := tree.Metadata.GetDataKey()
	if err != nil {
		return nil, decorateKeyError(err)
	}
	cipher := aes.NewCipher()
	mac, err := tree.Decrypt(key, cipher)
	if err != nil {
		return nil, err
	}
	if err := verifyMAC(&tree, key, cipher, mac); err != nil {
		return nil, err
	}
	return store.EmitPlainFile(tree.Branches)
}

// verifyMAC confirms the file has not been altered since it was encrypted. A
// decrypt that skips this reads a tampered file happily, and these values
// become a running deployment's credentials.
func verifyMAC(tree *sops.Tree, key []byte, cipher sops.Cipher, computed string) error {
	stored, err := cipher.Decrypt(
		tree.Metadata.MessageAuthenticationCode,
		key,
		tree.Metadata.LastModified.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("cannot read the file's authentication code: %w", err)
	}
	if stored != computed {
		return errors.New("authentication code mismatch: the file was modified after it was encrypted. Restore it from git rather than editing it by hand")
	}
	return nil
}

// decorateKeyError turns sops's internal wording into an instruction. Failing
// to find a key is the ordinary case for a new admin, and "0 successful groups
// required, got 0" tells them nothing they can act on.
func decorateKeyError(err error) error {
	return fmt.Errorf("%w\nNo age identity could decrypt this file. Point SOPS_AGE_KEY_FILE at your private key, or set SOPS_AGE_KEY_CMD to a command that prints it (a keychain lookup, say, so the key is never a file), and check that your public key is a recipient in .sops.yaml. If it was added recently, someone has to run `sops updatekeys` on the file", err)
}
