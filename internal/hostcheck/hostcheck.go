// Package hostcheck decides, before the toolkit changes a host, whether that
// host is the toolkit's alone, shared with something else, or holding
// something the site is about to claim.
//
// It has three parts, and each is testable alone. Claims is what
// paisans.yaml says the site will take on its host, with no host contact.
// Inventory is what the host already runs, read through the transport and
// never changed. Classify decides who owns each thing found and compares the
// two.
//
// The class is computed on every run and never stored: a host becomes shared
// the day someone installs something beside the toolkit, and a stored answer
// would be wrong from that day on.
package hostcheck

import "github.com/paisans-software/paisans-stack/internal/config"

// Run computes the site's claims, inventories its host and classifies it.
// It changes nothing.
func Run(cfg *config.Config, site string, t Transport) (*Report, error) {
	claims, err := ClaimsFor(cfg, site)
	if err != nil {
		return nil, err
	}
	inv, err := Inspect(t)
	if err != nil {
		return nil, err
	}
	return Classify(claims, inv), nil
}
