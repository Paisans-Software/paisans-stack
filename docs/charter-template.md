# Charter template — operational sections

> **This is a template, not a charter.** It is a **floor**, and it is not
> authoritative for any community.
>
> Every community's own Charter — which lives in that community's wiki, not here
> — supersedes this document entirely. Where the two differ, the community's
> Charter governs, and it may well be stricter.
>
> **This exists for one situation:** a community has lost its wiki along with
> everything else, and whoever is rebuilding it — **person or agent** — needs to
> know what may be done and by whom before the wiki comes back. It is the operational half only. The
> mission, the values, the code of conduct, the split protocol and every choice
> specific to a community are absent by design, because they are not generic and
> because publishing them here would put a deployment's shape in a third party's
> repository.
>
> **After restoring from backup, replace this with the real thing.** See
> *After a restore* at the end.

## What this covers

Two sections, chosen because they are the ones that transfer between
communities without alteration:

* **Decision authority** — what an agent may settle alone, and what belongs to a
  human.
* **Blast radius** — how dangerous an action is, and how much human involvement
  it therefore requires.

Everything else in a real Charter is community-specific.

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
unreachable; operating from the Charter template."* Never let that pass
silently.

Four things bound it:

* **Read every tier as the stricter option.** This is a floor. The community's
  own Charter may be stricter and you cannot check.
* **You cannot know you are alone.** Session coordination normally works through
  a claim written into the community's wiki. Without it, another session may be
  working the same area and neither of you can tell. Ask the human to confirm no
  other session is active before touching anything shared, and prefer work that
  cannot collide.
* **This document does not contain the community's particulars.** Which software
  fills a capability, federation policy, the identity model — absent here, and
  still a human's decision.
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
reasonable and is most often wrong. If the community's own Charter is
unreachable, assume it is **stricter** than this document, not more permissive.

## After a restore

Once the wiki is back:

1. **Read the community's own Charter.** It supersedes this file completely.
2. **Re-check every assumption made during the recovery** against it. Anything
   done under this template that the real Charter would have gated differently
   should be recorded, and raised with the community's admins.
3. **Record what was done while the wiki was unavailable**, including anything
   done without the approval that would normally have been required, and why.
4. **Confirm this template still matches** the operational sections of the real
   Charter. If they have diverged, the real Charter is right and this file needs
   updating upstream.

## For an operator adopting this stack

Copy these two sections into your own Charter and edit them to fit. They are
deliberately generic, and a real community will want to be more specific — about
who "a human" is, about which services count as sanctioned, and about anything
its own history has taught it.

Then write the parts this template does not contain, which are the parts that
make a community itself.
