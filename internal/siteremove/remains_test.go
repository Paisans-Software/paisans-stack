package siteremove

import "testing"

func TestDataLeftIsOneLine(t *testing.T) {
	got := dataLeft("home-a", "/srv/paisans/f2a9")
	want := "home-a: data left on the host in /srv/paisans/f2a9; --delete-data deletes it"
	if got != want {
		t.Errorf("dataLeft = %q, want %q", got, want)
	}
}
