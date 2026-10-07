package validate

import (
	"fmt"
	"net"
)

// cgnat is RFC 6598 shared address space, 100.64.0.0/10. net.IP.IsPrivate
// covers RFC 1918 and RFC 4193 but not this range, and a site behind carrier
// grade NAT is the commonest way an operator ends up holding an address that
// looks public on the router's status page and is not.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// publicAddress refuses a site's public_address or public_address6 that the
// internet cannot reach.
//
// The field exists for one purpose: it is the content of the records `paisans
// dns` publishes. A private, loopback, carrier grade NAT or mesh address
// published there sends every visitor to an address that is either nobody's
// or somebody else's, and DNS caches the mistake for as long as the record's
// TTL. It would be simpler to leave the check to `dns` itself, but the value is
// wrong whether or not `dns` is ever run, and a wrong value in a declaration is
// what validate exists to report.
//
// The absence of the field is not checked here. A deployment may manage its
// DNS some other way, so only `dns` requires it, and only where a record needs
// it.
func (c *checker) publicAddress() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if site.PublicAddress != "" {
			key := fmt.Sprintf("sites.%s.public_address", name)
			ip := net.ParseIP(site.PublicAddress)
			switch {
			case ip == nil || ip.To4() == nil:
				c.refuse("public-address-is-not-ipv4", key,
					"is %q, which is not an IPv4 address. It is published as an A record, so it must be the literal address the internet reaches this site on, for example 203.0.113.10. An IPv6 address goes in public_address6.",
					site.PublicAddress)
			default:
				if why := notPublic(ip, c.cfg.Mesh.Contains(site.PublicAddress)); why != "" {
					c.refuse("public-address-is-not-public", key,
						"is %s, which %s. It is published in DNS for the whole internet to dial, so it must be the address the internet actually reaches this site on, as the router or the cloud provider reports it rather than as the host's own interface shows it.",
						site.PublicAddress, why)
				}
			}
		}
		if site.PublicAddress6 != "" {
			key := fmt.Sprintf("sites.%s.public_address6", name)
			ip := net.ParseIP(site.PublicAddress6)
			switch {
			case ip == nil || ip.To4() != nil:
				c.refuse("public-address-is-not-ipv6", key,
					"is %q, which is not an IPv6 address. It is published as an AAAA record, so it must be an IPv6 literal, for example 2001:db8::10. An IPv4 address goes in public_address.",
					site.PublicAddress6)
			default:
				if why := notPublic(ip, false); why != "" {
					c.refuse("public-address-is-not-public", key,
						"is %s, which %s. It is published in DNS for the whole internet to dial, so it must be a global unicast address this site is reachable on.",
						site.PublicAddress6, why)
				}
			}
		}
	}
}

// notPublic says why an address cannot be reached from the internet, or
// returns nothing when it can. Documentation ranges (203.0.113.0/24,
// 2001:db8::/32) are accepted on purpose: examples and tests use them, and
// refusing them would make the example configuration fail its own check.
func notPublic(ip net.IP, inMesh bool) string {
	switch {
	case inMesh:
		return "is inside mesh.subnet. The mesh is the private tunnel between sites; nothing outside it can route there"
	case ip.IsLoopback():
		return "is a loopback address, which every machine answers for itself"
	case ip.IsPrivate():
		return "is private address space (RFC 1918 or RFC 4193), which the internet does not route"
	case cgnat.Contains(ip):
		return "is carrier grade NAT space (RFC 6598, 100.64.0.0/10). The provider shares the real public address between customers, so nothing can dial this one from outside"
	case ip.IsLinkLocalUnicast():
		return "is link local, which does not leave the local network segment"
	case ip.IsUnspecified(), ip.IsMulticast(), !ip.IsGlobalUnicast():
		return "is not a unicast address any host can be reached on"
	}
	return ""
}
