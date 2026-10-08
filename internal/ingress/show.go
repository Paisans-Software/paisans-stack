package ingress

import (
	"fmt"
	"io"
	"net"
	"strings"
)

// certificateMarker is the line in each snippet where the operator's own
// certificate goes. The toolkit obtains no certificate in ingress mode
// external: it cannot know how an unfamiliar web server gets one, and it must
// not edit a configuration it does not own.
const certificateMarker = "YOUR CERTIFICATE"

// Show prints the hand-off sheet for a target: what the web server in front
// of the app must do, and the same filled in for Caddy, nginx and Apache. In
// ingress mode paisans the toolkit's own Caddy is in front, and the sheet
// says there is nothing to hand off.
func Show(w io.Writer, t Target) {
	if !t.External() {
		fmt.Fprintf(w, "%s is served by the toolkit's own Caddy on %s (ingress mode paisans): nothing to hand off.\n", t.App, t.Site)
		fmt.Fprintf(w, "Its certificate for %s is obtained over DNS-01 through acme.provider, and %s points at %s.\n", t.Hostname, t.Hostname, addresses(t))
		fmt.Fprintf(w, "`paisans ingress check --app %s` checks the name and the certificate from this machine.\n", t.App)
		return
	}

	fmt.Fprintf(w, "Ingress for %s on %s (ingress mode external)\n\n", t.App, t.Site)
	fmt.Fprintf(w, "  hostname   %s\n", t.Hostname)
	fmt.Fprintf(w, "  DNS        %s must point at %s\n", t.Hostname, addresses(t))
	fmt.Fprintf(w, "  upstream   %s\n", t.Upstream())
	fmt.Fprintf(w, "  health     https://%s%s answers %s\n\n", t.Hostname, t.HealthPath, t.HealthExpect)

	fmt.Fprintf(w, "Your web server must:\n")
	fmt.Fprintf(w, "  * terminate TLS for %s, with a certificate you obtain and renew;\n", t.Hostname)
	fmt.Fprintf(w, "    the monitor checks https://%s%s and warns 14 days before it expires\n", t.Hostname, t.HealthPath)
	fmt.Fprintf(w, "  * proxy every path to %s\n", t.Upstream())
	fmt.Fprintf(w, "  * pass the Host header through unchanged\n")
	fmt.Fprintf(w, "  * set X-Forwarded-For to the client's address and X-Forwarded-Proto to https;\n")
	fmt.Fprintf(w, "    the monitor's sign in rate limit is keyed on the client's address\n\n")

	if ip := net.ParseIP(t.ListenHost); ip != nil && !ip.IsLoopback() {
		fmt.Fprintf(w, "WARNING: listen is %s, not loopback. Docker publishes the port with its own\n", t.Listen)
		fmt.Fprintf(w, "iptables rules, in front of ufw, so ufw does not protect it: anything that can\n")
		fmt.Fprintf(w, "reach %s can reach the app around your web server. Restrict it upstream\n", t.ListenHost)
		fmt.Fprintf(w, "of the host, or in Docker's DOCKER-USER chain.\n\n")
	}

	fmt.Fprintf(w, "Caddy (it sets X-Forwarded-For and X-Forwarded-Proto and passes Host itself):\n\n")
	fmt.Fprint(w, indent(caddySnippet(t)))
	fmt.Fprintf(w, "\nnginx:\n\n")
	fmt.Fprint(w, indent(nginxSnippet(t)))
	fmt.Fprintf(w, "\nApache (mod_ssl, mod_proxy, mod_proxy_http and mod_headers; mod_proxy sets\nX-Forwarded-For itself):\n\n")
	fmt.Fprint(w, indent(apacheSnippet(t)))
	fmt.Fprintf(w, "\nThen run `paisans ingress check --app %s` from this machine.\n", t.App)
}

// The three snippets differ in what they spell out because the servers
// differ in what they do unasked. Caddy's reverse_proxy passes incoming
// headers, Host included, through unchanged and sets X-Forwarded-For and
// X-Forwarded-Proto itself (caddyserver.com/docs/caddyfile/directives/
// reverse_proxy, "Defaults", read 2026-10-08). Apache's mod_proxy_http adds
// X-Forwarded-For in reverse proxy mode but not X-Forwarded-Proto, and
// replaces Host unless ProxyPreserveHost is on (httpd 2.4 mod_proxy,
// "Reverse Proxy Request Headers", read 2026-10-08). nginx sets none of them
// unasked.
func caddySnippet(t Target) string {
	return fmt.Sprintf(`%s {
	# %s: leave this line out if this Caddy obtains the certificate
	# itself; otherwise name your own files.
	tls /etc/ssl/%s/fullchain.pem /etc/ssl/%s/privkey.pem
	reverse_proxy %s
}
`, t.Hostname, certificateMarker, t.Hostname, t.Hostname, t.Listen)
}

func nginxSnippet(t Target) string {
	return fmt.Sprintf(`server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name %s;

    # %s: replace both paths with your own.
    ssl_certificate     /etc/ssl/%s/fullchain.pem;
    ssl_certificate_key /etc/ssl/%s/privkey.pem;

    location / {
        proxy_pass %s;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`, t.Hostname, certificateMarker, t.Hostname, t.Hostname, t.Upstream())
}

func apacheSnippet(t Target) string {
	return fmt.Sprintf(`<VirtualHost *:443>
    ServerName %s

    # %s: replace both paths with your own.
    SSLEngine on
    SSLCertificateFile    /etc/ssl/%s/fullchain.pem
    SSLCertificateKeyFile /etc/ssl/%s/privkey.pem

    ProxyPreserveHost On
    RequestHeader set X-Forwarded-Proto "https"
    ProxyPass        / %s/
    ProxyPassReverse / %s/
</VirtualHost>
`, t.Hostname, certificateMarker, t.Hostname, t.Hostname, t.Upstream(), t.Upstream())
}

// addresses is where the hostname must resolve, as a phrase.
func addresses(t Target) string {
	if t.PublicAddress6 == "" {
		return t.PublicAddress
	}
	return t.PublicAddress + " and " + t.PublicAddress6
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "    " + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
