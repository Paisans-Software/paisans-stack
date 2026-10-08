package render

// HostSitesDir is the gateway host's directory of Caddy site blocks that the
// host's owner adds for things paisans does not run, Eg: a blog beside the
// community. The gateway's Caddy mounts it read only at HostSitesMount and
// the Caddyfile imports every *.caddy file in it at the top level.
//
// It is the host's, not a deployment's: its path carries no token, because a
// host has one gateway (internal/registry refuses a second) and the owner's
// sites outlive any one deployment. apply creates it when it is missing and
// never writes, records or removes anything inside it.
const HostSitesDir = "/srv/caddy.d"

// HostSitesMount is where the gateway's Caddy sees HostSitesDir.
const HostSitesMount = "/etc/caddy.d"
