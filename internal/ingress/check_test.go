package ingress_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ingress"
)

// now is the clock every check runs at, so days to expiry are exact.
var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// resolver answers from a map, and never asks real DNS.
type resolver map[string][]string

func (r resolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if addrs, ok := r[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// selfSigned is a certificate for name, valid until notAfter, that is its own
// root, so a client trusting it verifies exactly the name check.
func selfSigned(t *testing.T, name string, notAfter time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// world is a web server in front of the monitor, reached whatever the
// hostname resolves to, plus the resolver and dialer the checks use.
type world struct {
	certName   string
	notAfter   time.Time
	resolves   []string
	location   string // where /login/oidc redirects
	health     int
	upstreamUp bool
	// dialErr is what a refused dial returns; nil means connection refused.
	dialErr error
	// dialled records every address the upstream check dialled.
	dialled *[]string
}

func good() world {
	return world{
		certName: "status.example.org",
		notAfter: now.Add(60 * 24 * time.Hour),
		resolves: []string{"203.0.113.20"},
		location: "https://id.example.org/authorize?client_id=c&redirect_uri=https%3A%2F%2Fstatus.example.org%2Flogin%2Foidc%2Fcallback&response_type=code",
		health:   200,
	}
}

func (w world) probes(t *testing.T) ingress.Probes {
	t.Helper()
	cert, pool := selfSigned(t, w.certName, w.notAfter)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			rw.WriteHeader(w.health)
		case "/login/oidc":
			http.Redirect(rw, r, w.location, http.StatusFound)
		default:
			http.NotFound(rw, r)
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	// A refused handshake is what several tests are about; the server's own
	// log of it is noise.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	address := server.Listener.Addr().String()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, Time: func() time.Time { return now }},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return ingress.Probes{
		Resolver: resolver{"status.example.org": w.resolves},
		Client:   &http.Client{Transport: transport, Timeout: 5 * time.Second},
		Dial: func(_ context.Context, _, address string) (net.Conn, error) {
			if w.dialled != nil {
				*w.dialled = append(*w.dialled, address)
			} else if address != "203.0.113.20:8480" {
				t.Errorf("dialled %s, want the public address and the listen port", address)
			}
			if w.upstreamUp {
				a, b := net.Pipe()
				b.Close()
				return a, nil
			}
			if w.dialErr != nil {
				return nil, w.dialErr
			}
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		},
		Now: func() time.Time { return now },
	}
}

func run(t *testing.T, w world, target ingress.Target) []ingress.Result {
	t.Helper()
	return ingress.Check(context.Background(), target, w.probes(t))
}

func result(t *testing.T, results []ingress.Result, name string) ingress.Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", name, results)
	return ingress.Result{}
}

func TestCheckPassesAWellConfiguredProxy(t *testing.T) {
	results := run(t, good(), external(t, "127.0.0.1:8480"))
	if len(results) != 4 {
		t.Fatalf("want four checks, got %+v", results)
	}
	for _, r := range results {
		if !r.OK || r.Fix != "" {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
	if d := result(t, results, ingress.CheckCertificate).Detail; !strings.Contains(d, "60 more days") {
		t.Errorf("days to expiry not printed: %s", d)
	}
	var buf bytes.Buffer
	if ingress.Print(&buf, results) {
		t.Errorf("Print reported a failure:\n%s", buf.String())
	}
}

func TestCheckFailsWhenTheNameDoesNotResolveToTheSite(t *testing.T) {
	w := good()
	w.resolves = []string{"203.0.113.10"}
	r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckDNS)
	if r.OK || !strings.Contains(r.Detail, "203.0.113.10") || !strings.Contains(r.Fix, "203.0.113.20") || !strings.Contains(r.Fix, "paisans dns init") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckFailsACertificateForAnotherName(t *testing.T) {
	w := good()
	w.certName = "other.example.org"
	results := run(t, w, external(t, "127.0.0.1:8480"))
	r := result(t, results, ingress.CheckCertificate)
	if r.OK || !strings.Contains(r.Detail, "not valid for status.example.org") || !strings.Contains(r.Fix, "YOUR CERTIFICATE") {
		t.Fatalf("%+v", r)
	}
	if len(results) != 4 {
		t.Fatalf("one failing item stopped the run: %+v", results)
	}
	var buf bytes.Buffer
	if !ingress.Print(&buf, results) || !strings.Contains(buf.String(), "FAIL") {
		t.Errorf("Print:\n%s", buf.String())
	}
}

func TestCheckFailsAnExpiredCertificate(t *testing.T) {
	w := good()
	w.notAfter = now.Add(-time.Hour)
	if r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckCertificate); r.OK || !strings.Contains(r.Detail, "expired") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckFailsAnUnhealthyAnswer(t *testing.T) {
	w := good()
	w.health = http.StatusBadGateway
	if r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckCertificate); r.OK || !strings.Contains(r.Detail, "502") || !strings.Contains(r.Fix, "http://127.0.0.1:8480") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckFailsAnHTTPCallback(t *testing.T) {
	w := good()
	w.location = "https://id.example.org/authorize?redirect_uri=http%3A%2F%2Fstatus.example.org%2Flogin%2Foidc%2Fcallback"
	r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckSignIn)
	if r.OK || !strings.Contains(r.Detail, "http://status.example.org/login/oidc/callback") || !strings.Contains(r.Fix, "paisans apply --site watch") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckFailsWhenSignInIsNotConfigured(t *testing.T) {
	w := good()
	w.location = "/login"
	r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckSignIn)
	if r.OK || !strings.Contains(r.Detail, "back to /login") || !strings.Contains(r.Fix, "paisans oidc client create --app status") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckFailsAnUpstreamReachableAroundTheProxy(t *testing.T) {
	w := good()
	w.upstreamUp = true
	r := result(t, run(t, w, external(t, "192.168.1.20:8480")), ingress.CheckUpstream)
	if r.OK || !strings.Contains(r.Detail, "203.0.113.20:8480") || !strings.Contains(r.Fix, "DOCKER-USER") {
		t.Fatalf("%+v", r)
	}
}

// In ingress mode paisans the toolkit's own Caddy is in front, so only the
// name and the certificate are the operator's to check.
func TestCheckRunsOnlyTheFirstTwoInPaisansMode(t *testing.T) {
	target, err := ingress.For(fixture(t), "status")
	if err != nil {
		t.Fatal(err)
	}
	results := run(t, good(), target)
	if len(results) != 2 || results[0].Name != ingress.CheckDNS || results[1].Name != ingress.CheckCertificate {
		t.Fatalf("%+v", results)
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("%+v", r)
		}
	}
}

// A stale record still pointing at the gateway beside the monitor's own sends
// some visitors to the wrong machine, so every resolved address must be one
// the site declares.
func TestCheckFailsAnAddressTheSiteDoesNotDeclare(t *testing.T) {
	w := good()
	w.resolves = []string{"203.0.113.20", "203.0.113.10"}
	r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckDNS)
	if r.OK || !strings.Contains(r.Detail, "203.0.113.10") || !strings.Contains(r.Fix, "203.0.113.10") {
		t.Fatalf("%+v", r)
	}
}

// Only a refusal or a timeout shows the port closed. Any other dial error
// (no route, network unreachable) says nothing about the port, so it is not
// a pass.
func TestCheckTreatsOnlyRefusedOrTimedOutAsClosed(t *testing.T) {
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ETIMEDOUT)}
	unreachable := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	for _, tc := range []struct {
		err error
		ok  bool
	}{{timeout, true}, {context.DeadlineExceeded, true}, {unreachable, false}} {
		w := good()
		w.dialErr = tc.err
		r := result(t, run(t, w, external(t, "127.0.0.1:8480")), ingress.CheckUpstream)
		if r.OK != tc.ok {
			t.Errorf("%v: %+v", tc.err, r)
		}
		if !tc.ok && (!strings.Contains(r.Detail, "inconclusive") || !strings.Contains(r.Detail, "unreachable")) {
			t.Errorf("%v: %+v", tc.err, r)
		}
	}
}

// With public_address6 declared the port must be closed there too.
func TestCheckDialsTheIPv6AddressToo(t *testing.T) {
	cfg := fixture(t)
	watch := cfg.Sites["watch"]
	watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}
	watch.PublicAddress6 = "2001:db8::20"
	cfg.Sites["watch"] = watch
	target, err := ingress.For(cfg, "status")
	if err != nil {
		t.Fatal(err)
	}
	var dialled []string
	w := good()
	w.resolves = []string{"203.0.113.20", "2001:db8::20"}
	w.dialled = &dialled
	r := result(t, run(t, w, target), ingress.CheckUpstream)
	if !r.OK || strings.Join(dialled, ",") != "203.0.113.20:8480,[2001:db8::20]:8480" {
		t.Fatalf("%+v dialled %v", r, dialled)
	}
}
