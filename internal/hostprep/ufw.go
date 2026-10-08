package hostprep

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// What follows is how the Ubuntu profile reads and writes ufw rules. Every
// claim about ufw's behaviour is from its source at tag 0.36.2, the version
// Ubuntu 24.04 ships (0.36.2-6), at https://git.launchpad.net/ufw/tree/src
// (read 2026-10-06):
//
//   - parser.py, UFWCommandRule.parse: `comment '<text>'` is accepted on an
//     allow rule, and a comment containing a single quote is refused
//     ("Comment may not contain \"'\"").
//   - parser.py, UFWCommandRule.get_command: `ufw show added` prints each rule
//     as the command that would add it, with ` comment '<text>'` last, after
//     `proto`, and only when the rule has a comment.
//   - common.py, UFWRule.match: two rules that differ only in their comment
//     match "excepting comment" (-2); in action or log type as well, "all but
//     action" (-1).
//   - backend_iptables.py, set_rule: adding a rule that matches an existing
//     one in everything but action, log type or comment *replaces it in
//     place* rather than adding a second; deleting with no comment removes a
//     rule that has one ("Allow removing a rule if the comment is empty").
//
// The last point decides two things here. A rule is identified by everything
// except its action and comment, because that is how ufw identifies it, and
// adding ours over someone else's would rewrite theirs. And adopting a rule an
// earlier prepare added without a comment is one command, not two.

// ownerTag starts the comment on every rule host prepare adds for deployment
// d, paisans-<token>:. It is how a later prepare tells its own rules from an
// operator's and from another deployment's on the same host: a rule is ours
// only when its comment starts with exactly this tag.
func ownerTag(d deployment.Deployment) string { return d.Prefix() + ":" }

// ufwSpec is a rule as ufw prints it back, without the comment.
func ufwSpec(r Rule) string {
	if r.Interface != "" && r.To != "" {
		return fmt.Sprintf("allow in on %s to %s port %d proto %s", r.Interface, r.To, r.Port, r.Proto)
	}
	if r.Interface != "" {
		return "allow in on " + r.Interface
	}
	return fmt.Sprintf("allow %d/%s", r.Port, r.Proto)
}

// ufwArgs is the ufw command that adds a rule, without the leading `ufw`. It
// is also exactly how `ufw show added` prints the rule back.
func ufwArgs(tag string, r Rule) string {
	return fmt.Sprintf("%s comment '%s %s'", ufwSpec(r), tag, r.Why)
}

// addedRule is one line of `ufw show added`, without its leading `ufw`.
type addedRule struct {
	line    string // verbatim, comment included, so a delete names it exactly
	action  string // "allow", "deny", "limit", "reject", with "log"/"log-all"
	key     string // everything else: what ufw matches rules on
	comment string
}

func (a addedRule) owned(tag string) bool { return strings.HasPrefix(a.comment, tag+" ") }

// commentSuffix is the comment get_command appends. A comment cannot contain a
// quote, so the last quoted string on the line is the whole of it.
var commentSuffix = regexp.MustCompile(`^(.*) comment '([^']*)'$`)

func parseAdded(line string) (addedRule, bool) {
	line = strings.TrimSpace(line)
	// Route rules govern forwarded traffic, which nothing here manages.
	if line == "" || strings.HasPrefix(line, "route ") {
		return addedRule{}, false
	}
	a := addedRule{line: line}
	body := line
	if m := commentSuffix.FindStringSubmatch(line); m != nil {
		body, a.comment = m[1], m[2]
	}
	a.action, a.key = splitAction(body)
	return a, true
}

// splitAction separates the action and log type from the rest of a rule. The
// log type follows the interface in ufw's full syntax, so it is lifted out
// wherever it falls.
func splitAction(spec string) (action, key string) {
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return "", ""
	}
	action = fields[0]
	var rest []string
	for _, f := range fields[1:] {
		if f == "log" || f == "log-all" {
			action += " " + f
			continue
		}
		rest = append(rest, f)
	}
	return action, strings.Join(rest, " ")
}

// overlaps reports whether a rule ufw holds bears on the traffic a derived
// rule allows: the same port, or the same interface. It is deliberately loose,
// because it decides only what the plan mentions, never what it changes.
func overlaps(a addedRule, r Rule) bool {
	fields := strings.Fields(a.key)
	if r.Interface != "" {
		on := false
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "on" && fields[i+1] == r.Interface {
				on = true
			}
		}
		// A rule for one port on an interface bears only on that port there.
		if !on || r.To == "" {
			return on
		}
	}
	port := strconv.Itoa(r.Port)
	names := func(s string) bool {
		s, proto, _ := strings.Cut(s, "/")
		if proto != "" && proto != r.Proto {
			return false
		}
		for _, p := range strings.Split(s, ",") {
			if p == port {
				return true
			}
		}
		return false
	}
	for i, f := range fields {
		if i == 0 && names(f) {
			return true // the short syntax: `allow 22/tcp`, `allow 22`
		}
		if f == "port" && i+1 < len(fields) && names(fields[i+1]) {
			return true
		}
	}
	return false
}

// planRules compares the rules a site derives with what ufw holds. It returns
// the additions, which go before the default policy and enabling, and the
// removals, which go after everything else.
func planRules(tag string, rules []Rule, added []addedRule) (out Section, removals []Step, err error) {
	var ssh *Rule
	for i := range rules {
		if rules[i].SSH {
			ssh = &rules[i]
		}
	}
	derived := map[string]bool{}
	byKey := map[string]addedRule{}
	for _, a := range added {
		if _, seen := byKey[a.key]; !seen {
			byKey[a.key] = a
		}
	}

	for _, r := range rules {
		if strings.Contains(r.Why, "'") {
			return out, nil, fmt.Errorf("the reason for %s contains a single quote, which ufw refuses in a comment", r)
		}
		action, key := splitAction(ufwSpec(r))
		derived[key] = true
		a, found := byKey[key]
		switch {
		case !found:
			out.Steps = append(out.Steps, Step{Describe: fmt.Sprintf("firewall: allow %s (%s)", r, r.Why), Command: "ufw " + ufwArgs(tag, r)})
		case a.owned(tag) && a.action == action:
			out.Present = append(out.Present, fmt.Sprintf("firewall: %s allowed", r))
		case a.owned(tag):
			// Ours, changed by hand since. Adding it again replaces it.
			out.Steps = append(out.Steps, Step{Describe: fmt.Sprintf("firewall: allow %s (%s), replacing `ufw %s`", r, r.Why, a.line), Command: "ufw " + ufwArgs(tag, r)})
		case a.comment == "" && a.action == action:
			// What an earlier version of host prepare added, before rules
			// carried a comment. ufw replaces it in place when the commented
			// rule is added, so there is no moment without it, and no second
			// command: `ufw delete` of the uncommented form would match the
			// commented rule too, and remove the very rule just adopted.
			out.Present = append(out.Present, fmt.Sprintf("firewall: %s allowed, by a rule without the paisans comment", r))
			out.Steps = append(out.Steps, Step{
				Label:    "adopt",
				Describe: fmt.Sprintf("firewall: mark `ufw %s` as host prepare's (%s); ufw rewrites it in place", a.line, r.Why),
				Command:  "ufw " + ufwArgs(tag, r),
			})
		case a.action == action:
			out.Foreign = append(out.Foreign, fmt.Sprintf("firewall: %s allowed by `ufw %s`", r, a.line))
		case r.SSH:
			if strings.HasPrefix(a.action, "deny") || strings.HasPrefix(a.action, "reject") {
				// Refused, not warned: enabling the firewall with this in
				// place shuts out the connection prepare runs over.
				return out, nil, fmt.Errorf("firewall: `ufw %s` blocks SSH, and host prepare did not add it. Enabling the firewall with it in place would lock out the next connection; delete or change it yourself and prepare again", a.line)
			}
			fallthrough
		default:
			// Adding ours would replace theirs, since ufw keeps one rule per
			// match. A rule host prepare did not create is not its to rewrite.
			out.Foreign = append(out.Foreign, fmt.Sprintf("firewall: `ufw %s`", a.line))
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"firewall: %s is governed by `ufw %s`, which host prepare did not add, so it is not allowed as derived (%s). Adding the allow would replace that rule, so it is left alone; change or delete it yourself if the allow is wanted",
				r, a.line, r.Why))
		}
	}

	for _, a := range added {
		if derived[a.key] {
			continue
		}
		if !a.owned(tag) {
			for _, r := range rules {
				if overlaps(a, r) {
					out.Foreign = append(out.Foreign, fmt.Sprintf("firewall: `ufw %s` (bears on %s)", a.line, r))
					break
				}
			}
			continue
		}
		// Never removed, even when ours: the SSH allow is the route this
		// command and every later one arrive by. With incoming denied by
		// default, removing it drops the next connection, and recovering
		// takes a console. A stale SSH allow costs almost nothing; a wrong
		// removal costs the host.
		if ssh != nil && overlaps(a, *ssh) {
			out.Present = append(out.Present, fmt.Sprintf("firewall: `ufw %s` kept; host prepare never removes an SSH allow", a.line))
			continue
		}
		// The same holds for the allow an earlier ssh.port was given, which
		// is recognised by its comment. Moving the port adds the new allow
		// and leaves the old one: host prepare cannot know that sshd already
		// listens on the new port, and if it does not, the old allow is the
		// only way back in. The operator deletes it once the new port works.
		if a.comment == tag+" "+sshWhy {
			note := fmt.Sprintf("firewall: `ufw %s` kept; it is the SSH allow for an earlier ssh.port, and host prepare never removes an SSH allow", a.line)
			if ssh != nil {
				note += fmt.Sprintf(". Delete it yourself once SSH on %d works", ssh.Port)
			}
			out.Present = append(out.Present, note)
			continue
		}
		removals = append(removals, Step{
			Label:    "remove",
			Describe: fmt.Sprintf("firewall: delete `ufw %s`, which host prepare added and this site's roles and configuration no longer give", a.line),
			Command:  "ufw delete " + a.line,
		})
	}
	return out, removals, nil
}
