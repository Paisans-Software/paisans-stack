package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// SSH is how the toolkit reaches a site, and who may.
//
// It is a section rather than a destination string because a destination says
// where and as whom, and nothing about which keys. The keys are the part every
// admin shares: paisans.yaml is one file for the whole deployment, so it lists
// public keys, which are safe to share, and never a path to a private key,
// which is one admin's and lives on one admin's machine. `host prepare` makes
// the listed keys the user's authorized keys, so the file is also the record
// of who can log in.
type SSH struct {
	// Host is a hostname or an IP address. Empty means the site's
	// public_address; a site with neither is refused.
	Host string `yaml:"host"`
	// User is the login user. It must already exist on the host: creating
	// users is not something host prepare does.
	User string `yaml:"user"`
	// Port is the SSH port. Zero means 22.
	Port int `yaml:"port"`
	// Keys are the public keys that may log in, each under a name the
	// deployment knows it by.
	Keys KeyList `yaml:"keys"`

	// declared records that the key was present at all, so a missing
	// section and an empty one get different messages.
	declared bool
}

// DefaultSSHPort is the port used when a site's ssh section names none.
const DefaultSSHPort = 22

var sshKeys = map[string]bool{"host": true, "user": true, "port": true, "keys": true}

// UnmarshalYAML reads the section. There is one way to write a site's
// access, so anything other than a section is refused.
//
// yaml.v3 does not carry KnownFields into a custom unmarshaller, so unknown
// keys are checked here by hand: a misspelt `public_keys` that silently
// authorised nobody would be found out only when the toolkit removed a key.
func (s *SSH) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: ssh must be a section with host, user, port and keys", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Value == "public_key" {
			return fmt.Errorf("line %d: ssh.public_key is replaced by ssh.keys, which names each key. Write:\n%s", key.Line, keysSection(node.Content[i+1].Value))
		}
		if !sshKeys[key.Value] {
			return fmt.Errorf("line %d: field %s not found in an ssh section. Its keys are host, user, port and keys", key.Line, key.Value)
		}
	}
	type plain SSH
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*s = SSH(p)
	s.declared = true
	return nil
}

// PortOrDefault is the port to connect to: the declared one, or 22.
func (s SSH) PortOrDefault() int {
	if s.Port == 0 {
		return DefaultSSHPort
	}
	return s.Port
}

// SSHHost is the address the toolkit connects to: ssh.host, or failing that
// the site's public_address, or failing that its endpoint's host. Defaulting
// is because on most sites they are the same address, written twice; a site
// reached some other way (a LAN name, a VPN address) says so with ssh.host.
// The endpoint comes last because it may name a router that forwards only
// WireGuard's port: there the connection fails and nothing is changed.
func (s Site) SSHHost() string {
	if s.SSH.Host != "" {
		return s.SSH.Host
	}
	if s.PublicAddress != "" {
		return s.PublicAddress
	}
	host, _, err := net.SplitHostPort(s.Endpoint)
	if err != nil {
		return ""
	}
	return host
}

// keysSection is the ssh.keys section that says what a public_key said, each
// key named by its comment up to the @, so a refusal can show what to write.
// A key's base64 is shortened: the line is the operator's to paste, and a
// whole one wraps past any terminal.
func keysSection(publicKey string) string {
	out := "  keys:\n"
	n := 0
	for _, line := range strings.Split(publicKey, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n++
		name := fmt.Sprintf("key%d", n)
		if fields := strings.Fields(line); len(fields) > 2 {
			if local, _, _ := strings.Cut(fields[2], "@"); keyName.MatchString(strings.ToLower(local)) {
				name = strings.ToLower(local)
			}
		}
		shown := line
		if fields := strings.Fields(line); len(fields) > 1 && len(fields[1]) > 12 {
			fields[1] = fields[1][:12] + "..."
			shown = strings.Join(fields, " ")
		}
		out += "    " + name + ": " + shown + "\n"
	}
	return out
}

// NamedKey is one entry of ssh.keys: a name and the .pub line it names.
type NamedKey struct {
	Name string
	Line string
}

// KeyList is ssh.keys, in the order the file lists it.
type KeyList []NamedKey

// keyName is what a key may be called: it goes into a command line and a
// file on the host.
var keyName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// UnmarshalYAML reads ssh.keys, a mapping from a key's name to its .pub line,
// keeping the file's order. A name given twice is refused here, since yaml.v3
// does not check a mapping read node by node.
func (k *KeyList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: ssh.keys must map each key's name to its public key, Eg: alice: ssh-ed25519 AAAA... alice@example.org", node.Line)
	}
	seen := map[string]int{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		name, value := node.Content[i], node.Content[i+1]
		if first, dup := seen[name.Value]; dup {
			return fmt.Errorf("line %d: ssh.keys.%s is already given on line %d. Name each key once", name.Line, name.Value, first)
		}
		seen[name.Value] = name.Line
		if value.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: ssh.keys.%s must be one public key, the whole line of the .pub file", value.Line, name.Value)
		}
		*k = append(*k, NamedKey{Name: name.Value, Line: value.Value})
	}
	return nil
}

// AuthorizedKey is one public key, as an authorized_keys line carries it.
type AuthorizedKey struct {
	// Name is what ssh.keys calls the key; empty for a line read off a host.
	Name string
	// Line is the key as written, without options: type, base64, comment.
	Line string
	// Type is the key's algorithm name, Eg: ssh-ed25519.
	Type string
	// Fingerprint is the SHA256 fingerprint ssh-keygen -l prints. It is the
	// key's identity: two lines with the same key and different comments are
	// the same key.
	Fingerprint string
	// Comment is whatever followed the key, usually user@machine.
	Comment string
}

// ParseKeyLine reads one authorized_keys line. Options, if the line has any,
// are returned rather than refused, because authorized_keys on a host may
// carry them; paisans.yaml may not, and Keys refuses them there.
func ParseKeyLine(line string) (AuthorizedKey, []string, error) {
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return AuthorizedKey{}, nil, err
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return AuthorizedKey{}, nil, fmt.Errorf("more than one key on the line")
	}
	marshalled := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		marshalled += " " + comment
	}
	return AuthorizedKey{
		Line:        marshalled,
		Type:        pub.Type(),
		Fingerprint: ssh.FingerprintSHA256(pub),
		Comment:     comment,
	}, options, nil
}

// AuthorizedKeys parses ssh.keys. A name this toolkit does not accept is a
// problem, and so is a line that is not a plain key and a key listed twice.
//
// Options (`from=`, `command=`, `restrict` and the rest) are refused rather
// than carried. They change what a key may do, and a line that host prepare
// writes and later compares has to mean the same thing everywhere; a
// restricted key belongs in authorized_keys by hand, where host prepare leaves
// it alone.
func (s SSH) AuthorizedKeys() ([]AuthorizedKey, []string) {
	var keys []AuthorizedKey
	var problems []string
	seen := map[string]string{}
	for _, nk := range s.Keys {
		if !keyName.MatchString(nk.Name) {
			problems = append(problems, fmt.Sprintf("keys.%s: not a key name this toolkit accepts. Use a lowercase letter or digit, then lowercase letters, digits, dots, underscores or hyphens, at most 32 characters", nk.Name))
			continue
		}
		key, options, err := ParseKeyLine(strings.TrimSpace(nk.Line))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("keys.%s: not an OpenSSH public key (%v). Paste the whole line of the .pub file, Eg: ssh-ed25519 AAAA... you@example.org", nk.Name, err))
			continue
		case len(options) > 0:
			problems = append(problems, fmt.Sprintf("keys.%s: carries options (%s). List plain keys only; a restricted key is added to authorized_keys by hand, where host prepare leaves it alone", nk.Name, strings.Join(options, ",")))
			continue
		}
		if first, dup := seen[key.Fingerprint]; dup {
			problems = append(problems, fmt.Sprintf("keys.%s: the same key as keys.%s (%s). List each key once", nk.Name, first, key.Fingerprint))
			continue
		}
		seen[key.Fingerprint] = nk.Name
		key.Name = nk.Name
		keys = append(keys, key)
	}
	return keys, problems
}

// unixUser is a conservative login name: what the toolkit is willing to put in
// a command line and a file name on the host without quoting surprises.
var unixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// hostLabel is one label of a DNS name.
var hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func isHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// sshProblems is structural's check of one site's ssh section.
func sshProblems(name string, site Site) []string {
	s := site.SSH
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if !s.declared {
		add("sites.%s.ssh: required. Give the section with at least user and keys: it is how the toolkit reaches the site and who may log in to it.", name)
		return problems
	}
	switch {
	case s.User == "":
		add("sites.%s.ssh.user: required. Name the login user, which must already exist on the host.", name)
	case !unixUser.MatchString(s.User):
		add("sites.%s.ssh.user: %q is not a user name this toolkit accepts. Use a lowercase letter or underscore, then lowercase letters, digits, underscores or hyphens, at most 32 characters.", name, s.User)
	}
	if s.Port < 0 || s.Port > 65535 {
		add("sites.%s.ssh.port: %d is not a port. Give 1 to 65535, or leave it out for 22.", name, s.Port)
	}
	switch {
	case s.Host == "" && site.PublicAddress == "" && site.Endpoint == "":
		add("sites.%s.ssh.host: required, because the site has neither public_address nor endpoint to default to. Give the hostname or address the site is reached on.", name)
	case s.Host != "" && net.ParseIP(s.Host) == nil && !isHostname(s.Host):
		add("sites.%s.ssh.host: %q is neither a hostname nor an IP address. Give only the host; the user and port have keys of their own.", name, s.Host)
	}
	keys, keyProblems := s.AuthorizedKeys()
	for _, p := range keyProblems {
		add("sites.%s.ssh.%s.", name, p)
	}
	if len(keys) == 0 && len(keyProblems) == 0 {
		add("sites.%s.ssh.keys: required. Name at least one public key, Eg: alice: ssh-ed25519 AAAA... alice@example.org; host prepare makes these the user's authorized keys.", name)
	}
	return problems
}

// Destination is where a host is reached: user, host and port. Its string
// form is user@host:port, with the port always written.
type Destination struct {
	User string
	Host string
	Port int
}

func (d Destination) String() string {
	return d.User + "@" + net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
}

// ParseDestination reads user@host or user@host:port, with an IPv6 host in
// brackets. The user is required, because it is whose authorized keys a
// cleaning reads, so an ssh alias is refused. The user and the host are held
// to the rules a declared ssh section is, since both go into command lines.
func ParseDestination(s string) (Destination, error) {
	form := fmt.Errorf("%q is not user@host[:port], Eg: admin@203.0.113.9, admin@203.0.113.9:2222 or admin@[2001:db8::1]:22", s)
	user, rest, ok := strings.Cut(s, "@")
	if !ok || !unixUser.MatchString(user) || rest == "" {
		return Destination{}, form
	}
	host, port := rest, DefaultSSHPort
	bracketed := strings.HasPrefix(rest, "[")
	switch {
	case bracketed && strings.HasSuffix(rest, "]"):
		host = rest[1 : len(rest)-1]
	case bracketed || strings.Contains(rest, ":"):
		h, p, err := net.SplitHostPort(rest)
		if err != nil {
			return Destination{}, form
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Destination{}, fmt.Errorf("%q: the port must be a number from 1 to 65535", s)
		}
		host, port = h, n
	}
	ip := net.ParseIP(host)
	switch {
	case bracketed && (ip == nil || ip.To4() != nil):
		return Destination{}, fmt.Errorf("%q: only an IPv6 address goes in brackets", s)
	case !bracketed && ip == nil && !isHostname(host):
		return Destination{}, form
	}
	return Destination{User: user, Host: host, Port: port}, nil
}

// Destination is where the site's ssh section reaches it.
func (s Site) Destination() Destination {
	return Destination{User: s.SSH.User, Host: s.SSHHost(), Port: s.SSH.PortOrDefault()}
}
