package validate

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// One owner binding the same port twice for two different keys still cannot
// start: an app published on its mesh address and again on an ingress listen
// at that same address and port is one bind refused, whoever owns both.
// Only the same owner and key, which is one listener named twice (a service
// on its mesh address and on loopback never overlaps), is not a collision.
func TestCollidesComparesOwnerAndKey(t *testing.T) {
	mesh := render.Listener{Owner: "app status (uptime)", Key: "apps.status", Proto: "tcp", Address: "10.44.0.4", Port: 3001}
	listen := render.Listener{Owner: "app status (uptime)", Key: "sites.watch.ingress.listen", Proto: "tcp", Address: "10.44.0.4", Port: 3001}
	if !collides(mesh, listen) {
		t.Error("the same owner under two keys on one address and port was not a collision")
	}
	if collides(mesh, mesh) {
		t.Error("a listener collided with itself")
	}
	other := render.Listener{Owner: "Postgres", Key: "sites.watch.roles (data)", Proto: "tcp", Address: "", Port: 3001}
	if !collides(mesh, other) {
		t.Error("two owners on one port, one on every address, did not collide")
	}
}
