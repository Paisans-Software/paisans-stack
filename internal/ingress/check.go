package ingress

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

// The checks, by name, in the order they run.
const (
	CheckDNS         = "dns"
	CheckCertificate = "certificate"
	CheckSignIn      = "sign in"
	CheckUpstream    = "upstream"
)

// Resolver looks a name up. net.DefaultResolver is one.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Probes are how Check reaches the world: a resolver, an HTTPS client and a
// TCP dialer, and a clock for days to expiry. Each is replaced in tests, so
// no test touches real DNS or the network.
type Probes struct {
	Resolver Resolver
	// Client makes the HTTPS requests. Check never follows a redirect with
	// it: it uses a copy that stops at the first response.
	Client *http.Client
	Dial   func(ctx context.Context, network, address string) (net.Conn, error)
	Now    func() time.Time
}

// DefaultProbes reaches the real world from the operator's machine.
func DefaultProbes() Probes {
	return Probes{
		Resolver: net.DefaultResolver,
		Client:   &http.Client{Timeout: 10 * time.Second},
		Dial:     (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		Now:      time.Now,
	}
}

// Result is one check: whether it passed, what was seen, and when it failed,
// what fixes it.
type Result struct {
	Name   string
	OK     bool
	Detail string
	Fix    string
}

// Check runs the read only checks for a target from this machine, every one
// of them whatever the others found:
//
//  1. the hostname resolves to the site's public address;
//  2. https://<hostname><health path> answers with a certificate valid for
//     the hostname, and how many days it has left;
//  3. the sign in redirect sends the browser to the identity provider with a
//     callback under https://<hostname>/;
//  4. the published port refuses a connection, or does not answer, on the
//     public address (and public_address6), around the web server.
//
// In ingress mode paisans the toolkit's own Caddy is in front of the app and
// only the first two are the operator's to check.
func Check(ctx context.Context, t Target, p Probes) []Result {
	results := []Result{checkDNS(ctx, t, p), checkCertificate(ctx, t, p)}
	if t.External() {
		results = append(results, checkSignIn(ctx, t, p), checkUpstream(ctx, t, p))
	}
	return results
}

// Report shows each result as a step titled by its check, with what was
// seen as the result. A failure ends the step failed beside a refusal
// carrying what was seen and the fix, since the operator must act on both.
// It reports whether any failed.
func Report(r ui.Reporter, results []Result) bool {
	failed := false
	for _, res := range results {
		s := r.Step("check " + res.Name)
		if res.OK {
			s.Done(res.Detail)
			continue
		}
		failed = true
		s.Fail(errors.New(res.Detail))
		r.Refuse(res.Detail, res.Fix)
	}
	return failed
}

func checkDNS(ctx context.Context, t Target, p Probes) Result {
	r := Result{Name: CheckDNS}
	want := []string{t.PublicAddress}
	if t.PublicAddress6 != "" {
		want = append(want, t.PublicAddress6)
	}
	fix := fmt.Sprintf("point %s at %s (A%s), as sites.%s.public_address%s says; `paisans dns init` creates exactly these records", t.Hostname, addresses(t), aaaa(t), t.Site, v6key(t))
	got, err := p.Resolver.LookupHost(ctx, t.Hostname)
	if err != nil {
		r.Detail = fmt.Sprintf("%s does not resolve: %v", t.Hostname, err)
		r.Fix = fix
		return r
	}
	var missing, stray []string
	for _, address := range want {
		if !slices.ContainsFunc(got, func(g string) bool { return sameIP(g, address) }) {
			missing = append(missing, address)
		}
	}
	// A record the site does not declare, such as a stale one still
	// pointing at the gateway, sends some visitors to the wrong machine.
	for _, g := range got {
		if !slices.ContainsFunc(want, func(w string) bool { return sameIP(g, w) }) {
			stray = append(stray, g)
		}
	}
	if len(missing) > 0 || len(stray) > 0 {
		var why []string
		if len(missing) > 0 {
			why = append(why, "not "+strings.Join(missing, ", "))
		}
		if len(stray) > 0 {
			why = append(why, "and also "+strings.Join(stray, ", ")+", which "+t.Site+" does not declare")
		}
		r.Detail = fmt.Sprintf("%s resolves to %s: %s", t.Hostname, strings.Join(got, ", "), strings.Join(why, ", "))
		r.Fix = fix
		if len(stray) > 0 {
			r.Fix += fmt.Sprintf(".\nDelete the records for %s pointing at %s by hand at the provider: dns init never updates a record, and dns prune keeps one whose name is still wanted", t.Hostname, strings.Join(stray, ", "))
		}
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("%s resolves to %s", t.Hostname, strings.Join(got, ", "))
	return r
}

func checkCertificate(ctx context.Context, t Target, p Probes) Result {
	r := Result{Name: CheckCertificate}
	address := fmt.Sprintf("https://%s%s", t.Hostname, t.HealthPath)
	certFix := fmt.Sprintf("give your web server a certificate for %s, on the lines marked YOUR CERTIFICATE in `paisans ingress show --app %s`, and renew it before it expires", t.Hostname, t.App)
	if !t.External() {
		certFix = fmt.Sprintf("the Caddy on %s obtains it over DNS-01: check that secrets external.acme_dns_token is set and scoped to the zone, then read the caddy container's log there", t.Site)
	}
	resp, err := get(ctx, p, address)
	if err != nil {
		r.Detail, r.Fix = describeTLSError(t, address, err), certFix
		var hostname x509.HostnameError
		var unknown x509.UnknownAuthorityError
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &hostname) && !errors.As(err, &unknown) && !errors.As(err, &invalid) {
			r.Fix = fmt.Sprintf("make %s answer HTTPS on 443 from your web server", t.Hostname)
			if t.External() {
				r.Fix += fmt.Sprintf(", proxying to %s", t.Upstream())
			}
		}
		return r
	}
	defer resp.Body.Close()
	expiry := ""
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		left := resp.TLS.PeerCertificates[0].NotAfter.Sub(p.Now())
		expiry = fmt.Sprintf(", certificate valid for %s for %d more days", t.Hostname, int(left.Hours()/24))
	}
	if !expected(t.HealthExpect, resp.StatusCode) {
		r.Detail = fmt.Sprintf("%s answered %d, want %s%s", address, resp.StatusCode, t.HealthExpect, expiry)
		r.Fix = fmt.Sprintf("the web server must proxy every path to %s, and the app must be running there (`paisans apply --site %s`)", upstreamOf(t), t.Site)
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("%s answered %d%s", address, resp.StatusCode, expiry)
	return r
}

// signInPath is where the uptime fork starts an identity provider sign in
// (src/routes/auth.js at 1.1.0-oidc.3). It redirects to the provider with a
// redirect_uri built from PUBLIC_BASE_URL (src/lib/oidc.js redirectUri), or
// back to /login when sign in through the provider is not configured or the
// provider does not answer.
const signInPath = "/login/oidc"

func checkSignIn(ctx context.Context, t Target, p Probes) Result {
	r := Result{Name: CheckSignIn}
	address := "https://" + t.Hostname + signInPath
	resp, err := get(ctx, p, address)
	if err != nil {
		r.Detail = fmt.Sprintf("%s: %v", address, err)
		r.Fix = fmt.Sprintf("the web server must proxy every path to %s, not only %s", t.Upstream(), t.HealthPath)
		return r
	}
	defer resp.Body.Close()
	location := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || location == "" {
		r.Detail = fmt.Sprintf("%s answered %d with no redirect", address, resp.StatusCode)
		r.Fix = fmt.Sprintf("the web server must proxy every path to %s, not only %s", t.Upstream(), t.HealthPath)
		return r
	}
	next, err := url.Parse(location)
	if err != nil {
		r.Detail = fmt.Sprintf("%s redirected to %q, which is not a URL", address, location)
		r.Fix = "the app answered something unexpected; read its container's log"
		return r
	}
	if next.Host == "" || strings.EqualFold(next.Hostname(), t.Hostname) {
		r.Detail = fmt.Sprintf("%s sent the browser back to %s, not to the identity provider", address, next.Path)
		r.Fix = fmt.Sprintf("create the app's client at the identity provider (`paisans oidc client create --app %s`), apply it (`paisans apply --site %s`), and check the identity provider is up", t.App, t.Site)
		return r
	}
	callback := next.Query().Get("redirect_uri")
	if want := "https://" + t.Hostname + "/"; !strings.HasPrefix(callback, want) {
		r.Detail = fmt.Sprintf("the sign in redirect names %q as its callback, not one under %s", callback, want)
		r.Fix = fmt.Sprintf("the app builds its callback from PUBLIC_BASE_URL, which apply renders as https://%s: run `paisans apply --site %s`, and make sure your web server passes Host through unchanged and does not rewrite Location", t.Hostname, t.Site)
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("%s redirects to %s with callback %s", address, next.Host, callback)
	return r
}

func checkUpstream(ctx context.Context, t Target, p Probes) Result {
	r := Result{Name: CheckUpstream}
	addresses := []string{t.PublicAddress}
	if t.PublicAddress6 != "" {
		addresses = append(addresses, t.PublicAddress6)
	}
	var closed, open, inconclusive []string
	var unreachable []string // families, for the note
	for _, a := range addresses {
		address := net.JoinHostPort(a, strconv.Itoa(t.ListenPort))
		conn, err := p.Dial(ctx, "tcp", address)
		switch {
		case err == nil:
			conn.Close()
			open = append(open, address)
		case refusedOrTimedOut(err):
			closed = append(closed, address)
		default:
			inconclusive = append(inconclusive, fmt.Sprintf("%s (%v)", address, err))
			unreachable = append(unreachable, family(a))
		}
	}
	switch {
	case len(open) > 0:
		r.Detail = fmt.Sprintf("%s answers: the app is reachable around your web server", strings.Join(open, " and "))
		r.Fix = fmt.Sprintf("Docker publishes %s in front of ufw. Set sites.%s.ingress.listen to 127.0.0.1:%d when the web server runs on this machine, or drop port %d from outside in Docker's DOCKER-USER chain", t.Listen, t.Site, t.ListenPort, t.ListenPort)
	case len(closed) == 0:
		// Not one address answered either way, so nothing is known about
		// the port.
		r.Detail = fmt.Sprintf("inconclusive: %s; the address was unreachable from here, which says nothing about the port", strings.Join(inconclusive, ", "))
		r.Fix = "run the check from a machine that can reach the site's public address, where only a refused or timed out connection shows the port closed"
	default:
		// One conclusive answer shows the publish is not on the public
		// interface. A family this machine has no route for, commonly IPv6
		// on a home connection, is named rather than failed.
		r.OK = true
		r.Detail = fmt.Sprintf("%s refused, or the host did not answer, so the app is reachable only through the web server", strings.Join(closed, " and "))
		if len(inconclusive) > 0 {
			r.Detail += fmt.Sprintf("; %s not tested, unreachable from here: %s", strings.Join(unreachable, " and "), strings.Join(inconclusive, ", "))
		}
	}
	return r
}

// family names an address's IP version, for a message.
func family(address string) string {
	if ip := net.ParseIP(address); ip != nil && ip.To4() == nil {
		return "IPv6"
	}
	return "IPv4"
}

// refusedOrTimedOut reports whether a dial failed because the port is
// closed or filtered: refused, or no answer at all. Anything else, such as
// no route to the address, did not reach the port and shows nothing.
func refusedOrTimedOut(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// get requests a URL without following a redirect.
func get(ctx context.Context, p Probes, address string) (*http.Response, error) {
	client := *p.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// describeTLSError says what was wrong with a failed HTTPS request, naming
// the certificate problem when there was one.
func describeTLSError(t Target, address string, err error) string {
	var hostname x509.HostnameError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &hostname):
		return fmt.Sprintf("the certificate is not valid for %s: it names %s", t.Hostname, strings.Join(certNames(hostname.Certificate), ", "))
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return fmt.Sprintf("the certificate for %s has expired: %v", t.Hostname, invalid)
	case errors.As(err, &invalid):
		return fmt.Sprintf("the certificate for %s is not valid: %v", t.Hostname, invalid)
	case errors.As(err, &unknown):
		return fmt.Sprintf("the certificate for %s is not signed by an authority this machine trusts", t.Hostname)
	}
	return fmt.Sprintf("%s: %v", address, err)
}

func certNames(c *x509.Certificate) []string {
	if c == nil {
		return nil
	}
	if len(c.DNSNames) > 0 {
		return c.DNSNames
	}
	return []string{c.Subject.CommonName}
}

// expected reports whether a status is one of the comma separated codes the
// kind's health route answers with.
func expected(codes string, status int) bool {
	for _, code := range strings.Split(codes, ",") {
		if strings.TrimSpace(code) == strconv.Itoa(status) {
			return true
		}
	}
	return false
}

func upstreamOf(t Target) string {
	if t.External() {
		return t.Upstream()
	}
	return "the app on " + t.Site
}

func sameIP(a, b string) bool {
	x, y := net.ParseIP(a), net.ParseIP(b)
	return x != nil && y != nil && x.Equal(y)
}

func aaaa(t Target) string {
	if t.PublicAddress6 == "" {
		return ""
	}
	return " and AAAA"
}

func v6key(t Target) string {
	if t.PublicAddress6 == "" {
		return ""
	}
	return " and public_address6"
}
