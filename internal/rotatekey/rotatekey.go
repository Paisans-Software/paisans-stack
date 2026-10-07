// Package rotatekey replaces one app's Garage S3 key: `paisans storage
// rotate-key`. It is staged like `storage add`, and every stage ends at a gate
// that stops the command with its evidence when it does not pass; nothing
// after a failed gate runs.
//
// The order is the design. The new pair is written to the encrypted secrets
// before Garage hears of it, so a key never exists in Garage that the secrets
// file does not hold. The new key is imported and granted beside the old one,
// so the app keeps working on the old key until it is switched. The app is
// switched, then the new key is proven with a real write, read and delete, and
// only then is the old key deleted. A run that stops anywhere before the
// deletion leaves the old key in Garage and granted, so the app keeps working
// on whichever key it was last started with.
//
// The state a resumed run needs lives in the secrets file: a previous pair
// beside the current one means a rotation is in progress, and Build plans
// from there, never generating a second new key.
package rotatekey

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

const (
	currentID     = "s3_access_key_id"
	currentSecret = "s3_secret_access_key"
	previousID    = secretsgen.PreviousKeyID
	previousKey   = secretsgen.PreviousSecretKey

	// probeObject is the one object the proof writes, overwritten by every
	// run and deleted at the end, so an interrupted run leaves at most this
	// one object behind.
	probeObject = "paisans-probe/rotate-key"
)

// Switch is how a rotation moves an app onto the new key: the same as
// `paisans apply --site <site> --only <app>`, which renders the app's .env from
// the secrets, writes it, recreates the app and holds it to apply's health
// gate. It is an interface so that this package does not render or reach a
// host's files itself, and so the order of a rotation can be tested on its own.
type Switch interface {
	// Pending lists what that apply would still change on site with these
	// secrets. Empty means the site already runs the app as rendered.
	Pending(site string, secrets *config.Secrets) ([]string, error)
	// Apply runs it, through apply's health gate.
	Apply(site string, secrets *config.Secrets) error
}

// Options are everything a rotation reads and the ways it writes.
type Options struct {
	App     string
	Config  *config.Config
	Secrets *config.Secrets
	// Transports reach the first Garage site and every site running the app.
	Transports map[string]apply.Transport
	Switch     Switch
	// Save writes the secrets back, encrypted to the file's recipients.
	Save func(*config.Secrets) error
	// Generate makes the new pair. Nil uses secretsgen.GarageKeyPair.
	Generate func() (keyID, secret string, err error)
	// Probe is the body the proof writes. Empty uses a fixed marker.
	Probe string
}

// Stage is one stage of the rotation: what it will do, then its gate.
type Stage struct {
	Number int
	Name   string
	Steps  []string
	Gate   string
	// Verifies marks a stage that only proves the result, run on every
	// execute whether or not it has steps.
	Verifies bool

	run  func() error
	gate func() error
}

// Plan is one app's rotation, decided from the secrets and the live Garage.
type Plan struct {
	App    string
	Bucket string
	// Anchor is the Garage site every key and bucket command runs on.
	Anchor string
	// Sites run the app, and are switched and probed.
	Sites []string
	// OldKeyID is the key being retired. NewKeyID is empty until stage 1
	// has generated it.
	OldKeyID string
	NewKeyID string
	// InProgress says the secrets already hold a previous pair.
	InProgress bool
	Stages     []*Stage
	// Progress receives each stage as it starts and each gate as it passes.
	Progress io.Writer

	opts      Options
	oldSecret string
	newSecret string
	region    string
	endpoint  string
	garage    apply.Transport
}

// Build decides the rotation, reading Garage and changing nothing.
func Build(opts Options) (*Plan, error) {
	cfg, secrets := opts.Config, opts.Secrets
	app, ok := cfg.Apps[opts.App]
	if !ok {
		return nil, fmt.Errorf("storage rotate-key: the configuration declares no app %q. Declared apps are %s", opts.App, strings.Join(cfg.AppNames(), ", "))
	}
	if !kinds.UsesObjectStorage(app.Kind) {
		return nil, fmt.Errorf("storage rotate-key: %s is %s, which stores no objects, so it has no Garage key to rotate", opts.App, app.Kind)
	}
	if len(cfg.Storage.Garage.Sites) == 0 {
		return nil, fmt.Errorf("storage rotate-key: storage.garage.sites lists no site, so there is no Garage to hold a key")
	}
	if opts.Generate == nil {
		opts.Generate = secretsgen.GarageKeyPair
	}
	if opts.Probe == "" {
		opts.Probe = "paisans storage rotate-key probe"
	}

	p := &Plan{App: opts.App, Bucket: garage.BucketName(app, opts.App), Anchor: cfg.Storage.Garage.Sites[0], opts: opts}
	p.region = "garage"
	if v, ok := app.Settings["s3_region"].(string); ok && v != "" {
		p.region = v
	}
	// The address the app itself writes to: render's S3_ENDPOINT is the first
	// listed Garage site's mesh address.
	p.endpoint = cfg.Sites[p.Anchor].Address
	switch app.Placement.Mode {
	case config.PlacementPinned:
		p.Sites = []string{app.Placement.Site}
	case config.PlacementCluster:
		p.Sites = cfg.AppsSites()
	}
	if len(p.Sites) == 0 {
		return nil, fmt.Errorf("storage rotate-key: %s runs on no site", opts.App)
	}
	for _, site := range append([]string{p.Anchor}, p.Sites...) {
		if _, ok := opts.Transports[site]; !ok {
			return nil, fmt.Errorf("storage rotate-key: no way to reach %s", site)
		}
	}
	p.garage = opts.Transports[p.Anchor]

	curID, _ := garage.SecretString(secrets, opts.App, currentID)
	curSecret, _ := garage.SecretString(secrets, opts.App, currentSecret)
	prevID, _ := garage.SecretString(secrets, opts.App, previousID)
	prevSecret, _ := garage.SecretString(secrets, opts.App, previousKey)
	if curID == "" || curSecret == "" {
		return nil, fmt.Errorf("storage rotate-key: apps.%s has no S3 key in the secrets. `paisans init` generates one and `paisans storage init` imports it; there is nothing to rotate yet", opts.App)
	}
	switch {
	case prevID == "" && prevSecret == "":
		p.OldKeyID, p.oldSecret = curID, curSecret
	case prevID != "" && prevSecret != "":
		if prevID == curID {
			return nil, fmt.Errorf("storage rotate-key: apps.%s.%s and %s are the same key, %s. Nothing was changed. Restore the secrets file from its history: a rotation must never delete the key the app is using", opts.App, previousID, currentID, curID)
		}
		p.InProgress = true
		p.OldKeyID, p.oldSecret = prevID, prevSecret
		p.NewKeyID, p.newSecret = curID, curSecret
	default:
		return nil, fmt.Errorf("storage rotate-key: apps.%s holds half of a previous S3 pair (%s and %s must both be set or both be absent). Nothing was changed. Restore the secrets file from its history", opts.App, previousID, previousKey)
	}

	oldPresent, err := garage.KeyPresent(p.garage, p.OldKeyID)
	if err != nil {
		return nil, err
	}
	newPresent := false
	if p.InProgress {
		if newPresent, err = garage.KeyPresent(p.garage, p.NewKeyID); err != nil {
			return nil, err
		}
	}
	if !oldPresent && !p.InProgress {
		return nil, fmt.Errorf("storage rotate-key: %s's key %s is not in Garage, so there is no key to retire. Nothing was changed. `paisans storage init` imports the key the secrets hold", opts.App, p.OldKeyID)
	}
	if !oldPresent && !newPresent {
		return nil, fmt.Errorf("storage rotate-key: neither %s's retiring key %s nor its new key %s is in Garage, so the app has no key at all. Nothing was changed. `paisans storage init` imports and grants the new key the secrets hold; run it, then run rotate-key again to clear the previous pair", opts.App, p.OldKeyID, p.NewKeyID)
	}
	bucket, err := garage.ReadBucket(p.garage, p.Bucket)
	if err != nil {
		return nil, err
	}
	if bucket.Absent {
		return nil, fmt.Errorf("storage rotate-key: %s's bucket %s does not exist. Nothing was changed. `paisans storage init` creates it", opts.App, p.Bucket)
	}

	p.Stages = append(p.Stages, p.buildSecrets())
	p.Stages = append(p.Stages, p.buildImport(newPresent, bucket))
	sw, err := p.buildSwitch()
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, sw, p.buildProve(), p.buildRetire(oldPresent))
	for i, st := range p.Stages {
		st.Number = i + 1
	}
	return p, nil
}

// newName is how a step names the new key before it exists.
func (p *Plan) newName() string {
	if p.NewKeyID == "" {
		return "the new key"
	}
	return p.NewKeyID
}

// buildSecrets is stage 1: the new pair, with the old one kept beside it.
func (p *Plan) buildSecrets() *Stage {
	st := &Stage{
		Name: "new key in the secrets",
		Gate: fmt.Sprintf("the secrets hold the new pair as apps.%s.%s and %s, and the retiring %s as %s and %s", p.App, currentID, currentSecret, p.OldKeyID, previousID, previousKey),
	}
	if !p.InProgress {
		st.Steps = []string{
			fmt.Sprintf("generate a new key ID and secret for %s, the way `paisans init` does", p.App),
			fmt.Sprintf("keep %s as apps.%s.%s and %s", p.OldKeyID, p.App, previousID, previousKey),
			"write the secrets file, encrypted to its recipients",
		}
	}
	st.run = func() error {
		id, secret, err := p.opts.Generate()
		if err != nil {
			return err
		}
		if id == p.OldKeyID {
			return fmt.Errorf("the generated key ID is the retiring one, %s", id)
		}
		next := cloneSecrets(p.opts.Secrets)
		for key, value := range map[string]string{previousID: p.OldKeyID, previousKey: p.oldSecret, currentID: id, currentSecret: secret} {
			if err := next.Set("apps."+p.App+"."+key, value); err != nil {
				return err
			}
		}
		if err := secretsgen.CheckGarageKeys(p.opts.Config, next); err != nil {
			return err
		}
		if err := p.opts.Save(next); err != nil {
			return fmt.Errorf("writing the secrets: %w. Garage was not touched, and the next run starts the rotation again", err)
		}
		*p.opts.Secrets = *next
		p.NewKeyID, p.newSecret = id, secret
		return nil
	}
	st.gate = func() error {
		s := p.opts.Secrets
		prev, _ := garage.SecretString(s, p.App, previousID)
		prevSecret, _ := garage.SecretString(s, p.App, previousKey)
		cur, _ := garage.SecretString(s, p.App, currentID)
		if prev != p.OldKeyID || prevSecret != p.oldSecret {
			return fmt.Errorf("apps.%s.%s is not the retiring key %s", p.App, previousID, p.OldKeyID)
		}
		if cur == "" || cur == p.OldKeyID || cur != p.NewKeyID {
			return fmt.Errorf("apps.%s.%s is not a new key", p.App, currentID)
		}
		return nil
	}
	return st
}

// buildImport is stage 2: the new key in Garage, beside the old one, with the
// same grant. Website access is a property of the bucket rather than of a
// key, so it is left exactly as it is.
func (p *Plan) buildImport(newPresent bool, bucket garage.Bucket) *Stage {
	st := &Stage{
		Name: "new key in Garage",
		Gate: fmt.Sprintf("%s's Garage holds %s, granted read/write/owner on %s", p.Anchor, p.newName(), p.Bucket),
	}
	if !newPresent {
		st.Steps = append(st.Steps, fmt.Sprintf("import %s on %s under the name %s", p.newName(), p.Anchor, p.App))
	}
	if !p.InProgress || !bucket.Grants(p.NewKeyID) {
		st.Steps = append(st.Steps, fmt.Sprintf("grant %s read/write/owner on %s; website access is the bucket's and is left as it is", p.newName(), p.Bucket))
	}
	// The steps are decided again from Garage when they run: the key ID is
	// not known at Build on a fresh rotation, and a resumed run may find
	// either step already done.
	st.run = func() error {
		var steps []garage.Step
		present, err := garage.KeyPresent(p.garage, p.NewKeyID)
		if err != nil {
			return err
		}
		if !present {
			steps = append(steps, garage.ImportStep(p.App, p.NewKeyID, p.newSecret))
		}
		b, err := garage.ReadBucket(p.garage, p.Bucket)
		if err != nil {
			return err
		}
		if !b.Grants(p.NewKeyID) {
			steps = append(steps, garage.GrantStep(p.App, p.Bucket, p.NewKeyID))
		}
		return garage.Execute(&garage.Plan{Site: p.Anchor, Steps: steps}, p.garage)
	}
	st.gate = func() error {
		present, err := garage.KeyPresent(p.garage, p.NewKeyID)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("%s is not in Garage", p.NewKeyID)
		}
		b, err := garage.ReadBucket(p.garage, p.Bucket)
		if err != nil {
			return err
		}
		if !b.Grants(p.NewKeyID) {
			return fmt.Errorf("`bucket info %s` shows no read/write/owner grant for %s", p.Bucket, p.NewKeyID)
		}
		return nil
	}
	return st
}

// buildSwitch is stage 3: every site running the app applies it alone.
func (p *Plan) buildSwitch() (*Stage, error) {
	st := &Stage{
		Name: fmt.Sprintf("switch %s to the new key", p.App),
		Gate: fmt.Sprintf("`apply --only %s` has nothing left to do on %s: the app runs with the rendered .env and passed apply's health gate", p.App, strings.Join(p.Sites, ", ")),
	}
	var todo []string
	for _, site := range p.Sites {
		if !p.InProgress {
			// The .env changes once stage 1 has run; with the secrets as
			// they are, apply would find nothing to do.
			st.Steps = append(st.Steps, fmt.Sprintf("%s: as `paisans apply --site %s --only %s`: write its .env with %s, recreate it, health gate", site, site, p.App, p.newName()))
			todo = append(todo, site)
			continue
		}
		pending, err := p.opts.Switch.Pending(site, p.opts.Secrets)
		if err != nil {
			return nil, err
		}
		if len(pending) > 0 {
			st.Steps = append(st.Steps, fmt.Sprintf("%s: as `paisans apply --site %s --only %s`: %s", site, site, p.App, strings.Join(pending, "; ")))
			todo = append(todo, site)
		}
	}
	st.run = func() error {
		for _, site := range todo {
			if err := p.opts.Switch.Apply(site, p.opts.Secrets); err != nil {
				return fmt.Errorf("%s: %w", site, err)
			}
		}
		return nil
	}
	st.gate = func() error {
		for _, site := range p.Sites {
			pending, err := p.opts.Switch.Pending(site, p.opts.Secrets)
			if err != nil {
				return fmt.Errorf("%s: %w", site, err)
			}
			if len(pending) > 0 {
				return fmt.Errorf("%s still has %s to apply: %s", site, p.App, strings.Join(pending, "; "))
			}
		}
		return nil
	}
	return st, nil
}

// buildProve is stage 4: the new key writes, reads and deletes a probe
// object, from every site running the app, through the S3 address the app
// itself is rendered with.
func (p *Plan) buildProve() *Stage {
	st := &Stage{
		Name:     "prove the new key",
		Verifies: true,
		Gate:     fmt.Sprintf("from every site running %s, %s writes %s/%s through %s's S3 API, reads it back byte for byte, and deletes it", p.App, p.newName(), p.Bucket, probeObject, p.Anchor),
	}
	for _, site := range p.Sites {
		st.Steps = append(st.Steps, fmt.Sprintf("%s: write, read back and delete %s/%s with %s through %s's S3 API (3900), the address the app is rendered with", site, p.Bucket, probeObject, p.newName(), p.Anchor))
	}
	st.gate = func() error {
		obj := garage.S3Object{Address: p.endpoint, Bucket: p.Bucket, Key: probeObject, KeyID: p.NewKeyID, Secret: p.newSecret, Region: p.region}
		for _, site := range p.Sites {
			t := p.opts.Transports[site]
			if out, err := t.RunInput(garage.CurlStdin, obj.CurlConfig("PUT", p.opts.Probe)); err != nil {
				return fmt.Errorf("%s: writing the probe with %s: %s", site, p.NewKeyID, obj.Redact(lastLines(out, 3)))
			}
			got, err := t.RunInput(garage.CurlStdin, obj.CurlConfig("GET", ""))
			if err != nil {
				return fmt.Errorf("%s: reading the probe with %s: %s", site, p.NewKeyID, obj.Redact(lastLines(got, 3)))
			}
			if got != p.opts.Probe {
				return fmt.Errorf("%s: reading the probe with %s returned %q, not the probe", site, p.NewKeyID, obj.Redact(lastLines(got, 1)))
			}
			if out, err := t.RunInput(garage.CurlStdin, obj.CurlConfig("DELETE", "")); err != nil {
				return fmt.Errorf("%s: deleting the probe with %s: %s", site, p.NewKeyID, obj.Redact(lastLines(out, 3)))
			}
		}
		return nil
	}
	return st
}

// buildRetire is stage 5: the old key out of Garage, then out of the secrets.
// It runs only after every earlier gate passed in the same run.
func (p *Plan) buildRetire(oldPresent bool) *Stage {
	st := &Stage{
		Name: "retire the old key",
		Gate: fmt.Sprintf("Garage no longer holds %s, and the secrets no longer hold a previous pair for %s", p.OldKeyID, p.App),
	}
	if oldPresent {
		st.Steps = append(st.Steps, fmt.Sprintf("delete %s from Garage on %s: garage key delete --yes %s", p.OldKeyID, p.Anchor, p.OldKeyID))
	}
	st.Steps = append(st.Steps, fmt.Sprintf("remove apps.%s.%s and %s from the secrets, and write the file", p.App, previousID, previousKey))
	st.run = func() error {
		// The last word before an irreversible step: the ID being deleted
		// is the retiring one, never the one the app now runs with.
		if p.OldKeyID == p.NewKeyID || p.OldKeyID == "" {
			return errors.New("the key to delete is not the retiring one")
		}
		present, err := garage.KeyPresent(p.garage, p.OldKeyID)
		if err != nil {
			return err
		}
		if present {
			if err := garage.Execute(&garage.Plan{Site: p.Anchor, Steps: []garage.Step{garage.DeleteKeyStep(p.OldKeyID)}}, p.garage); err != nil {
				return err
			}
		}
		next := cloneSecrets(p.opts.Secrets)
		delete(next.Apps[p.App], previousID)
		delete(next.Apps[p.App], previousKey)
		if err := p.opts.Save(next); err != nil {
			return fmt.Errorf("%s is deleted from Garage, but writing the secrets failed: %w. Run rotate-key again to remove the previous pair", p.OldKeyID, err)
		}
		*p.opts.Secrets = *next
		return nil
	}
	st.gate = func() error {
		present, err := garage.KeyPresent(p.garage, p.OldKeyID)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("Garage still holds %s", p.OldKeyID)
		}
		if id, _ := garage.SecretString(p.opts.Secrets, p.App, previousID); id != "" {
			return fmt.Errorf("the secrets still hold apps.%s.%s", p.App, previousID)
		}
		return nil
	}
	return st
}

// Execute runs the stages in order. A stage's steps run only when it has
// any; its gate always runs, so a resumed rotation proves each stage again
// before moving past it, and the old key is deleted only in a run that has
// just seen the app switched and the new key proven.
func Execute(p *Plan) error {
	for _, st := range p.Stages {
		p.say("stage %d, %s\n", st.Number, st.Name)
		if st.run != nil && len(st.Steps) > 0 && !st.Verifies {
			if err := st.run(); err != nil {
				return p.fail(st, err)
			}
		}
		if err := st.gate(); err != nil {
			return p.fail(st, fmt.Errorf("gate: %w", err))
		}
		p.say("  %-9s %s\n", "passed", st.Gate)
	}
	return nil
}

func (p *Plan) fail(st *Stage, err error) error {
	kept := fmt.Sprintf(" %s is still in Garage and still granted, so %s keeps working on whichever key it was last started with.", p.OldKeyID, p.App)
	if st.Name == "retire the old key" {
		kept = ""
	}
	return fmt.Errorf("storage rotate-key stopped at stage %d (%s), and nothing after it ran: %v.%s\nFix the cause and run storage rotate-key again: it resumes from the secrets file", st.Number, st.Name, err, kept)
}

func (p *Plan) say(format string, args ...any) {
	if p.Progress != nil {
		fmt.Fprintf(p.Progress, format, args...)
	}
}

// Print writes the plan as an operator reads it. It names key IDs, which are
// not secret, and never a secret key.
func (p *Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "storage rotate-key: %s, bucket %s, through %s's Garage\n", p.App, p.Bucket, p.Anchor)
	fmt.Fprintf(w, "  %-9s %s\n", "retiring", p.OldKeyID)
	if p.NewKeyID == "" {
		fmt.Fprintf(w, "  %-9s %s\n", "new", "generated at stage 1 when run with --execute")
	} else {
		fmt.Fprintf(w, "  %-9s %s (a rotation is in progress, resumed from the secrets file)\n", "new", p.NewKeyID)
	}
	for _, st := range p.Stages {
		fmt.Fprintf(w, "\n%d. %s\n", st.Number, st.Name)
		if len(st.Steps) == 0 {
			fmt.Fprintf(w, "  %-9s nothing to do here, and the gate is still checked\n", "nothing")
		}
		for _, step := range st.Steps {
			fmt.Fprintf(w, "  %-9s %s\n", "step", step)
		}
		fmt.Fprintf(w, "  %-9s %s\n", "gate", st.Gate)
	}
}

// cloneSecrets copies the secrets deep enough that changing one app's entries
// leaves the original alone until the write has succeeded.
func cloneSecrets(s *config.Secrets) *config.Secrets {
	next := *s
	next.Apps = make(map[string]map[string]any, len(s.Apps))
	for name, entries := range s.Apps {
		copied := make(map[string]any, len(entries))
		for k, v := range entries {
			copied[k] = v
		}
		next.Apps[name] = copied
	}
	return &next
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}
