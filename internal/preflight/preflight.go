// stub: replaced by the preflight branch
//
// Package preflight is site add's stage 1: read only checks on the new site
// and every existing one. This file is a placeholder with the agreed
// signature, so that site add builds and its tests run before the real
// checks are merged; it checks nothing and passes.
package preflight

import (
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// Check is one finding on one site.
type Check struct {
	Site, Name, Detail string
	Refused, Warned    bool
}

// Report is every check preflight ran.
type Report struct{ Checks []Check }

// Refused reports whether any check refused.
func (r Report) Refused() bool {
	for _, c := range r.Checks {
		if c.Refused {
			return true
		}
	}
	return false
}

// Run checks newSite and every existing site. transports maps site name to
// apply.Transport.
func Run(cfg *config.Config, newSite string, transports map[string]apply.Transport) (Report, error) {
	return Report{}, nil
}
