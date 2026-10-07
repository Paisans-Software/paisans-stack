package kinds

import "github.com/paisans-software/paisans-stack/internal/config"

// PocketIDStandbyMarker records, per Pocket ID image, the text its log carries
// when it refuses to start because another instance already holds the
// database. The standby wrapper the pocket-id kind renders (README, "Pocket ID
// runs on every apps site, and one of them is active") stands by on an exit
// whose last lines contain it, and treats every other exit as a failure.
//
// v2.14.0: bootstrap.Bootstrap turns francis's ErrClusterFull into "it
// appears that there's already one instance of Pocket ID running - running
// multiple replicas is not (yet) supported"
// (backend/internal/bootstrap/bootstrap.go:126-128), and the root command logs
// it under "error" and exits 1 (backend/internal/cmds/root.go:21-24). The part
// kept here has no apostrophe, so neither slog's quoting nor the wrapper's own
// single quotes can change it.
//
// Keyed by the whole reference, as ImageVolumes is: a bump changes the
// reference, the new one has no entry, and a test fails until somebody reads
// the new release's bootstrap.go and records its text. A marker that stops
// matching fails safe, not silently: the wrapper exits, Docker's restart
// policy retries the container more slowly, and apply's gate reports it as
// restarting.
var PocketIDStandbyMarker = map[string]string{
	"ghcr.io/pocket-id/pocket-id:v2.14.0": "already one instance of Pocket ID running",
}

// StandbyMarker is the refusal text for a Pocket ID image, and whether the
// table knows that image. An image it does not know, such as an operator's
// own `images.app`, gets the default image's text: the most likely to match,
// and harmless when it does not.
func StandbyMarker(image string) (string, bool) {
	if marker, ok := PocketIDStandbyMarker[image]; ok {
		return marker, true
	}
	def, _ := DefaultImage(config.KindPocketID, "app")
	return PocketIDStandbyMarker[def], false
}
