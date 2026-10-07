package main

import (
	"strings"
	"testing"
)

// The site is required, may stand before or after the flags, and is one.
// Each refusal comes before the configuration is read, so no host is reached.
func TestRunSiteAddArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "name the site"},
		{[]string{"--execute"}, "name the site"},
		{[]string{"home-b", "home-c"}, "one site at a time"},
		{[]string{"home-b", "--config", "/nonexistent/paisans.yaml"}, "nonexistent"},
		{[]string{"--config", "/nonexistent/paisans.yaml", "home-b"}, "nonexistent"},
	} {
		err := runSiteAdd(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: want an error containing %q, got %v", tc.args, tc.want, err)
		}
	}
}
