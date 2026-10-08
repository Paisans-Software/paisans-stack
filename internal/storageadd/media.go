package storageadd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// mediaSnippetDir is where render places each app's media routing on the
// gateway, relative to a site's root, one <app>-media.caddy per app that
// stores objects.
func mediaSnippetDir(d deployment.Deployment) string {
	return d.RelPath("infra", "caddy", "snippets") + "/"
}

// webSuffix is the internal suffix Garage's web endpoint resolves a bucket
// from. It must match root_domain in garage.toml.tmpl and render's
// webEndpointSuffix.
const webSuffix = ".web.garage.internal"

func validateCaddy(d deployment.Deployment) string {
	return d.ComposeCmd("infra") + " exec -T caddy caddy validate --config /etc/caddy/Caddyfile"
}

func reloadCaddy(d deployment.Deployment) string {
	return d.ComposeCmd("infra") + " exec -T caddy caddy reload --config /etc/caddy/Caddyfile"
}

// buildMedia is stage 8: the gateway's media routes, one snippet per app that
// stores objects, each naming the Garage nodes in storage.garage.sites order.
// It comes after the join on purpose: a node with no role answers every
// bucket as missing, so a route must not prefer one before it has joined.
// Nil when the deployment has no gateway or no app stores objects.
func (p *Plan) buildMedia() (*Stage, error) {
	if p.gateway == "" {
		return nil, nil
	}
	want := map[string]string{}
	var paths []string
	for _, f := range p.rendered.Files {
		rel, ok := strings.CutPrefix(f.Path, p.gateway+"/")
		if !ok || !strings.HasPrefix(rel, mediaSnippetDir(p.dep())) || !strings.HasSuffix(rel, "-"+kinds.MediaRole+".caddy") {
			continue
		}
		want[rel] = f.Content
		paths = append(paths, rel)
	}
	if len(paths) == 0 {
		return nil, nil
	}
	sort.Strings(paths)
	st := &Stage{
		Name: "media routes",
		Gate: "the gateway's media routes match the render, the Garage nodes in storage.garage.sites order",
	}
	t := p.transports[p.gateway]
	sp, err := apply.Build(p.gateway, p.rendered, acme.Module(p.cfg.ACME.Provider), t, apply.Scope(paths...))
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
			Step{Site: p.gateway, Verb: "validate", Text: "the gateway's configuration: " + validateCaddy(p.dep())},
			Step{Site: p.gateway, Verb: "reload", Text: "Caddy: " + reloadCaddy(p.dep())})
	}
	st.run = func() error {
		if err := apply.Execute(sp, t); err != nil {
			return err
		}
		if out, err := t.Run(validateCaddy(p.dep())); err != nil {
			return fmt.Errorf("%s: the gateway's configuration does not validate with the new media routes, so Caddy was not reloaded and still serves the old ones:\n%s", p.gateway, out)
		}
		if out, err := t.Run(reloadCaddy(p.dep())); err != nil {
			return fmt.Errorf("%s: reloading Caddy: %s", p.gateway, lastLines(out, 5))
		}
		return nil
	}
	st.gate = func() error {
		for _, rel := range paths {
			have, _, err := t.ReadFile("/" + rel)
			if err != nil {
				return err
			}
			if have != want[rel] {
				return fmt.Errorf("%s's /%s does not match the render", p.gateway, rel)
			}
		}
		return nil
	}
	return st, nil
}

// probe is the smoke test's object and the credential it is written with.
type probe struct {
	app, bucket, keyID, secret, region string
	key, body                          string
	// media is the app's own media hostname, empty when it has none.
	media string
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
			media: kinds.MediaHostname(app, p.cfg.Community.Domain),
			// One fixed key, overwritten by every run and deleted at the end,
			// so an interrupted run leaves at most this one object behind.
			key:  "paisans-probe/storage-add",
			body: "paisans storage add probe " + hex.EncodeToString(buf),
		}
	}
	return nil
}

// s3Config is a curl configuration for one signed request to Garage's S3 API,
// shaped by garage.S3Object. It travels on stdin, so the secret is never in a
// command line where `ps` would show it.
func (pr *probe) s3Config(method, address string) string {
	return garage.S3Object{Address: address, Bucket: pr.bucket, Key: pr.key, KeyID: pr.keyID, Secret: pr.secret, Region: pr.region}.CurlConfig(method, pr.body)
}

// curlStdin runs curl with its configuration on stdin.
const curlStdin = garage.CurlStdin

func (pr *probe) redact(s string) string {
	return strings.ReplaceAll(s, pr.secret, "[redacted]")
}

// buildSmoke is the last stage: a probe object written through the first
// listed node, read back through every node's S3 API and web endpoint and
// through the app's own media hostname, then deleted. Nil, with a note, when no app
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
		Gate:     "the probe reads back, byte for byte, through every node's S3 API and web endpoint and through the app's media hostname, and is deleted",
	}
	st.Steps = append(st.Steps, Step{Site: first.site, Verb: "write", Text: fmt.Sprintf("%s/%s through %s's S3 API, with %s's key", pr.bucket, pr.key, first.site, pr.app)})
	for _, n := range p.nodes {
		st.Steps = append(st.Steps, Step{Site: n.site, Verb: "read", Text: "the probe through its S3 API (3900) and its web endpoint (3902)"})
	}
	if p.gateway != "" && pr.media != "" {
		st.Steps = append(st.Steps, Step{Site: p.gateway, Verb: "read", Text: fmt.Sprintf("https://%s/%s, %s's media hostname, through the gateway's own Caddy", pr.media, pr.key, pr.app)})
	}
	if p.opts.StopTest {
		victim := p.nodes[len(p.nodes)-1]
		st.Steps = append(st.Steps,
			Step{Site: victim.site, Verb: "stop", Text: "Garage, the last listed site, to prove the rest serve without it: " + stopGarage(p.dep())},
			Step{Site: first.site, Verb: "read", Text: "the probe through every other node and the media hostname, with " + victim.site + " stopped"},
			Step{Site: first.site, Verb: "write", Text: "a second probe through " + first.site + ", and read it back, with " + victim.site + " stopped: uploads must survive"},
			Step{Site: victim.site, Verb: "start", Text: "Garage again, whatever the reads said: " + startGarage(p.dep())},
		)
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
		if err := p.readProbe(pr, p.nodes); err != nil {
			return err
		}
		if p.opts.StopTest {
			if err := p.stopTest(pr); err != nil {
				return err
			}
		}
		if out, err := t.RunInput(curlStdin, pr.s3Config("DELETE", first.address)); err != nil {
			return fmt.Errorf("%s: deleting the probe: %s", first.site, pr.redact(lastLines(out, 3)))
		}
		return nil
	}
	return st
}

// readProbe reads the probe back through each given node's S3 API and web
// endpoint, from the first listed site's host, then through its app's media
// hostname on the gateway.
func (p *Plan) readProbe(pr *probe, nodes []*node) error {
	for _, n := range nodes {
		// At consistency dangerous a node may not have the probe yet,
		// so a read is retried for a while before it counts as failed.
		err := poll(attempts(probeWait, probePoll), probePoll, func() error {
			got, err := p.transports[p.nodes[0].site].RunInput(curlStdin, pr.s3Config("GET", n.address))
			if err != nil {
				return fmt.Errorf("reading the probe through %s's S3 API: %s", n.site, pr.redact(lastLines(got, 3)))
			}
			if got != pr.body {
				return fmt.Errorf("%s's S3 API returned %q, not the probe", n.site, pr.redact(lastLines(got, 1)))
			}
			web := fmt.Sprintf("curl -fsS --max-time 20 -H 'Host: %s%s' http://%s:3902/%s", pr.bucket, webSuffix, n.address, pr.key)
			got, err = p.transports[p.nodes[0].site].Run(web)
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
	if p.gateway != "" && pr.media != "" {
		host := pr.media
		cmd := fmt.Sprintf("curl -fsS --max-time 20 --resolve %s:443:127.0.0.1 https://%s/%s", host, host, pr.key)
		got, err := p.transports[p.gateway].Run(cmd)
		if err != nil {
			return fmt.Errorf("reading the probe through https://%s on %s: %s", host, p.gateway, lastLines(got, 3))
		}
		if got != pr.body {
			return fmt.Errorf("https://%s returned %q, not the probe", host, lastLines(got, 1))
		}
	}
	return nil
}

// stopTestAllowed reports whether uploads survive one Garage node down, the
// only case in which --stop-test may stop one (founder decision): replication
// 3 or more with at least three nodes, or replication 2 or more at
// consistency dangerous. Garage v1.0.1's write quorum is 2 of 3 at
// replication 3 and 1 at dangerous (src/rpc/replication_mode.rs:45-59).
func (p *Plan) stopTestAllowed() error {
	rf, n := p.replication(), len(p.nodes)
	switch {
	case n < 2:
		return fmt.Errorf("--stop-test needs at least two Garage sites; there is %d", n)
	case rf >= 3 && n >= 3:
		return nil
	case rf >= 2 && p.consistency() == config.GarageDangerous:
		return nil
	}
	return fmt.Errorf("--stop-test stops one Garage node, and at replication %d on %d site(s) at consistency %s every upload would fail while it is down. It runs at replication 3 or more on three or more sites, or at replication 2 or more at consistency dangerous", rf, n, p.consistency())
}

// stopTest stops Garage on the last listed site, proves reads and an upload
// still work through the rest, and starts it again whatever happened, so a
// failure is the only thing left to fix. The last listed site is never the
// first, which serves media and takes the apps' writes.
func (p *Plan) stopTest(pr *probe) (err error) {
	first, victim := p.nodes[0], p.nodes[len(p.nodes)-1]
	vt := p.transports[victim.site]
	p.say("  %-9s Garage on %s\n", "stop", victim.site)
	if out, err := vt.Run(stopGarage(p.dep())); err != nil {
		return fmt.Errorf("%s: stopping Garage for the stop test: %s", victim.site, lastLines(out, 5))
	}
	defer func() {
		p.say("  %-9s Garage on %s\n", "start", victim.site)
		if out, startErr := vt.Run(startGarage(p.dep())); startErr != nil {
			err = fmt.Errorf("%v\n%s: starting Garage again after the stop test failed too: %s", err, victim.site, lastLines(out, 5))
			return
		}
		if healthErr := p.everyNodeHealthy(); healthErr != nil && err == nil {
			err = fmt.Errorf("%s did not rejoin after the stop test: %w", victim.site, healthErr)
		}
	}()
	if err := p.readProbe(pr, p.nodes[:len(p.nodes)-1]); err != nil {
		return fmt.Errorf("with %s stopped: %w", victim.site, err)
	}
	second := *pr
	second.key = pr.key + "-stopped"
	second.body = pr.body + " (written with " + victim.site + " stopped)"
	t := p.transports[first.site]
	if out, err := t.RunInput(curlStdin, second.s3Config("PUT", first.address)); err != nil {
		return fmt.Errorf("with %s stopped, an upload through %s failed: %s", victim.site, first.site, second.redact(lastLines(out, 3)))
	}
	defer t.RunInput(curlStdin, second.s3Config("DELETE", first.address))
	if err := p.readProbe(&second, p.nodes[:len(p.nodes)-1]); err != nil {
		return fmt.Errorf("with %s stopped, the upload made then: %w", victim.site, err)
	}
	return nil
}
