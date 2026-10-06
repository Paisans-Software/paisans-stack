package hostprep

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// What follows makes the keys in a site's ssh.public_key the login user's
// authorized keys, without taking the file over.
//
// authorized_keys is shared with whoever else manages the host: cloud-init
// puts the provider's key there, and an operator may add a restricted key by
// hand. So host prepare only ever adds a listed key, and only ever removes a
// key it added itself. What it added is recorded in a sidecar file,
// /etc/paisans/authorized_keys.<user>.owned, one fingerprint per line, root's
// and 0600. The line in authorized_keys is left exactly as the key was
// written.
//
// Rejected: marking ownership in the key's comment, Eg: appending
// "paisans" to it. The comment is how an operator recognises a key
// (`alice@laptop`), and rewriting it, or a cloud-init key's, changes what
// they see in the one place they look. The sidecar costs a second file and
// keeps authorized_keys looking exactly like what was pasted.
//
// The commands use GNU coreutils (`chown --reference`), which every Linux
// profile registered here has. authorized_keys itself is OpenSSH's format
// everywhere, which is why this is not behind Profile.

// ownedDir holds the sidecar files.
const ownedDir = "/etc/paisans"

// OwnedKeysPath is the sidecar recording which of user's authorized keys host
// prepare added.
func OwnedKeysPath(user string) string {
	return ownedDir + "/authorized_keys." + user + ".owned"
}

// passwdProbe prints the user's passwd entry, or a marker when there is none,
// so that "no such user" cannot be confused with ssh failing.
func passwdProbe(user string) string {
	return "getent passwd " + shellQuote(user) + " || echo __PAISANS_ABSENT__"
}

// hostKeyLine is one key line of a host's authorized_keys.
type hostKeyLine struct {
	raw     string // verbatim, so a removal names exactly this line
	key     config.AuthorizedKey
	options bool
}

// ownedEntry is one line of the sidecar.
type ownedEntry struct {
	fingerprint string
	comment     string
}

// planAuthorizedKeys compares the listed keys with the user's authorized_keys
// and the sidecar. It returns the additions and adoptions, which run with the
// rest of the plan, and the removals, which run last: a key is only taken
// away once every listed key is in place.
func planAuthorizedKeys(t Transport, s config.SSH) (out Section, removals []Step, err error) {
	user := s.User
	listed, problems := s.Keys()
	if len(problems) > 0 {
		// Load refuses these; reaching here is a bug, not an operator error.
		return out, nil, fmt.Errorf("ssh.public_key: %s", strings.Join(problems, "; "))
	}

	entry, err := t.Run(passwdProbe(user))
	if err != nil {
		return out, nil, err
	}
	entry = strings.TrimSpace(entry)
	if entry == "" || entry == "__PAISANS_ABSENT__" {
		return out, nil, fmt.Errorf("ssh.user %s does not exist on %s. host prepare does not create users: create it, with sudo, and prepare again", user, t.Describe())
	}
	fields := strings.Split(entry, ":")
	if len(fields) < 7 || fields[5] == "" {
		return out, nil, fmt.Errorf("cannot read the passwd entry for %s on %s: %q", user, t.Describe(), entry)
	}
	uid, gid, home := fields[2], fields[3], fields[5]
	dir := strings.TrimSuffix(home, "/") + "/.ssh"
	file := dir + "/authorized_keys"

	content, found, err := t.ReadFile(file)
	if err != nil {
		return out, nil, err
	}
	var lines []hostKeyLine
	for _, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, options, err := config.ParseKeyLine(trimmed)
		if err != nil {
			continue // not a key this package understands, so never touched
		}
		lines = append(lines, hostKeyLine{raw: strings.TrimSuffix(raw, "\r"), key: key, options: len(options) > 0})
	}

	sidecar, _, err := t.ReadFile(OwnedKeysPath(user))
	if err != nil {
		return out, nil, err
	}
	var owned []ownedEntry
	for _, line := range strings.Split(sidecar, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fp, comment, _ := strings.Cut(line, " ")
		owned = append(owned, ownedEntry{fingerprint: fp, comment: comment})
	}
	isOwned := func(fp string) bool {
		for _, o := range owned {
			if o.fingerprint == fp {
				return true
			}
		}
		return false
	}
	setOwned := func(fp, comment string) {
		if !isOwned(fp) {
			owned = append(owned, ownedEntry{fingerprint: fp, comment: comment})
		}
	}
	dropOwned := func(fp string) {
		kept := owned[:0:0]
		for _, o := range owned {
			if o.fingerprint != fp {
				kept = append(kept, o)
			}
		}
		owned = kept
	}
	onHost := func(fp string) (plain, restricted []hostKeyLine) {
		for _, l := range lines {
			if l.key.Fingerprint == fp {
				if l.options {
					restricted = append(restricted, l)
				} else {
					plain = append(plain, l)
				}
			}
		}
		return plain, restricted
	}

	if !found {
		out.Steps = append(out.Steps, Step{
			Describe: fmt.Sprintf("ssh: create %s (0600) in %s (0700), owned by %s", file, dir, user),
			Command: fmt.Sprintf("set -e; [ -d %[1]s ] || install -d -m 700 -o %[3]s -g %[4]s %[1]s; [ -f %[2]s ] || install -m 600 -o %[3]s -g %[4]s /dev/null %[2]s",
				shellQuote(dir), shellQuote(file), uid, gid),
		})
	}

	isListed := map[string]bool{}
	remaining := 0 // listed keys in the file once additions are done
	for _, k := range listed {
		isListed[k.Fingerprint] = true
		label := keyLabel(k.Fingerprint, k.Comment)
		plain, restricted := onHost(k.Fingerprint)
		switch {
		case len(plain) > 0 && isOwned(k.Fingerprint):
			remaining++
			out.Present = append(out.Present, fmt.Sprintf("ssh: key %s authorized for %s", label, user))
		case len(plain) > 0:
			// Already there, put there by someone else (cloud-init, most
			// often). Recorded rather than added again: a second copy of the
			// line would be one more thing for an operator to wonder about.
			remaining++
			setOwned(k.Fingerprint, k.Comment)
			out.Steps = append(out.Steps, Step{
				Label:    "adopt",
				Describe: fmt.Sprintf("ssh: key %s is already authorized for %s; record it as host prepare's", label, user),
				Command:  writeOwned(user, owned),
			})
		case len(restricted) > 0:
			// The key is there with options someone chose. It is theirs:
			// adding the plain key beside it would undo the restriction.
			remaining++
			out.Foreign = append(out.Foreign, fmt.Sprintf("ssh: key %s authorized for %s with options, by `%s`; left as it is", label, user, restricted[0].raw))
		default:
			remaining++
			setOwned(k.Fingerprint, k.Comment)
			out.Steps = append(out.Steps, Step{
				Label:    "add",
				Describe: fmt.Sprintf("ssh: authorize key %s for %s", label, user),
				Command:  appendKey(file, k.Line) + "; " + writeOwned(user, owned),
			})
		}
	}

	// Keys host prepare added that are no longer listed. Snapshot first:
	// the loop changes owned.
	previous := append([]ownedEntry(nil), owned...)
	for _, o := range previous {
		if isListed[o.fingerprint] {
			continue
		}
		label := keyLabel(o.fingerprint, o.comment)
		plain, restricted := onHost(o.fingerprint)
		if len(plain) == 0 {
			// Gone from the file already, or kept only with options someone
			// added since. Either way it is no longer host prepare's line.
			dropOwned(o.fingerprint)
			describe := fmt.Sprintf("ssh: forget key %s, which is no longer in %s", label, file)
			if len(restricted) > 0 {
				describe = fmt.Sprintf("ssh: forget key %s, which is now authorized with options by `%s` and so is no longer host prepare's", label, restricted[0].raw)
			}
			out.Steps = append(out.Steps, Step{Describe: describe, Command: writeOwned(user, owned)})
			continue
		}
		dropOwned(o.fingerprint)
		var raws []string
		for _, l := range plain {
			raws = append(raws, l.raw)
		}
		removals = append(removals, Step{
			Label:    "remove",
			Describe: fmt.Sprintf("ssh: remove key %s from %s, which host prepare added and ssh.public_key no longer lists", label, file),
			Command:  removeLines(file, raws) + "; " + writeOwned(user, owned),
		})
	}

	// The safety property, checked rather than assumed. Load guarantees a
	// listed key and every listed key is added above, so this cannot fire
	// today; it is here so that a later change to either cannot quietly plan
	// a file with no listed key left in it.
	if len(removals) > 0 && remaining == 0 {
		return out, nil, fmt.Errorf("ssh: removing %d key(s) from %s would leave %s with none of the keys ssh.public_key lists. Nothing was planned", len(removals), file, user)
	}
	return out, removals, nil
}

func keyLabel(fingerprint, comment string) string {
	if comment == "" {
		return fingerprint
	}
	return fingerprint + " (" + comment + ")"
}

// appendKey adds one line to authorized_keys, first ending the last line if
// someone left it without a newline, so the key is never glued onto it.
// Appending as root keeps the file's owner and mode.
func appendKey(file, line string) string {
	f := shellQuote(file)
	return fmt.Sprintf("set -e; if [ -s %[1]s ] && [ -n \"$(tail -c1 %[1]s)\" ]; then echo >> %[1]s; fi; printf '%%s\\n' %[2]s >> %[1]s",
		f, shellQuote(line))
}

// removeLines drops exactly these lines from authorized_keys and nothing else,
// through a temporary file given the original's owner and mode and renamed
// over it, so sshd never reads a half written file. grep exits 1 when no line
// is left, which here is an empty file rather than a failure.
func removeLines(file string, lines []string) string {
	f := shellQuote(file)
	tmp := shellQuote(file + ".paisans-tmp")
	var patterns []string
	for _, l := range lines {
		patterns = append(patterns, "-e "+shellQuote(l))
	}
	return fmt.Sprintf("set -e; grep -vxF %[3]s %[1]s > %[2]s || [ $? -eq 1 ]; chown --reference=%[1]s %[2]s; chmod --reference=%[1]s %[2]s; mv %[2]s %[1]s",
		f, tmp, strings.Join(patterns, " "))
}

// writeOwned replaces the sidecar with entries, sorted so that the file does
// not change when only the order of the configuration did. Fingerprints and
// comments are public, so a command line is no leak.
func writeOwned(user string, entries []ownedEntry) string {
	sorted := append([]ownedEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].fingerprint < sorted[j].fingerprint })
	body := "# Keys paisans host prepare added to " + user + "'s authorized_keys. It removes only these.\n"
	for _, e := range sorted {
		body += strings.TrimSpace(e.fingerprint+" "+e.comment) + "\n"
	}
	path := OwnedKeysPath(user)
	return fmt.Sprintf("mkdir -p %s && printf '%%s' %s > %s && chmod 600 %s && mv %s %s",
		ownedDir, shellQuote(body), shellQuote(path+".paisans-tmp"), shellQuote(path+".paisans-tmp"), shellQuote(path+".paisans-tmp"), shellQuote(path))
}

// shellQuote wraps a value in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
