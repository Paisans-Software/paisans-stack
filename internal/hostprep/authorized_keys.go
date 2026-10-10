package hostprep

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// What follows makes the keys in a site's ssh.keys the login user's
// authorized keys, without taking the file over.
//
// authorized_keys is shared with whoever else manages the host: cloud-init
// puts the provider's key there, an operator may add a restricted key by
// hand, and several deployments may share the user. So host prepare only ever
// adds a listed key, and only ever removes a line it wrote itself that no other
// deployment claims. A listed key that was already there, and that no
// deployment claims, is left as it is and never recorded: it is often the key
// the operator logs in with.
//
// Each deployment records its claims in a sidecar file,
// /etc/paisans/authorized_keys.<user>.paisans-<token>.owned, root's and 0600,
// one `<mark> <fingerprint> <name>` line per key: `added` for a line this
// deployment appended, `shared` for one another deployment's sidecar listed
// when this one recorded it. The line in authorized_keys is left exactly as
// the key was written. Every write to a sidecar or to authorized_keys runs
// under one lock, and a removal checks the other sidecars again inside it.
//
// The commands use GNU coreutils (`chown --reference`) and util-linux's
// `flock`, which every Linux profile registered here has. authorized_keys itself is OpenSSH's format
// everywhere, which is why this is not behind Profile.

// ownedDir holds the sidecar files.
const ownedDir = "/etc/paisans"

// The marks a sidecar line starts with: a line this deployment appended, and
// one another deployment wrote that this one also needs.
const (
	markAdded  = "added"
	markShared = "shared"
)

// keysLock serialises every write to a sidecar or to authorized_keys on a
// host, across users and deployments, so a removal's check of the other
// sidecars still holds when it deletes.
const keysLock = ownedDir + "/authorized_keys.lock"

// Locked runs command under keysLock.
func Locked(command string) string {
	return fmt.Sprintf("mkdir -p %s && flock %s sh -c %s", ownedDir, shellQuote(keysLock), shellQuote(command))
}

// OwnedKeysGlob matches every deployment's sidecar for user.
func OwnedKeysGlob(user string) string {
	return ownedDir + "/authorized_keys." + user + ".paisans-*.owned"
}

// OwnedKeysPath is the sidecar recording which of user's authorized keys host
// prepare added for deployment d.
func OwnedKeysPath(d deployment.Deployment, user string) string {
	return ownedDir + "/authorized_keys." + user + "." + d.Prefix() + ".owned"
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

// ownedEntry is one line of the sidecar: a key this deployment claims, how,
// and what it calls the key.
type ownedEntry struct {
	mark        string
	fingerprint string
	name        string
}

// parseOwned reads a sidecar's `<mark> <fingerprint> <name>` lines. A line of
// any other shape is not a claim this deployment can act on.
func parseOwned(content string) []ownedEntry {
	var out []ownedEntry
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || (f[0] != markAdded && f[0] != markShared) || !strings.HasPrefix(f[1], "SHA256:") {
			continue
		}
		out = append(out, ownedEntry{mark: f[0], fingerprint: f[1], name: f[2]})
	}
	return out
}

// planAuthorizedKeys compares the listed keys with the user's authorized_keys,
// this deployment's sidecar and every other deployment's sidecar for the same
// user. It returns the additions and the changes to the sidecar, which run
// with the rest of the plan, and the removals, which run last: a key is only
// taken away once every listed key is in place.
func planAuthorizedKeys(t Transport, d deployment.Deployment, s config.SSH) (out Section, removals []Step, err error) {
	user := s.User
	listed, problems := s.AuthorizedKeys()
	if len(problems) > 0 {
		// Load refuses these; reaching here is a bug, not an operator error.
		return out, nil, fmt.Errorf("ssh.keys: %s", strings.Join(problems, "; "))
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

	own := OwnedKeysPath(d, user)
	sidecar, _, err := t.ReadFile(own)
	if err != nil {
		return out, nil, err
	}
	owned := parseOwned(sidecar)

	// Every other deployment's sidecar for this user, whole. A file claims a
	// key when the key's fingerprint appears anywhere in it, whatever the
	// line's shape, so a format this version does not read keeps a key.
	listing, err := t.Run(fmt.Sprintf(`for f in %s; do [ -f "$f" ] && echo "$f"; done; true`, OwnedKeysGlob(user)))
	if err != nil {
		return out, nil, err
	}
	var others []string // "<path>\n<content>" per sidecar, for claimedBy
	for _, path := range strings.Fields(listing) {
		if path == own {
			continue
		}
		c, _, err := t.ReadFile(path)
		if err != nil {
			return out, nil, err
		}
		others = append(others, path+"\n"+c)
	}
	claimedBy := func(fp string) string {
		for _, o := range others {
			path, c, _ := strings.Cut(o, "\n")
			if strings.Contains(c, fp) {
				return strings.TrimSuffix(strings.TrimPrefix(path, ownedDir+"/authorized_keys."+user+"."), ".owned")
			}
		}
		return ""
	}

	find := func(fp string) int {
		for i, o := range owned {
			if o.fingerprint == fp {
				return i
			}
		}
		return -1
	}
	drop := func(fp string) {
		if i := find(fp); i >= 0 {
			owned = append(owned[:i:i], owned[i+1:]...)
		}
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
			Title:    "create authorized_keys",
			Describe: fmt.Sprintf("ssh: create %s (0600) in %s (0700), owned by %s", file, dir, user),
			Command: fmt.Sprintf("set -e; [ -d %[1]s ] || install -d -m 700 -o %[3]s -g %[4]s %[1]s; [ -f %[2]s ] || install -m 600 -o %[3]s -g %[4]s /dev/null %[2]s",
				shellQuote(dir), shellQuote(file), uid, gid),
		})
	}

	isListed := map[string]bool{}
	remaining := 0 // listed keys in the file once additions are done
	for _, k := range listed {
		isListed[k.Fingerprint] = true
		label := keyLabel(k.Fingerprint, k.Name)
		plain, restricted := onHost(k.Fingerprint)
		i := find(k.Fingerprint)
		switch {
		case len(plain) > 0 && i >= 0:
			remaining++
			out.Present = append(out.Present, fmt.Sprintf("ssh: key %s authorized for %s", label, user))
			if owned[i].name != k.Name {
				was := owned[i].name
				owned[i].name = k.Name
				out.Steps = append(out.Steps, Step{
					Label:    "rename",
					Title:    "rename ssh key",
					Describe: fmt.Sprintf("ssh: record key %s as %s, which ssh.keys called %s", k.Fingerprint, k.Name, was),
					Command:  Locked(writeOwned(d, user, owned)),
				})
			}
		case len(plain) > 0 && claimedBy(k.Fingerprint) != "":
			// Already there, and another deployment claims it: paisans wrote
			// it. Claimed here too, so that deployment cannot remove it from
			// under this one, but shared, so this one never removes it.
			remaining++
			owned = append(owned, ownedEntry{mark: markShared, fingerprint: k.Fingerprint, name: k.Name})
			out.Steps = append(out.Steps, Step{
				Label:    "share",
				Title:    "share ssh key",
				Describe: fmt.Sprintf("ssh: key %s is already authorized for %s and %s claims it; claim it too, so it stays while either lists it", label, user, claimedBy(k.Fingerprint)),
				Command:  Locked(writeOwned(d, user, owned)),
			})
		case len(plain) > 0:
			// Already there, put there by someone else (cloud-init, most
			// often), and often the key this run logs in with. Neither added
			// again nor recorded, so host prepare never removes it.
			remaining++
			out.Present = append(out.Present, fmt.Sprintf("ssh: key %s authorized for %s, not by host prepare, which never removes it", label, user))
		case len(restricted) > 0:
			// The key is there with options someone chose. It is theirs:
			// adding the plain key beside it would undo the restriction.
			remaining++
			out.Foreign = append(out.Foreign, fmt.Sprintf("ssh: key %s authorized for %s with options, by `%s`; left as it is", label, user, restricted[0].raw))
		default:
			remaining++
			drop(k.Fingerprint)
			owned = append(owned, ownedEntry{mark: markAdded, fingerprint: k.Fingerprint, name: k.Name})
			out.Steps = append(out.Steps, Step{
				Label:    "add",
				Title:    "authorize ssh key",
				Describe: fmt.Sprintf("ssh: authorize key %s for %s", label, user),
				Command:  Locked(appendKey(file, k.Line) + "; " + writeOwned(d, user, owned)),
			})
		}
	}

	// Keys this deployment claims that are no longer listed. Snapshot first:
	// the loop changes owned.
	previous := append([]ownedEntry(nil), owned...)
	for _, o := range previous {
		if isListed[o.fingerprint] {
			continue
		}
		label := keyLabel(o.fingerprint, o.name)
		plain, restricted := onHost(o.fingerprint)
		drop(o.fingerprint)
		switch {
		case len(plain) == 0:
			// Gone from the file already, or kept only with options someone
			// added since. Either way it is no longer host prepare's line.
			describe := fmt.Sprintf("ssh: forget key %s, which is no longer in %s", label, file)
			if len(restricted) > 0 {
				describe = fmt.Sprintf("ssh: forget key %s, which is now authorized with options by `%s` and so is no longer host prepare's", label, restricted[0].raw)
			}
			out.Steps = append(out.Steps, Step{Title: "forget ssh key", Describe: describe, Command: Locked(writeOwned(d, user, owned))})
		case o.mark == markShared:
			out.Steps = append(out.Steps, Step{
				Label:    "release",
				Title:    "release ssh key",
				Describe: fmt.Sprintf("ssh: stop claiming key %s, which ssh.keys no longer lists; another deployment wrote it, so its line stays in %s", label, file),
				Command:  Locked(writeOwned(d, user, owned)),
			})
		case claimedBy(o.fingerprint) != "":
			out.Steps = append(out.Steps, Step{
				Label:    "release",
				Title:    "release ssh key",
				Describe: fmt.Sprintf("ssh: stop claiming key %s, which ssh.keys no longer lists; %s still claims it, so its line stays in %s", label, claimedBy(o.fingerprint), file),
				Command:  Locked(writeOwned(d, user, owned)),
			})
		default:
			var raws []string
			for _, l := range plain {
				raws = append(raws, l.raw)
			}
			removals = append(removals, Step{
				Label:    "remove",
				Title:    "remove ssh key",
				Describe: fmt.Sprintf("ssh: remove key %s from %s, which host prepare added, ssh.keys no longer lists and no other deployment claims", label, file),
				Command:  Locked(removeUnclaimed(user, own, o.fingerprint, file, raws, writeOwned(d, user, owned))),
			})
		}
	}

	// The safety property, checked rather than assumed. Load guarantees a
	// listed key and every listed key is added above, so this cannot fire
	// today; it is here so that a later change to either cannot quietly plan
	// a file with no listed key left in it.
	if len(removals) > 0 && remaining == 0 {
		return out, nil, fmt.Errorf("ssh: removing %d key(s) from %s would leave %s with none of the keys ssh.keys lists. Nothing was planned", len(removals), file, user)
	}
	return out, removals, nil
}

func keyLabel(fingerprint, name string) string {
	if name == "" {
		return fingerprint
	}
	return fingerprint + " (" + name + ")"
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

// removeUnclaimed deletes lines, a key's plain lines, from authorized_keys
// only if no other sidecar for user then contains its fingerprint, and writes
// this deployment's sidecar either way. It runs under the lock, so the check
// still holds when it deletes; a sidecar grep cannot read counts as a claim.
func removeUnclaimed(user, own, fingerprint, file string, lines []string, write string) string {
	return fmt.Sprintf(`claimed=0; for f in %s; do [ "$f" = %s ] && continue; [ -e "$f" ] || continue; grep -qF -- %s "$f"; [ $? -eq 1 ] || claimed=1; done; if [ $claimed -eq 0 ]; then %s; fi; %s`,
		OwnedKeysGlob(user), shellQuote(own), shellQuote(fingerprint), removeLines(file, lines), write)
}

// writeOwned replaces the sidecar with entries, sorted so that the file does
// not change when only the order of the configuration did. Fingerprints and
// comments are public, so a command line is no leak.
func writeOwned(d deployment.Deployment, user string, entries []ownedEntry) string {
	sorted := append([]ownedEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].fingerprint < sorted[j].fingerprint })
	body := "# Keys this deployment claims in " + user + "'s authorized_keys: <mark> <fingerprint> <name>. host prepare removes a line only when it is marked added here and no other deployment's file lists it.\n"
	for _, e := range sorted {
		body += e.mark + " " + e.fingerprint + " " + e.name + "\n"
	}
	path := OwnedKeysPath(d, user)
	return fmt.Sprintf("mkdir -p %s && printf '%%s' %s > %s && chmod 600 %s && mv %s %s",
		ownedDir, shellQuote(body), shellQuote(path+".paisans-tmp"), shellQuote(path+".paisans-tmp"), shellQuote(path+".paisans-tmp"), shellQuote(path))
}

// shellQuote wraps a value in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
