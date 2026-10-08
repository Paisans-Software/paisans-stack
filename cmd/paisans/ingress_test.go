package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/ingress"
)

func TestIngressShowPrintsTheSheet(t *testing.T) {
	var err error
	out := captureStdout(t, func() { err = runIngress([]string{"show", "--app", "status", "--config", fixtureConfig()}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to hand off") {
		t.Fatalf("the fixture's monitor runs the toolkit's Caddy:\n%s", out)
	}
}

func TestIngressRefusesAnAppTheGatewayServes(t *testing.T) {
	if err := runIngress([]string{"show", "--app", "talk", "--config", fixtureConfig()}); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("err = %v", err)
	}
}

func TestIngressRefusesAnUnknownSubcommand(t *testing.T) {
	if err := runIngress([]string{"fix", "--app", "status"}); err == nil || !strings.Contains(err.Error(), "show or check") {
		t.Fatalf("err = %v", err)
	}
	if err := runIngress([]string{"check", "--config", fixtureConfig()}); err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("err = %v", err)
	}
}

type noDNS struct{}

func (noDNS) LookupHost(context.Context, string) ([]string, error) {
	return nil, errors.New("no such host")
}

// check fails, and exits non zero, when an item fails. The probes reach
// nothing: the resolver knows no names and every connection is refused.
func TestIngressCheckFailsOnAFailedItem(t *testing.T) {
	saved := ingressProbes
	t.Cleanup(func() { ingressProbes = saved })
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	ingressProbes = func() ingress.Probes {
		return ingress.Probes{
			Resolver: noDNS{},
			Client:   &http.Client{Transport: &http.Transport{DialContext: refuse}},
			Dial:     refuse,
			Now:      time.Now,
		}
	}
	var err error
	out := captureStdout(t, func() { err = runIngress([]string{"check", "--app", "status", "--config", fixtureConfig()}) })
	if err == nil || !strings.Contains(out, "FAIL  dns") || !strings.Contains(out, "FAIL  certificate") {
		t.Fatalf("err = %v\n%s", err, out)
	}
}
