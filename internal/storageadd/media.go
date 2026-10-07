package storageadd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// mediaSnippet is the gateway's media routing, relative to a site's root, as
// render places it.
const mediaSnippet = "srv/infra/caddy/snippets/media.caddy"

// webSuffix is the internal suffix Garage's web endpoint resolves a bucket
// from. It must match root_domain in garage.toml.tmpl and render's
// webEndpointSuffix.
const webSuffix = ".web.garage.internal"

const (
	validateCaddy = "docker compose -f /srv/infra/compose.yaml exec -T caddy caddy validate --config /etc/caddy/Caddyfile"
	reloadCaddy   = "docker compose -f /srv/infra/compose.yaml exec -T caddy caddy reload --config /etc/caddy/Caddyfile"
)

// buildMedia is stage 8: the gateway's media routes, which name every Garage
// node in storage.garage.sites order. It comes after the join on purpose: a
// node with no role answers every bucket as missing, so the route must not
// prefer one before it has joined. Nil when the deployment has no gateway or
// no media hostname.
func (p *Plan) buildMedia() (*Stage, error) {
	if p.gateway == "" || p.cfg.Storage.MediaHostname == "" {
		return nil, nil
	}
	want := ""
	for _, f := range p.rendered.Files {
		if f.Path == p.gateway+"/"+mediaSnippet {
			want = f.Content
		}
	}
	if want == "" {
		return nil, nil
	}
	st := &Stage{
		Name: "media routes",
		Gate: "the gateway's media routes match the render, every Garage node in storage.garage.sites order",
	}
	t := p.transports[p.gateway]
	sp, err := apply.Build(p.gateway, p.rendered, acme.Module(p.cfg.ACME.Provider), t, apply.Scope(mediaSnippet))
	if err != nil {
		return nil, err
	}
	if c := sp.Conflicts(); len(c) > 0 {
		return nil, fmt.Errorf("storage add: %s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed. Restore it, or copy what is wanted into the configuration, and run storage add again", p.gateway, c[0].Path)
	}
	for _, c := range sp.Writes() {
		st.Steps = append(st.Steps, Step{Site: p.gateway, Verb: c.Kind.String(), Text: c.Path})
	}
	if len(sp.Writes()) > 0 {
		st.Steps = append(st.Steps,
			Step{Site: p.gateway, Verb: "validate", Text: "the gateway's configuration: " + validateCaddy},
			Step{Site: p.gateway, Verb: "reload", Text: "Caddy: " + reloadCaddy})
	}
	st.run = func() error {
		if err := apply.Execute(sp, t); err != nil {
			return err
		}
		if out, err := t.Run(validateCaddy); err != nil {
			return fmt.Errorf("%s: the gateway's configuration does not validate with the new media routes, so Caddy was not reloaded and still serves the old ones:\n%s", p.gateway, out)
		}
		if out, err := t.Run(reloadCaddy); err != nil {
			return fmt.Errorf("%s: reloading Caddy: %s", p.gateway, lastLines(out, 5))
		}
		return nil
	}
	st.gate = func() error {
		have, _, err := t.ReadFile("/" + mediaSnippet)
		if err != nil {
			return err
		}
		if have != want {
			return fmt.Errorf("%s's /%s does not match the render", p.gateway, mediaSnippet)
		}
		return nil
	}
	return st, nil
}

// probe is the smoke test's object and the credential it is written with.
type probe struct {
	app, bucket, keyID, secret, region string
	key, body                          string
}

// publicProbe picks the first app, by name, that stores objects and serves
// them publicly, so the probe can be read through the gateway with no
// credential, the way a browser reads media.
func (p *Plan) publicProbe() *probe {
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if !kinds.UsesObjectStorage(app.Kind) || !kinds.ServesObjectsPublicly(app.Kind) {
			continue
		}
		keyID, _ := garage.SecretString(p.secrets, name, "s3_access_key_id")
		secret, _ := garage.SecretString(p.secrets, name, "s3_secret_access_key")
		if keyID == "" || secret == "" {
			continue
		}
		region := "garage"
		if v, ok := app.Settings["s3_region"].(string); ok && v != "" {
			region = v
		}
		buf := make([]byte, 8)
		rand.Read(buf)
		return &probe{
			app: name, bucket: garage.BucketName(app, name), keyID: keyID, secret: secret, region: region,
			// One fixed key, overwritten by every run and deleted at the end,
			// so an interrupted run leaves at most this one object behind.
			key:  "paisans-probe/storage-add",
			body: "paisans storage add probe " + hex.EncodeToString(buf),
		}
	}
	return nil
}

// s3Config is a curl configuration for one signed request to Garage's S3 API.
// It travels on stdin, so the secret is never in a command line where `ps`
// would show it.
func (pr *probe) s3Config(method, address string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "url = \"http://%s:3900/%s/%s\"\n", address, pr.bucket, pr.key)
	fmt.Fprintf(&b, "request = \"%s\"\n", method)
	fmt.Fprintf(&b, "user = \"%s:%s\"\n", pr.keyID, pr.secret)
	fmt.Fprintf(&b, "aws-sigv4 = \"aws:amz:%s:s3\"\n", pr.region)
	b.WriteString("header = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\n")
	if method == "PUT" {
		fmt.Fprintf(&b, "data-binary = \"%s\"\n", pr.body)
	}
	return b.String()
}

// curlStdin runs curl with its configuration on stdin.
const curlStdin = "curl -fsS --max-time 20 -K -"

func (pr *probe) redact(s string) string {
	return strings.ReplaceAll(s, pr.secret, "[redacted]")
}

// buildSmoke is the last stage: a probe object written through the first
// listed node, read back through every node's S3 API and web endpoint and
// through the media hostname, then deleted. Nil, with a note, when no app
// serves objects publicly.
func (p *Plan) buildSmoke() *Stage {
	pr := p.publicProbe()
	if pr == nil {
		p.Notes = append(p.Notes, "no app serves objects publicly, so there is no bucket to put a probe through and the smoke test is skipped")
		return nil
	}
	first := p.nodes[0]
	st := &Stage{
		Name:     "smoke",
		Verifies: true,
		Gate:     "the probe reads back, byte for byte, through every node's S3 API and web endpoint and through the media hostname, and is deleted",
	}
	st.Steps = append(st.Steps, Step{Site: first.site, Verb: "write", Text: fmt.Sprintf("%s/%s through %s's S3 API, with %s's key", pr.bucket, pr.key, first.site, pr.app)})
	for _, n := range p.nodes {
		st.Steps = append(st.Steps, Step{Site: n.site, Verb: "read", Text: "the probe through its S3 API (3900) and its web endpoint (3902)"})
	}
	if p.gateway != "" && p.cfg.Storage.MediaHostname != "" {
		st.Steps = append(st.Steps, Step{Site: p.gateway, Verb: "read", Text: fmt.Sprintf("https://%s/%s/%s, through the gateway's own Caddy", p.cfg.Storage.MediaHostname, pr.bucket, pr.key)})
	}
	st.Steps = append(st.Steps, Step{Site: first.site, Verb: "delete", Text: "the probe"})

	// Every S3 and web request runs on the first listed node's host, which
	// reaches every node over the mesh.
	t := p.transports[first.site]
	st.run = func() error {
		out, err := t.RunInput(curlStdin, pr.s3Config("PUT", first.address))
		if err != nil {
			return fmt.Errorf("%s: writing the probe: %s", first.site, pr.redact(lastLines(out, 3)))
		}
		return nil
	}
	st.gate = func() error {
		for _, n := range p.nodes {
			// At consistency dangerous a node may not have the probe yet,
			// so a read is retried for a while before it counts as failed.
			err := poll(attempts(probeWait, probePoll), probePoll, func() error {
				got, err := t.RunInput(curlStdin, pr.s3Config("GET", n.address))
				if err != nil {
					return fmt.Errorf("reading the probe through %s's S3 API: %s", n.site, pr.redact(lastLines(got, 3)))
				}
				if got != pr.body {
					return fmt.Errorf("%s's S3 API returned %q, not the probe", n.site, pr.redact(lastLines(got, 1)))
				}
				web := fmt.Sprintf("curl -fsS --max-time 20 -H 'Host: %s%s' http://%s:3902/%s", pr.bucket, webSuffix, n.address, pr.key)
				got, err = t.Run(web)
				if err != nil {
					return fmt.Errorf("reading the probe through %s's web endpoint: %s", n.site, lastLines(got, 3))
				}
				if got != pr.body {
					return fmt.Errorf("%s's web endpoint returned %q, not the probe", n.site, lastLines(got, 1))
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		if p.gateway != "" && p.cfg.Storage.MediaHostname != "" {
			host := p.cfg.Storage.MediaHostname
			cmd := fmt.Sprintf("curl -fsS --max-time 20 --resolve %s:443:127.0.0.1 https://%s/%s/%s", host, host, pr.bucket, pr.key)
			got, err := p.transports[p.gateway].Run(cmd)
			if err != nil {
				return fmt.Errorf("reading the probe through https://%s on %s: %s", host, p.gateway, lastLines(got, 3))
			}
			if got != pr.body {
				return fmt.Errorf("https://%s returned %q, not the probe", host, lastLines(got, 1))
			}
		}
		if out, err := t.RunInput(curlStdin, pr.s3Config("DELETE", first.address)); err != nil {
			return fmt.Errorf("%s: deleting the probe: %s", first.site, pr.redact(lastLines(out, 3)))
		}
		return nil
	}
	return st
}
