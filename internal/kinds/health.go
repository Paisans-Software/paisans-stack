package kinds

import "github.com/paisans-software/paisans-stack/internal/config"

// Health is the route a monitor requests to learn whether an app is up, and
// the status codes that mean it is. Expect is the uptime fork's own syntax: a
// comma separated list of exact codes, because the fork has no ranges
// (src/lib/checker.js:55-61 at 1.1.0-oidc.1).
type Health struct {
	Path   string
	Expect string
}

// health was established on 2026-10-07 from each pinned tag's source and,
// where it says so, by running the pinned image. None of it is recalled. A
// kind with no health route of its own gets `/`, which is a deeper check than
// a health route (it renders a page) and is said so beside it.
var health = map[config.Kind]Health{
	// The gateway's members gate on 4181 is a second instance of the same
	// binary and is not checked separately. /ping answers before any auth and
	// needs no Host (pkg/middleware/healthcheck.go:44-53 at v7.15.4); ran the
	// image: 200 OK.
	config.KindOAuth2Proxy: {"/ping", "200"},
	// The static client has no health route; /version is a cheap file the
	// image serves. Ran v1.12.27: /version 200, /health 404.
	config.KindElement: {"/version", "200"},
	// No health route at v1.13.3+paisans. / renders the front page through
	// Symfony and Postgres, and 302s to sign in when the instance is private.
	config.KindMbin: {"/", "200,302"},
	// Runs SELECT 1 and pings Redis, 500 when either fails
	// (server/main.ts:78-105 at v1.10.0).
	config.KindOutline: {"/_health", "200"},
	// 204 No Content, not 200 (backend/internal/controller/
	// healthz_controller.go:18,30-31 at v2.14.0); ran the image.
	config.KindPocketID: {"/healthz", "204"},
	// synapse/rest/health.py:38-50 at v1.160.0; ran the image: 200 OK.
	config.KindSynapse: {"/health", "200"},
	// No health route at sha-ff9dceb (routes.go). / renders sign in on a
	// private instance.
	config.KindWriteFreely: {"/", "200,302"},
	// src/server.js:63 at 1.1.0-oidc.1. Recorded for completeness: a monitor
	// does not watch itself.
	config.KindUptime: {"/healthz", "200"},
}

// HealthFor returns a kind's health route, false for a kind with none
// recorded.
func HealthFor(kind config.Kind) (Health, bool) {
	h, ok := health[kind]
	return h, ok
}
