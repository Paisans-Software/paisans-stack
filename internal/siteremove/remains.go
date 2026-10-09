package siteremove

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// Every line a removal leaves for the operator is written here. The text
// before its first ". " is the hint, so it holds only the site and the action
// or fact, never a path, ID or fingerprint: those follow the first sentence,
// on a line of their own under the hint, where a long one cannot push the
// hint past a line.

func containerKept(site, name string) string {
	return fmt.Sprintf("%s: container kept; it does not carry this deployment's label. It is %s", site, name)
}

func networkKept(site, name string) string {
	return fmt.Sprintf("%s: network kept; it does not carry this deployment's label. It is %s", site, name)
}

func volumeKept(site, name string) string {
	return fmt.Sprintf("%s: volume kept; --delete-data deletes it. It is %s, this deployment's data", site, name)
}

func editedFileKept(site, path string) string {
	return fmt.Sprintf("%s: an edited file kept; delete it by hand once nothing needs it. It is /%s, edited on the host since apply wrote it", site, path)
}

func sshAllowKept(site, rule string) string {
	return fmt.Sprintf("%s: SSH allow rule kept; delete it yourself once nothing logs in through it. With incoming denied, deleting `%s` cuts the next connection", site, rule)
}

func ufwRuleKept(site, rule string) string {
	return fmt.Sprintf("%s: ufw rule kept; it does not carry this deployment's tag. The rule is `%s`", site, rule)
}

func rootEditedKept(site, root string) string {
	return fmt.Sprintf("%s: the deployment directory kept, because a file in it was edited on the host. It is %s, with everything in it", site, root)
}

func dataLeft(site, root string, files int) string {
	return fmt.Sprintf("%s: data left on the host; --delete-data deletes it. It is in %s: %d file(s) apply did not write, the data in its bind mounts (Eg: a database, an object store)", site, root, files)
}

func keysNoPasswd(site, record, user string) string {
	return fmt.Sprintf("%s: SSH keys kept; the user has no passwd entry to find authorized_keys by. The user is %s, and the keys are those %s lists", site, user, record)
}

func keySharedKept(site, fingerprint, file string) string {
	return fmt.Sprintf("%s: an SSH key line kept; another deployment's record lists it too. It is key %s in %s: both added it as this one line", site, fingerprint, file)
}

func keyLinesKept(site, file, record, user string) string {
	return fmt.Sprintf("%s: SSH key lines kept; delete them yourself once another way in exists. They are in %s: deleting the key(s) %s lists would leave %s with no authorized key, and nobody could log in over SSH again", site, file, record, user)
}

func secretsLeft(site string) string {
	return fmt.Sprintf("secrets: remove sites.%s from the secrets file with sops. It is its WireGuard key, and this command never edits the file, so remove it once nothing needs it", site)
}

func dnsLeft(addr string) string {
	if addr == "" {
		addr = "this host"
	}
	return fmt.Sprintf("DNS: records pointing at this host stay; `paisans dns prune --execute` deletes them. They point at %s, and dns init made them", addr)
}

func ownedNote(site, owed string) string {
	return fmt.Sprintf("%s: run `paisans apply --site %s` when that is acceptable. %s", site, site, owed)
}

func handoverKept(site, dir string, env bool) []string {
	lines := []string{fmt.Sprintf("%s: Caddy handed over to the host's owner; paisans never touches it again. It is in %s for %s, and its data directory holds the certificates and keys of every hostname this deployment's Caddy served", site, dir, render.HostSitesDir)}
	if env {
		lines = append(lines, fmt.Sprintf("%s: DNS token left on the host; rotate it once the owner has their own. %s/caddy.env holds a copy of secrets external.acme_dns_token, a credential this deployment no longer controls, left for the owner. Rotate it at the DNS provider that issued it", site, dir))
	}
	return lines
}
