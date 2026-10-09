package doctor

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ClockCommand prints the host's time as seconds and nanoseconds since the
// epoch (GNU date's %s and %N).
const ClockCommand = "date +%s.%N"

// maxDrift is where a clock is reported. etcd's peer prober logs "prober
// found high clock drift" past one second (etcd v3.5.16,
// server/etcdserver/api/rafthttp/probing_status.go, `s.ClockDiff() >
// time.Second`), and etcd's leases and Patroni's leader key expire by time.
const maxDrift = time.Second

// ClockSample is one site's clock, with the workstation's clock read just
// before and just after asking.
type ClockSample struct {
	Site          string
	Before, After time.Time
	Out           string
	Err           string
}

// Offset is the host's clock minus the workstation's at the moment the host
// answered, taken as the middle of the round trip, and the round trip, which
// bounds how wrong that guess can be.
func (c ClockSample) Offset() (offset, roundTrip time.Duration, err error) {
	remote, err := strconv.ParseFloat(strings.TrimSpace(c.Out), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unreadable time %q", firstLine(c.Out))
	}
	roundTrip = c.After.Sub(c.Before)
	middle := c.Before.Add(roundTrip / 2)
	sec, frac := math.Modf(remote)
	host := time.Unix(int64(sec), int64(frac*1e9))
	return host.Sub(middle), roundTrip, nil
}

// Clocks compares each site's clock with the workstation's.
func Clocks(samples []ClockSample) []Finding {
	var out []Finding
	for _, c := range samples {
		if c.Err != "" {
			out = append(out, Finding{Section: SectionClocks, Level: Warn, Line: fmt.Sprintf("%s: could not read the clock", c.Site), More: []string{firstLine(c.Err)}})
			continue
		}
		offset, rtt, err := c.Offset()
		if err != nil {
			out = append(out, Finding{Section: SectionClocks, Level: Warn, Line: fmt.Sprintf("%s: %v", c.Site, err)})
			continue
		}
		dir := "ahead of"
		if offset < 0 {
			dir = "behind"
		}
		abs := offset.Abs()
		line := fmt.Sprintf("%s: %.2f s %s this workstation (round trip %.2f s)", c.Site, abs.Seconds(), dir, rtt.Seconds())
		if abs <= maxDrift {
			out = append(out, Finding{Section: SectionClocks, Level: OK, Line: line})
			continue
		}
		out = append(out, Finding{Section: SectionClocks, Level: Warn, Line: line,
			More: []string{
				"etcd's peer prober reports high clock drift past 1 s (etcd v3.5.16, server/etcdserver/api/rafthttp/probing_status.go), and etcd's leases and Patroni's leader key expire by time.",
				"Check time sync on the host with `timedatectl`. A suspended VM's clock stops while it is paused. If every site is off by the same amount, the workstation's own clock is the one to check.",
			}})
	}
	return out
}
