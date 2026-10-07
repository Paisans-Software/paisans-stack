package config_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The units are Garage's, as dxflrs/garage:v1.0.1 displays them back:
// decimal for G and GB, binary for GiB.
func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"3G": 3e9, "3GB": 3e9, "3gb": 3e9, "3GiB": 3 << 30, "1.5T": 1.5e12, "500M": 5e8, "100G": 1e11,
	} {
		got, err := config.ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "500", "0G", "G", "3 parsecs", "-1G"} {
		if _, err := config.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should be refused", bad)
		}
	}
}

func TestCapacityForFallsBackToTheDefault(t *testing.T) {
	g := config.Garage{Capacity: "100G", Capacities: map[string]string{"store": "2T"}}
	if g.CapacityFor("store") != "2T" || g.CapacityFor("home-a") != "100G" {
		t.Errorf("CapacityFor: store %q, home-a %q", g.CapacityFor("store"), g.CapacityFor("home-a"))
	}
}
