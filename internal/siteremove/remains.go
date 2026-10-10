package siteremove

import (
	"fmt"
	"strings"

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

func imageKept(site, id, why string) string {
	return fmt.Sprintf("%s: image kept; remove it yourself once nothing runs from it. It is %s: %s", site, id, why)
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

func keysOtherUserKept(site, record, reach string) string {
	return fmt.Sprintf("%s: another user's SSH keys kept; clean them by running again as that user. The record is %s; run again with --ssh %s", site, record, reach)
}

func recordMissed(gw, why string) string {
	return fmt.Sprintf("%s: deployment record not updated; the next change catches it up. %s", gw, why)
}

func recordRaced(gw, site, why string) string {
	return fmt.Sprintf("%s: deployment record changed meanwhile; take the site out again. Another command wrote it between the read and the write, so it may still list %s: run paisans site remove %s --force --ssh <its host> once this removal is done. %s", gw, site, site, why)
}

func secretsLeft(site string) string {
	return fmt.Sprintf("secrets: run `paisans secrets prune` once nothing needs sites.%s. It holds its WireGuard key and heartbeat token, and this command never edits the secrets file", site)
}

func dnsLeft(addr string) string {
	if addr == "" {
		addr = "the address this host had"
	}
	return fmt.Sprintf("DNS: records pointing at this host stay; delete them at the DNS provider by hand. They point at %s, and dns init made them; dns prune deletes only a record whose address paisans.yaml still declares", addr)
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

func handoverNotRendered(site string, hostSites []string) string {
	if len(hostSites) == 0 {
		return fmt.Sprintf("%s: Caddy not handed over, and nothing relied on it. %s held no *.caddy file, so this deployment's Caddy is removed with its other containers", site, render.HostSitesDir)
	}
	return fmt.Sprintf("%s: Caddy not handed over; the owner's sites in %s stop being served. Without paisans.yaml its Caddyfile cannot be rendered, so this deployment's Caddy is removed with its other containers. Each of %s needs a Caddy of the owner's own", site, render.HostSitesDir, strings.Join(hostSites, ", "))
}

func secretsNotRead(site string) string {
	return fmt.Sprintf("secrets: not read; `paisans secrets prune` removes sites.%s from a surviving copy. This command read no paisans.yaml and no secrets file", site)
}

func otherHostsLeft(id string) string {
	return fmt.Sprintf("other hosts: not reached; run this command on each host the deployment used. Each keeps its registry entry and what it ran of deployment %s, and a gateway's deployment record still lists this site", id)
}

func pocketIDNotChecked(site string) string {
	return fmt.Sprintf("Pocket ID: not checked; sign in may stop until a standby takes over. Without paisans.yaml it is not known whether %s held the active instance", site)
}
