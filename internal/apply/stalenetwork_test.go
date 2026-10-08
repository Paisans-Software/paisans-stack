package apply

import "testing"

// Only declared IPv4 subnets are compared, and a gateway only where the
// compose file declares one, so what Docker fills in for itself never takes
// a stack down.
func TestStaleNetworkComparesDeclaredIPv4Subnets(t *testing.T) {
	for _, tc := range []struct {
		name           string
		declared, live []pool
		stale          bool
	}{
		{"no gateway declared", []pool{{Subnet: "10.255.255.0/29"}}, []pool{{Subnet: "10.255.255.0/29", Gateway: "10.255.255.1"}}, false},
		{"gateway differs", []pool{{Subnet: "10.255.255.0/29", Gateway: "10.255.255.1"}}, []pool{{Subnet: "10.255.255.0/29", Gateway: "10.255.255.6"}}, true},
		{"subnet differs", []pool{{Subnet: "10.255.255.0/29"}}, []pool{{Subnet: "172.18.0.0/16"}}, true},
		{"ipv6 beside", []pool{{Subnet: "10.255.255.0/29"}}, []pool{{Subnet: "fd00:1::/64"}, {Subnet: "10.255.255.0/29"}}, false},
		{"ipv6 declared", []pool{{Subnet: "10.255.255.0/29"}, {Subnet: "fd00:2::/64"}}, []pool{{Subnet: "10.255.255.0/29"}}, false},
		{"unpinned on docker's pool", nil, []pool{{Subnet: "172.18.0.0/16"}, {Subnet: "fd00:1::/64"}}, false},
		{"unpinned on the old pin", nil, []pool{{Subnet: "10.255.255.0/29"}}, true},
	} {
		if got := staleNetwork(tc.declared, tc.live) != ""; got != tc.stale {
			t.Errorf("%s: stale %v, want %v", tc.name, got, tc.stale)
		}
	}
}
