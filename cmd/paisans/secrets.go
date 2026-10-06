package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

// maxSecret bounds what `secrets set` reads. The largest secret this file
// holds is a PEM signing key of a few kilobytes; anything near this size is a
// wrong pipe, not a credential.
const maxSecret = 64 << 10

// runSecrets dispatches `paisans secrets <subcommand>`.
func runSecrets(args []string) error {
	if len(args) < 1 || args[0] != "set" {
		return fmt.Errorf("secrets takes one subcommand, set: paisans secrets set <dotted.key> [--secrets path] < value")
	}
	return runSecretsSet(args[1:], os.Stdin)
}

// runSecretsSet writes one value, read from stdin, into the encrypted secrets
// file.
//
// It exists so a pasted credential never touches a terminal or an editor. The
// alternatives each leave a copy somewhere: an argument is in shell history
// and in `ps`, a prompt is in a scrollback buffer the moment it is echoed, and
// `sops secrets.enc.yaml` opens the whole decrypted file in an editor that may
// keep swap or backup files. A pipe from a keychain or another host leaves
// none of those.
//
// It refuses a key that is neither already set nor owed from elsewhere. A
// generated secret is created by `init`, and creating one by hand here would
// skip the shape checks init applies; changing one that exists is a rotation,
// which is allowed.
func runSecretsSet(args []string, stdin io.Reader) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("secrets set: name the key first, as in `paisans secrets set external.acme_dns_token < token`")
	}
	key := args[0]
	fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file (default: secrets.enc.yaml beside the config)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("secrets set: the value is read from stdin and never from an argument, which shell history and `ps` would both keep. Got extra argument(s)")
	}

	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("secrets set: stdin is a terminal, so the value would be typed where it can be seen and kept in scrollback. Pipe it in, as in `security find-generic-password -s <item> -w | paisans secrets set %s`", key)
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}

	if err := secretKeyAllowed(cfg, secrets, key); err != nil {
		return err
	}

	data, err := io.ReadAll(io.LimitReader(stdin, maxSecret+1))
	if err != nil {
		return fmt.Errorf("secrets set: reading stdin: %w", err)
	}
	if len(data) > maxSecret {
		return fmt.Errorf("secrets set: more than %d bytes on stdin, which is not a credential this file holds", maxSecret)
	}
	value := string(data)
	// One trailing newline, because `echo` and most password managers add
	// exactly one and no credential ends in one. Only one: a value that ends
	// in two kept the second on purpose.
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" {
		return fmt.Errorf("secrets set: %s: nothing on stdin. An empty value is refused rather than written, since it reads as set and renders as missing", key)
	}

	if err := secrets.Set(key, value); err != nil {
		return err
	}
	// The same check render and apply make, so a malformed key is refused
	// here rather than at the next render.
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}

	recipients, err := config.Recipients(filepath.Dir(*secretsPath))
	if err != nil {
		return err
	}
	if secrets.Encrypted && len(recipients) == 0 {
		return fmt.Errorf("secrets set: %s is encrypted, but no %s beside it names a recipient, so writing it back would leave it in plaintext. Nothing was written", *secretsPath, config.SOPSConfigName)
	}
	if err := config.WriteSecrets(*secretsPath, secrets, recipients); err != nil {
		return err
	}
	// The name, never the value.
	fmt.Fprintf(os.Stdout, "set %s\n", key)
	if len(recipients) == 0 {
		fmt.Fprintf(os.Stderr, "paisans: %s is PLAINTEXT, because no %s beside it names an age recipient.\n", *secretsPath, config.SOPSConfigName)
	}
	return nil
}

// secretKeyAllowed accepts a key that is already set, a key `init` reports as
// owed from elsewhere, and any key under external, which is by definition
// the section for pasted credentials. Everything else is refused, and an
// unknown top level section most loudly, because a typo there would write a
// value nothing reads.
func secretKeyAllowed(cfg *config.Config, secrets *config.Secrets, key string) error {
	section := key
	if i := strings.IndexByte(key, '.'); i >= 0 {
		section = key[:i]
	}
	switch section {
	case "cluster", "storage", "sites", "apps", "external", "oidc_clients":
	default:
		return fmt.Errorf("secrets set: %s: there is no %q section. Secrets live under cluster, storage, sites, apps, external and oidc_clients", key, section)
	}
	if _, set := secrets.Get(key); set {
		return nil
	}
	if section == "external" {
		return nil
	}
	for _, name := range secretsgen.OwedNames(cfg, secrets) {
		if key == name || strings.HasPrefix(key, name+".") {
			return nil
		}
	}
	return errors.New("secrets set: " + key + " is neither set nor owed from elsewhere. A generated secret is created by `paisans init`, which fills in what is missing; this command sets a pasted or captured one, or replaces one that exists")
}
