package kinds

import (
	"strconv"
	"strings"
)

// PostgresDataMount is where a pinned app's own Postgres container has its
// data directory bind mounted, given the image it runs and the deployment's
// cluster.postgres_version.
//
// The official image moved its declared volume in 18. Up to 17 it declares
// VOLUME /var/lib/postgresql/data and keeps PGDATA there. From 18 it declares
// VOLUME /var/lib/postgresql and sets PGDATA=/var/lib/postgresql/18/docker,
// so a cluster's data sits in a subdirectory named for its major version and
// `pg_upgrade --link` can see two versions under one mount. Both were read
// from postgres:16-alpine, 17-alpine and 18-alpine with `docker image
// inspect` on 2026-10-07.
//
// The old target is worse than leaky on 18. The image's entrypoint finds a
// mount at /var/lib/postgresql/data, takes it for a database of an older
// layout, and exits rather than start ("The suggested container
// configuration for 18+ is to place a single mount at /var/lib/postgresql"),
// which was observed with an empty bind there. So every pinned app at the
// default postgres_version of 18 rendered a database that could not start.
//
// The major is read from the image's tag when the image is the official one,
// because an app may pin an older image than the cluster's (the fixture's
// Synapse runs postgres:17-alpine beside a cluster at 18), and the mount has
// to match what the container runs. Any other image falls back to the
// cluster's version; `paisans apply` checks the result against what that
// image really declares.
func PostgresDataMount(image, clusterMajor string) string {
	major := clusterMajor
	if m, ok := officialPostgresMajor(image); ok {
		major = m
	}
	if n, err := strconv.Atoi(major); err == nil && n < 18 {
		return "/var/lib/postgresql/data"
	}
	return "/var/lib/postgresql"
}

// officialPostgresMajor is the leading number of an official postgres image's
// tag (Eg: 17 from postgres:17.6-alpine), false for any other image or a tag
// that does not start with one.
func officialPostgresMajor(image string) (string, bool) {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	repo, tag := ref, ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	switch repo {
	case "postgres", "library/postgres", "docker.io/postgres", "docker.io/library/postgres":
	default:
		return "", false
	}
	end := 0
	for end < len(tag) && tag[end] >= '0' && tag[end] <= '9' {
		end++
	}
	if end == 0 {
		return "", false
	}
	return tag[:end], true
}
