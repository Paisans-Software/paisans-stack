package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

// Pocket ID's signup default groups are named in paisans.yaml and given to
// Pocket ID as IDs (kinds.PocketIDSignupGroupsSetting). An ID exists only
// once the group exists on the running instance, so the .env cannot carry one
// until apply has asked. This is that step: on a site running Pocket ID it
// finds each named group at the active instance, creates a missing one as the
// client step creates the groups a client admits, and records each ID under
// pocket_id_groups in the secrets file, where render reads it. The site is
// then planned again, the .env changes, and Pocket ID is recreated with it.
//
// Recording the ID rather than looking it up at render time keeps render a
// pure function of the two files, so every Pocket ID instance renders the
// same list and a dry run renders what an apply would. A recorded ID the
// instance no longer holds under that name (a database restored or started
// afresh) is replaced, because Pocket ID silently skips an ID it does not
// have (service/user_service.go:374-379 at tag v2.14.0), so a stale one would
// fail without a word.

// groupStep is what one signup group needs.
type groupStep int

const (
	groupPresent groupStep = iota
	groupRecord
	groupCreate
	// groupByClient is a group a client step in the same dry run creates.
	groupByClient
)

type groupPlan struct {
	name     string
	step     groupStep
	id       string
	recorded string
}

// ensureGroups plans each signup group against Pocket ID and, with execute,
// creates what is missing and records every ID that is not recorded yet.
// creating names groups a client plan in the same dry run creates.
func (c *clientStep) ensureGroups(api *pocketid.Client, where string, execute bool, creating map[string]bool, waiting bool) {
	if len(c.signupGroups) == 0 {
		return
	}
	fmt.Fprintf(os.Stdout, "\nsignup default groups of %s (pocket-id), as IDs for its .env\n", c.idp)
	plans, err := c.planGroups(api, creating)
	if err != nil {
		c.groupsCannotAsk(c.signupGroups, unreachable(fmt.Errorf("Pocket ID on %s could not be asked: %w", where, err)), waiting)
		return
	}
	for _, p := range plans {
		fmt.Fprintf(os.Stdout, "  %s\n", p.line(c.idp))
		if p.step != groupPresent {
			c.steps++
		}
	}
	if !execute {
		return
	}
	changed := false
	for _, p := range plans {
		switch p.step {
		case groupPresent:
			continue
		case groupCreate:
			g, err := api.CreateGroup(p.name)
			if err != nil {
				c.groupsErr = fmt.Errorf("signup group %s could not be created at Pocket ID: %w. Pocket ID runs without it until a re-run creates it", p.name, err)
				fmt.Fprintf(os.Stdout, "  %-9s %v\n", "refuse", c.groupsErr)
				return
			}
			p.id = g.ID
		}
		if c.secrets.PocketIDGroups == nil {
			c.secrets.PocketIDGroups = map[string]string{}
		}
		c.secrets.PocketIDGroups[p.name] = p.id
		changed = true
	}
	if !changed {
		return
	}
	if c.secrets.Encrypted && len(c.recipients) == 0 {
		c.groupsErr = fmt.Errorf("%s is encrypted, but no %s beside it names a recipient, so the signup group IDs could not be written back encrypted", c.secretsPath, config.SOPSConfigName)
		return
	}
	if err := config.WriteSecrets(c.secretsPath, c.secrets, c.recipients); err != nil {
		c.groupsErr = fmt.Errorf("recording pocket_id_groups: %w", err)
		return
	}
	for _, p := range plans {
		if p.step != groupPresent {
			fmt.Fprintf(os.Stdout, "  recorded pocket_id_groups.%s\n", p.name)
		}
	}
	// As the client step does: a call that answered and did not do its job
	// is the failure nobody notices, so ask again and expect nothing to do.
	again, err := c.planGroups(api, nil)
	if err != nil {
		c.groupsErr = fmt.Errorf("checking the signup groups after recording them: %w", err)
		return
	}
	for _, p := range again {
		if p.step != groupPresent {
			c.groupsErr = fmt.Errorf("signup group %s was recorded, but a fresh look at Pocket ID still plans: %s", p.name, p.line(c.idp))
			return
		}
	}
}

// planGroups reads each signup group at Pocket ID and decides its step. It
// only reads.
func (c *clientStep) planGroups(api *pocketid.Client, creating map[string]bool) ([]groupPlan, error) {
	var out []groupPlan
	for _, name := range c.signupGroups {
		g, err := api.FindGroup(name)
		if err != nil {
			return nil, fmt.Errorf("looking up group %s: %w", name, err)
		}
		p := groupPlan{name: name, recorded: c.secrets.PocketIDGroups[name]}
		switch {
		case g == nil && creating[name]:
			p.step = groupByClient
		case g == nil:
			p.step = groupCreate
		case g.ID == p.recorded:
			p.step, p.id = groupPresent, g.ID
		default:
			p.step, p.id = groupRecord, g.ID
		}
		out = append(out, p)
	}
	return out, nil
}

// line is how a dry run and an apply show one group's step.
func (p groupPlan) line(idp string) string {
	key := "pocket_id_groups." + p.name
	switch p.step {
	case groupPresent:
		return fmt.Sprintf("present group %s (id %s), recorded as %s", p.name, p.id, key)
	case groupByClient:
		return fmt.Sprintf("record %s once a client step above has created group %s, then render %s's .env again and recreate it", key, p.name, idp)
	case groupCreate:
		body, _ := json.Marshal(map[string]string{"name": p.name, "friendlyName": p.name})
		line := fmt.Sprintf("create group %s: POST /api/user-groups %s, record its ID as %s, then render %s's .env again and recreate it", p.name, body, key, idp)
		if p.recorded != "" {
			line += fmt.Sprintf(". The recorded ID %s is not a group on this instance", p.recorded)
		}
		return line
	default:
		if p.recorded == "" {
			return fmt.Sprintf("record %s %s, the ID of group %s, then render %s's .env again and recreate it", key, p.id, p.name, idp)
		}
		return fmt.Sprintf("record %s %s in place of %s, which this instance does not hold as group %s, then render %s's .env again and recreate it", key, p.id, p.recorded, p.name, idp)
	}
}

// groupsCannotAsk is what happens to the signup groups when Pocket ID cannot
// be asked. A recorded ID is rendered as it is, as a recorded client is used:
// it was right when it was recorded. A group with none is left out of the
// .env until a re-run resolves it. Neither fails the apply, since a re-run
// finishes it and Pocket ID runs either way.
func (c *clientStep) groupsCannotAsk(groups []string, why string, waiting bool) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(os.Stdout, "\nsignup default groups of %s (pocket-id), as IDs for its .env\n", c.idp)
	if waiting {
		fmt.Fprintf(os.Stdout, "  %-9s group %s once pocket-id %s on %s has started and answers, then render its .env again with their IDs and recreate it. It does not answer yet: %s\n",
			"resolve", strings.Join(groups, ", "), c.idp, c.site, why)
		c.steps++
		return
	}
	for _, g := range groups {
		if id := c.secrets.PocketIDGroups[g]; id != "" {
			fmt.Fprintf(os.Stdout, "  %-9s group %s: %s; its recorded ID %s is rendered as it is\n", "unchecked", g, why, id)
			continue
		}
		fmt.Fprintf(os.Stdout, "  %-9s group %s: %s; Pocket ID's .env leaves it out until a re-run resolves it\n", "skip", g, why)
	}
}
