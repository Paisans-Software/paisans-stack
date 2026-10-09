package appremove

import (
	"fmt"
	"strings"
)

// Every line a removal leaves for the operator is written here. The text
// before its first ". " is the hint, so it holds only the names that are
// short by nature and the action or fact, never an ID, path or list: those
// follow the first sentence, on a line of their own under the hint, where a
// long one cannot push the hint past a line.

func clientKept(app, name, id, provider, why string) string {
	return fmt.Sprintf("a client kept; delete it in Pocket ID if it served only %s. It is %s (id %s) at %s: %s", app, name, id, provider, why)
}

func databaseNotApps(name string) string {
	return fmt.Sprintf("database and role kept; not a name apply creates for an app. They are named %s", name)
}

func databaseDeclared(name, declared string) string {
	return fmt.Sprintf("database and role kept; a declared app uses them. They are named %s, used by %s", name, declared)
}

func databaseForeign(name, leader, owner string) string {
	return fmt.Sprintf("a database kept; not provably this app's. It is %s on %s, owned by %s, not by the role apply creates for it", name, leader, owner)
}

func roleKept(name, leader string) string {
	return fmt.Sprintf("a role kept with the database above. It is %s on %s", name, leader)
}

func keyDeclared(id string) string {
	return fmt.Sprintf("an S3 key kept; a declared app records it too. It is %s", id)
}

func bucketUnread(name string) string {
	return fmt.Sprintf("a bucket kept; nothing about it is proven. It is %s, and `garage bucket info` did not read as expected", name)
}

func bucketGranted(name string, keys []string) string {
	return fmt.Sprintf("a bucket kept; it is also granted to %d other key(s). The bucket is %s, and the keys are %s, which this app does not record", len(keys), name, strings.Join(keys, ", "))
}

func bucketAliases(name string, global, local int) string {
	return fmt.Sprintf("a bucket kept; it has extra aliases. The bucket is %s, with %d global and %d key specific aliases, where storage init gives it one", name, global, local)
}

func dirEdited(site, dir string) string {
	return fmt.Sprintf("%s: stack directory kept, because a file in it was edited on the host. It is %s, with its data", site, dir)
}

func dirData(site, dir string, files int, bytes string) string {
	return fmt.Sprintf("%s: files kept; apply did not write them. They are in %s: %d file(s), %s", site, dir, files, bytes)
}

func fileEdited(site, path string) string {
	return fmt.Sprintf("%s: an edited file kept; delete it once nothing needs it, then run this again. It is %s, edited on the host since apply wrote it, so it stays with its manifest entry until it is deleted by hand", site, path)
}

func secretsLeft(app, keys string) string {
	return fmt.Sprintf("secrets: remove %s's entries from the secrets file with sops. This command never edits it, so remove them once nothing needs them: %s", app, keys)
}

const dnsLeft = "DNS: its hostnames' records stay; `paisans dns prune --execute` deletes them. dns init made them"

func dataKept(app string) string {
	return "data: kept; `paisans app remove " + app + " --delete-data --execute` deletes it. Kept: its database, its Garage bucket and key, its named volumes and what it wrote under its stack directory"
}
