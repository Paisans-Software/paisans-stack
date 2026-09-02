package render

import (
	"fmt"
	"net"
	"sort"
)

// meshPrefixLength is the size of the mesh network.
//
// The subnet has to be a property of the deployment rather than of the sites
// currently in it. Applications trust it for forwarded client addresses, and
// if it were computed as the tightest network containing today's addresses,
// adding a fourth site would widen it and silently change TRUSTED_PROXIES in
// every app. That is the retrofit the design exists to avoid, so the mesh is
// a fixed /24 with room to grow into.
const meshPrefixLength = 24

// meshSubnet returns the network every site address sits in.
//
// Applications are rendered to trust this subnet, never a specific host, which
// is what lets the gateway role move between machines later without breaking
// client address handling.
func meshSubnet(addresses []string) (string, error) {
	if len(addresses) == 0 {
		return "", fmt.Errorf("no site addresses to derive a mesh subnet from")
	}
	sorted := append([]string(nil), addresses...)
	sort.Strings(sorted)

	var network string
	for _, a := range sorted {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			return "", fmt.Errorf("site address %q is not an IPv4 address", a)
		}
		masked := ip.To4().Mask(net.CIDRMask(meshPrefixLength, 32))
		got := fmt.Sprintf("%s/%d", masked.String(), meshPrefixLength)
		if network == "" {
			network = got
			continue
		}
		if got != network {
			return "", fmt.Errorf(
				"site addresses span more than one /%d network (%s and %s). Every site shares one mesh subnet, because applications trust that subnet for forwarded client addresses and it must not change when a site is added",
				meshPrefixLength, network, got)
		}
	}
	return network, nil
}
