# Deployment agent rules

> **This is a template, and it is a floor.** It is not authoritative for any
> community.
>
> It governs agents operating a *deployed* paisans stack. It does not govern
> agents working on the paisans-stack toolkit itself, which has its own
> `AGENTS.md` at the root of that repository.
>
> **Copy it into your deployment's own configuration repository as `AGENTS.md`**
> and edit it to fit. Under that name, every agent that opens that repository
> reads it without being told to.
>
> **A community's Charter still governs.** The Charter is that community's
> mission, values and code of conduct, and it lives in the community's wiki, not
> here. This file is a rulebook derived from those values, not a substitute for
> them. Where a rule here conflicts with the community's Charter, or with an
> explicit instruction from the community's admins, the Charter wins and the
> rule here is the one that is wrong.
>
> **The second situation it exists for** is a community that has lost its wiki
> along with everything else, where whoever is rebuilding it (person or agent)
> needs to know what may be done and by whom before the wiki comes back. That is
> why the rules here are written to stand alone. The mission, the values, the
> code of conduct, the split protocol and every choice specific to a community
> are absent by design, because they are not generic and because publishing them
> here would put a deployment's shape in a third party's repository.
>
> **After restoring from backup, reconcile against the real thing.** See *After
> a restore* at the end.

## What this covers

Two things, chosen because they are the ones that transfer between communities
without alteration:

* **Decision authority**: what an agent may settle alone, and what belongs to a
  human.
* **Blast radius**: how dangerous an action is, and how much human involvement
  it therefore requires.

Everything a community decides for itself is out of scope here, starting with
its values. Those live in its Charter.

## Decision authority

**An agent decides alone:** container names, flag values, file layout, document
structure, which runbook to write, how to word machine-generated sysadmin
documentation.

**A human decides:** which software fills a capability; anything changing the
identity model; federation policy; anything that locks in a migration path; when
a service is exposed to the public internet.

An agent choosing between two candidate applications has left its lane. Research
and recommend; the human chooses.

## Blast radius

When an action matches more than one tier, **the strictest applicable tier
governs**. An agent unsure which tier an action falls under must treat it as the
stricter one.

**Free — do it and record it**

* Reading host state
* Writing documentation
* Editing a repository on a branch
* Restarting a service that is already sanctioned and running

**Requires explicit human approval, every time**

* Stopping or removing containers
* Deleting volumes or images
* Reverse-proxy changes touching live routes
* Bringing up a new container or stack on a shared host — a first deployment,
  not a restart of something already sanctioned
* Any mutation of the identity provider's clients, groups or users
* Deleting documentation

**Never without the human present**

* Anything touching member data
* Exposing a service to the public internet
* Exposing a service that has no prior live route
* Changing a federation allowlist
* Changing private-network access control
* Deleting backups
* Removing or rotating the identity provider's API key

The distinction between the last two tiers matters and is easy to blur.
*Approved* means a human said yes to this specific action. *Present* means a
human is there while it happens, because the action cannot be undone and its
consequences reach people who are not in the room.

## If you are an agent reading this because the wiki is unreachable

You may act on this document, and you should say so before you do: *"the wiki is
unreachable; operating from the agent rules template."* Never let that pass
silently.

Four things bound it:

* **Read every tier as the stricter option.** This is a floor. The community's
  own rules may be stricter and you cannot check.
* **You cannot know you are alone.** Session coordination normally works through
  a claim written into the community's wiki. Without it, another session may be
  working the same area and neither of you can tell. Ask the human to confirm no
  other session is active before touching anything shared, and prefer work that
  cannot collide.
* **This document does not contain the community's particulars.** Which software
  fills a capability, federation policy, the identity model: all absent here,
  and all still a human's decision.
* **Record everything you do**, including anything done without the approval that
  would normally have been required, and why. It has to be reconciled once the
  wiki is back.

You know what tier an action falls in. You do not know this community's specifics
and you do not know who else is working. Act accordingly.

## During a recovery, specifically

A catastrophic recovery is unusual in one helpful way: **a human is definitionally
involved**, because someone is rebuilding a machine. That does not lower any tier.
It means the human-present requirement is already satisfied for the steps that
need it, and the approval requirement should be met by asking rather than
inferring from the emergency.

Emergencies are exactly when "they would obviously have said yes" feels most
reasonable and is most often wrong. If the community's own rules are
unreachable, assume they are **stricter** than this document, not more
permissive.

## After a restore

Once the wiki is back:

1. **Read the community's own `AGENTS.md` and its Charter.** Its own rules
   replace this template, and its Charter governs both.
2. **Re-check every assumption made during the recovery** against them. Anything
   done under this template that the community's own rules would have gated
   differently should be recorded, and raised with the community's admins.
3. **Record what was done while the wiki was unavailable**, including anything
   done without the approval that would normally have been required, and why.
4. **Reconcile this template with what the recovery taught you.** If the
   community's own rules differ, theirs are right. If the difference is
   something any community would want, it belongs upstream in the toolkit
   repository as well.

## For an operator adopting this stack

Copy this file into your deployment's configuration repository as `AGENTS.md`
and edit it to fit. It is deliberately generic, and a real community will want
to be more specific (Eg: who counts as "a human", which services are sanctioned,
which hosts an agent may reach, and anything its own history has taught it).

Then write your Charter, which is the part this template does not contain and
cannot: the mission, the values and the code of conduct that these rules exist
to serve.
