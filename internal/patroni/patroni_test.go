package patroni

import "testing"

// The document is Patroni v4.1.0's own /cluster example (docs/rest_api.rst),
// with the replica made a Sync Standby and a third member that reported no
// position.
const document = `{"members":[
 {"name":"home-a","role":"leader","state":"running","timeline":5},
 {"name":"home-b","role":"sync_standby","state":"streaming","lag":0},
 {"name":"home-c","role":"replica","state":"starting","lag":"unknown"}],
 "scope":"demo"}`

func TestParse(t *testing.T) {
	c, err := Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := c.Leader(); !ok || l.Name != "home-a" {
		t.Errorf("leader = %+v %v", l, ok)
	}
	if !c.HasSyncStandby() {
		t.Error("no sync standby")
	}
	b, _ := c.Member("home-b")
	if n, ok := b.LagBytes(); !ok || n != 0 {
		t.Errorf("home-b lag = %d %v", n, ok)
	}
	cc, _ := c.Member("home-c")
	if _, ok := cc.LagBytes(); ok || cc.LagText() != "unknown" {
		t.Errorf("home-c lag should be unknown, got %s", cc.LagText())
	}
	a, _ := c.Member("home-a")
	if _, ok := a.LagBytes(); ok {
		t.Error("the leader has no lag")
	}
}

func TestNoLeaderWhileStarting(t *testing.T) {
	c, _ := Parse(`{"members":[{"name":"home-a","role":"leader","state":"starting"}]}`)
	if _, ok := c.Leader(); ok {
		t.Error("a leader that is not running is no leader")
	}
	if _, err := Parse("curl: (7) Failed to connect"); err == nil {
		t.Error("garbage parsed")
	}
}

func TestParseSize(t *testing.T) {
	if n, err := ParseSize(" 1073741824\n"); err != nil || n != 1<<30 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := ParseSize(""); err == nil {
		t.Error("empty size parsed")
	}
}
