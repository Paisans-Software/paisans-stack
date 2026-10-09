package hostcaddy

import (
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// Kind is what a plan does to the server's configuration.
type Kind int

const (
	// Nothing means nothing is written: the block is already there as
	// wanted, or the toolkit cannot add it (Plan.Held says why).
	Nothing Kind = iota
	// CreateFile writes the toolkit's own file into the import directory.
	CreateFile
	// UpdateFile rewrites the toolkit's own file there.
	UpdateFile
	// AppendBlock appends the toolkit's marked block to the Caddyfile.
	AppendBlock
	// ReplaceBlock replaces the toolkit's marked block in the Caddyfile,
	// and nothing outside its markers.
	ReplaceBlock
)

func (k Kind) String() string {
	switch k {
	case CreateFile:
		return "create"
	case UpdateFile:
		return "update"
	case AppendBlock:
		return "append"
	case ReplaceBlock:
		return "replace"
	}
	return "unchanged"
}

// Site is one site block the toolkit wants on the server: a hostname and
// the block that serves it.
type Site struct {
	App      string
	Hostname string
	// HealthPath is checked after a reload.
	HealthPath string
	// Block is the whole site block, ingress.CaddyBlock with the upstream
	// this server reaches.
	Block string
}

// Plan is the one edit the toolkit would make to the server.
type Plan struct {
	Finding *Finding
	Kind    Kind
	// Path is the host file written, Before what it holds now ("" when it
	// does not exist) and After what it will hold.
	Path   string
	Before string
	After  string
	// Old and New are the toolkit's own block within it, before and after,
	// which is all that differs. Old is "" when there is none yet.
	Old, New string
	// Foreign is whether Path is a file the toolkit did not write (the
	// Caddyfile), and so is backed up before it is changed.
	Foreign bool
	Sites   []Site
	// Held says why nothing can be written, "" when something can or
	// nothing is needed. The blocks are then printed for the server's owner
	// to add by hand.
	Held string
}

// Pending reports whether the plan writes anything.
func (p *Plan) Pending() bool { return p != nil && p.Kind != Nothing }

// Markers name the toolkit's block by the deployment's token and full id, so
// a later apply recognises its own block and only its own: another
// deployment on the same host carries another id.
func beginMarker(d deployment.Deployment) string {
	return fmt.Sprintf("# BEGIN %s deployment %s: written by `paisans apply`. Do not edit between these lines; apply rewrites them.", d.Prefix(), d.ID)
}

func endMarker(d deployment.Deployment) string { return "# END " + d.Prefix() }

func fileHeader(d deployment.Deployment) string {
	return fmt.Sprintf("# %s deployment %s: written by `paisans apply`.\n# Do not edit: apply rewrites this file. Every other file here is left alone.\n", d.Prefix(), d.ID)
}

// PlanChange decides the edit that puts sites on the server f found, for
// deployment d. It reads the file it would write and changes nothing.
//
// A directory the Caddyfile already imports is preferred, because a file of
// the toolkit's own there leaves every byte of the owner's Caddyfile as it
// was. Without one, the block is appended to the Caddyfile between two
// marker lines, and only what lies between them is ever replaced.
func PlanChange(f *Finding, d deployment.Deployment, sites []Site, t Transport) (*Plan, error) {
	p := &Plan{Finding: f, Sites: sites}
	if f.Unsupported != "" {
		p.Held = f.Unsupported
		return p, nil
	}
	var body strings.Builder
	for i, s := range sites {
		if i > 0 {
			body.WriteString("\n")
		}
		body.WriteString(s.Block)
	}

	if f.Import != nil {
		if name, ok := f.Import.fileName(d.Token()); ok {
			p.Path = path.Join(f.Import.HostDir, name)
			p.New = fileHeader(d) + body.String()
			current, found, err := t.ReadFile(p.Path)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", p.Path, err)
			}
			switch {
			case !found:
				p.Kind = CreateFile
			case !strings.HasPrefix(current, fileHeader(d)):
				p.Held = fmt.Sprintf("%s exists and is not this deployment's (its first lines do not name deployment %s), so it is left as it is", p.Path, d.ID)
				return p, nil
			case current == p.New:
				p.Kind = Nothing
			default:
				p.Kind = UpdateFile
			}
			p.Before, p.Old, p.After = current, current, p.New
			if held := duplicate(f.Caddyfile, "", sites); held != "" {
				p.Kind, p.Held = Nothing, held
			}
			return p, nil
		}
	}

	p.Path, p.Foreign, p.Before = f.ConfigHost, true, f.Caddyfile
	p.New = beginMarker(d) + "\n" + body.String() + endMarker(d) + "\n"
	start, end, err := ownBlock(f.Caddyfile, d)
	if err != nil {
		p.Held = err.Error()
		return p, nil
	}
	if start < 0 {
		p.Kind = AppendBlock
		before := f.Caddyfile
		if before != "" && !strings.HasSuffix(before, "\n") {
			before += "\n"
		}
		if before != "" && !strings.HasSuffix(before, "\n\n") {
			before += "\n"
		}
		p.After = before + p.New
	} else {
		p.Old = f.Caddyfile[start:end]
		p.After = f.Caddyfile[:start] + p.New + f.Caddyfile[end:]
		if p.Old == p.New {
			p.Kind = Nothing
		} else {
			p.Kind = ReplaceBlock
		}
	}
	outside := f.Caddyfile
	if start >= 0 {
		outside = f.Caddyfile[:start] + f.Caddyfile[end:]
	}
	if held := duplicate(outside, f.ConfigHost, sites); held != "" {
		p.Kind, p.Held = Nothing, held
	}
	return p, nil
}

// duplicate says which wanted hostname a site block the toolkit did not
// write already serves. Caddy refuses a configuration with two blocks for one
// address, and refuses all of it rather than the block, so adding one would
// take every site on that server down at the reload.
func duplicate(caddyfile, where string, sites []Site) string {
	have := map[string]bool{}
	for _, a := range Parse(caddyfile).Sites {
		have[a] = true
	}
	for _, s := range sites {
		if have[strings.ToLower(s.Hostname)] {
			if where == "" {
				where = "the Caddyfile"
			}
			return fmt.Sprintf("%s already has a site block for %s that this toolkit did not write. Caddy refuses a second block for the same address, so nothing is added; that block is the owner's to keep or replace with the one below", where, s.Hostname)
		}
	}
	return ""
}

// ownBlock finds the toolkit's marked block for d: from its BEGIN line to
// the end of its END line, or -1 when there is none. A BEGIN line carrying
// d's token and another id is another deployment's, and is refused rather
// than read as this one's.
func ownBlock(content string, d deployment.Deployment) (int, int, error) {
	begin, end := beginMarker(d), endMarker(d)
	tokenBegin := "# BEGIN " + d.Prefix() + " "
	offset := 0
	start := -1
	for _, line := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case start < 0 && trimmed == begin:
			start = offset
		case start < 0 && strings.HasPrefix(trimmed, tokenBegin):
			return -1, -1, fmt.Errorf("the Caddyfile holds a block marked %s that names another deployment (%q), so it is left alone and nothing is added", d.Prefix(), trimmed)
		case start >= 0 && trimmed == end:
			return start, offset + len(line), nil
		}
		offset += len(line)
	}
	if start >= 0 {
		return -1, -1, fmt.Errorf("the Caddyfile has this deployment's BEGIN line and no %q after it, so where the block ends cannot be told. Put the END line back, or remove the block, and apply again", end)
	}
	return -1, -1, nil
}

// Print writes the plan for an operator: what was found, where the block
// goes and exactly what changes.
func (p *Plan) Print(w io.Writer) {
	f := p.Finding
	fmt.Fprintf(w, "\nthe host's own web server (ingress mode external):\n")
	fmt.Fprintf(w, "  found     %s, image %s", f.Name(), f.Image)
	if f.Version != "" {
		fmt.Fprintf(w, ", Caddy %s", strings.Fields(f.Version)[0])
	}
	fmt.Fprintln(w)
	switch {
	case f.HostNetwork:
		fmt.Fprintf(w, "  network   the host's own, so it reaches the app where ingress.listen publishes it\n")
	case f.Network != "":
		fmt.Fprintf(w, "  network   %s (it is at %s there); the app joins it as %s and trusts that address\n", f.Network, f.Address, upstreamHost(p.Sites))
	}
	if f.ConfigHost != "" {
		fmt.Fprintf(w, "  config    %s, %s on the host\n", f.Config, f.ConfigHost)
	}
	if p.Held != "" {
		fmt.Fprintf(w, "  HELD      %s\n", p.Held)
		fmt.Fprintf(w, "  Add this to that server's configuration yourself, then run `paisans ingress check`:\n\n")
		for _, s := range p.Sites {
			fmt.Fprint(w, indent(s.Block, "    "))
		}
		return
	}
	switch p.Kind {
	case Nothing:
		fmt.Fprintf(w, "  unchanged %s already serves %s as wanted\n", p.Path, hostnames(p.Sites))
		return
	case CreateFile, UpdateFile:
		fmt.Fprintf(w, "  %-9s %s, a file of this deployment's own in the directory the Caddyfile imports (%s)\n", p.Kind, p.Path, f.Import.Glob)
	case AppendBlock:
		fmt.Fprintf(w, "  %-9s a marked block at the end of %s; it imports no directory to add a file to, so the block goes in the Caddyfile itself and nothing else in it changes\n", p.Kind, p.Path)
		fmt.Fprintf(w, "  backup    %s.paisans-backup-<time>, before it is written\n", p.Path)
	case ReplaceBlock:
		fmt.Fprintf(w, "  %-9s this deployment's marked block in %s, and nothing outside it\n", p.Kind, p.Path)
		fmt.Fprintf(w, "  backup    %s.paisans-backup-<time>, before it is written\n", p.Path)
	}
	fmt.Fprintf(w, "  then      caddy validate, and caddy reload (graceful) in %s; a failure of either restores the file\n\n", f.Container.Name)
	fmt.Fprint(w, p.Diff())
}

// Diff is the change as removed and added lines: the toolkit's block as it
// was and as it will be, which is all that changes.
func (p *Plan) Diff() string {
	var b strings.Builder
	fmt.Fprintf(&b, "    --- %s\n    +++ %s\n", p.Path, p.Path)
	for _, line := range lines(p.Old) {
		fmt.Fprintf(&b, "    -%s\n", line)
	}
	for _, line := range lines(p.New) {
		fmt.Fprintf(&b, "    +%s\n", line)
	}
	return b.String()
}

// Execute makes the planned edit, validates it with the server's own Caddy,
// and reloads that Caddy gracefully. It changes nothing else. A file that
// changed since the plan read it is refused. A validation or reload that
// fails puts the file back as it was and stops.
func Execute(p *Plan, t Transport, at time.Time, w io.Writer) error {
	if !p.Pending() {
		return nil
	}
	f := p.Finding
	current, found, err := t.ReadFile(p.Path)
	if err != nil {
		return fmt.Errorf("reading %s again before writing it: %w", p.Path, err)
	}
	if found != (p.Kind != CreateFile) || current != p.Before {
		return fmt.Errorf("%s changed since this apply read it, so it was not written. Run apply again to plan against what it holds now", p.Path)
	}

	var restore string
	switch {
	case p.Foreign:
		backup := fmt.Sprintf("%s.paisans-backup-%s", p.Path, at.UTC().Format("20060102T150405Z"))
		if out, err := t.Run("cp -p " + shellQuote(p.Path) + " " + shellQuote(backup)); err != nil {
			return fmt.Errorf("backing up %s, so nothing was written: %w\n%s", p.Path, err, out)
		}
		fmt.Fprintf(w, "  backed up %s to %s\n", p.Path, backup)
		restore = "cat " + shellQuote(backup) + " > " + shellQuote(p.Path)
	case p.Kind == CreateFile:
		restore = "rm -f " + shellQuote(p.Path)
	}
	put := func(content string) error {
		// In place, so a Caddyfile bind mounted on its own keeps its inode
		// and the container sees the change; its owner and mode stay too.
		out, err := t.RunInput("umask 022; cat > "+shellQuote(p.Path), content)
		if err != nil {
			return fmt.Errorf("writing %s: %w\n%s", p.Path, err, out)
		}
		return nil
	}
	undo := func(why string, cause error) error {
		var err error
		if restore != "" {
			_, err = t.Run(restore)
		} else {
			err = put(p.Before)
		}
		if err != nil {
			return fmt.Errorf("%s: %w\nputting %s back failed too: %v. Restore it by hand before that Caddy next reloads", why, cause, p.Path, err)
		}
		return fmt.Errorf("%s, so %s was put back as it was and %s was not reloaded: %w", why, p.Path, f.Container.Name, cause)
	}

	if err := put(p.After); err != nil {
		if restore != "" && p.Foreign {
			_, _ = t.Run(restore)
		}
		return err
	}
	fmt.Fprintf(w, "  wrote     %s\n", p.Path)
	if out, err := t.Run(ValidateCommand(f)); err != nil {
		return undo("the server's Caddy does not accept its configuration with the block added", fmt.Errorf("%w\n%s", err, out))
	}
	fmt.Fprintf(w, "  validated with caddy validate in %s\n", f.Container.Name)
	if out, err := t.Run(ReloadCommand(f)); err != nil {
		return undo("the server's Caddy did not reload", fmt.Errorf("%w\n%s", err, out))
	}
	fmt.Fprintf(w, "  reloaded  %s (caddy reload, graceful)\n", f.Container.Name)
	return nil
}

// ValidateCommand checks the server's whole configuration with its own
// Caddy, as the container sees the files.
func ValidateCommand(f *Finding) string {
	return fmt.Sprintf("docker exec %s caddy validate --config %s --adapter caddyfile", shellQuote(f.Container.ID), shellQuote(f.Config))
}

// ReloadCommand hands the server its configuration through its admin
// endpoint, which swaps it in without dropping a connection. The container
// is never restarted or recreated: it is not the toolkit's.
func ReloadCommand(f *Finding) string {
	cmd := fmt.Sprintf("docker exec %s caddy reload --config %s --adapter caddyfile", shellQuote(f.Container.ID), shellQuote(f.Config))
	if f.Admin != "" && !strings.HasPrefix(f.Admin, "unix/") {
		cmd += " --address " + shellQuote(f.Admin)
	}
	return cmd
}

// ProbeCommand asks the server for a site from the host itself, by name, so
// the answer is the server's and its certificate's, not DNS's.
func ProbeCommand(f *Finding, s Site) string {
	return fmt.Sprintf("curl -sS -o /dev/null -w '%%{http_code}' --max-time 10 --resolve %s https://%s%s",
		shellQuote(s.Hostname+":443:"+f.ProbeAddress), s.Hostname, s.HealthPath)
}

// Probe reports whether each site answers through the server after the
// reload. It never fails the apply: a certificate the server has only just
// started to obtain answers with a TLS error for a minute or so, so a site
// that has not answered is reported with what to run once it has had time.
func Probe(p *Plan, t Transport, w io.Writer, attempts int, wait time.Duration, sleep func(time.Duration)) {
	for _, s := range p.Sites {
		var last string
		ok := false
		for i := 0; i < attempts; i++ {
			if i > 0 {
				sleep(wait)
			}
			out, err := t.Run(ProbeCommand(p.Finding, s))
			last = strings.TrimSpace(out)
			if err == nil && len(last) == 3 && (last[0] == '2' || last[0] == '3') {
				ok = true
				break
			}
			if err != nil && last == "" {
				last = err.Error()
			}
		}
		if ok {
			fmt.Fprintf(w, "  answered  https://%s%s with %s\n", s.Hostname, s.HealthPath, last)
			continue
		}
		fmt.Fprintf(w, "  not yet   https://%s%s: %s\n", s.Hostname, s.HealthPath, firstLine(last))
		fmt.Fprintf(w, "            a new certificate can take a few minutes; run `paisans ingress check --app %s` from your machine once it has had time\n", s.App)
	}
}

func hostnames(sites []Site) string {
	var names []string
	for _, s := range sites {
		names = append(names, s.Hostname)
	}
	return strings.Join(names, ", ")
}

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range lines(s) {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

// upstreamHost is the name the first site's block proxies to, for the plan's
// account of a server on a network of its own.
func upstreamHost(sites []Site) string {
	for _, s := range sites {
		for _, line := range lines(s.Block) {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "reverse_proxy "); ok {
				host, _, _ := strings.Cut(rest, ":")
				return host
			}
		}
	}
	return "its alias"
}
