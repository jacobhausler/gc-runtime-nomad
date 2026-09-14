---
name: gc-nomad-lane-debug
description: Diagnose a Gas City lane (pool) running on the Nomad runtime that is not serving - no seats launching, a seat that never answers, work sitting routed but unworked, or a seat that appears alive and does nothing. Walks queue -> demand -> seats -> boxes -> pane -> binding -> alloc events in order and names the layer whose evidence is lying. Use before theorizing about a starved pool, a stuck assignee, or a "dead" session.
---

# gc-nomad-lane-debug

Field diagnosis for "this lane is not serving" on a Nomad-managed Gas City lane.

Two skills, different jobs. `runtime-nomad.gc-nomad-debug` is the **pack's**
front door - its config keys, its lifecycle, its contract. Do not re-derive or
re-copy that here: the environment table lives in the pack's `README.md` and the
scope in `FRONT-DOOR.md`, and every hand-copied subset of those tables has
drifted and sent someone down a dead path. This page is the other question -
**which of the seven layers is lying to me right now** - and the one instrument
that answers each layer. Most lanes are not broken; one layer's summary column
disagrees with the layer below it and the operator stops at the summary.

## Gate 0 - is it us?

Before any instrument, rule out the three things the city does to itself:

- **A held bead left on a hook.** A blocked bead still on its hook is re-claimed
  forever, and the lane looks busy while nothing advances. `gc bd show <id>` for
  blocked-vs-hooked, and `blocked=` in the health line.
- **A bead that is not routable work.** Mail `message`, convoy, epic, session,
  spec and molecule beads are open-and-unrouted but not claimable by a pool.
  If your deployment ships a route-audit helper, it will say which and name the
decider; otherwise read `gc.routed_to` and the `hold:` labels off the beads
directly.
- **A permanent record that got routed.** A standing order / policy / invariant
  with `gc.routed_to` is re-claimed by a fresh seat every time the last one
  exits. Those beads wear `non-claimable` AND `hold:external` and carry no route.

The cheap entry signal is a city-health snapshot, not a probe. If your
deployment records one, read its newest line first: `ANOM:
pool_starved=<lane>:N` means demand with no serving seat, while `p0_unclaimed`,
`blk` and `non_work_ready` tell you whether the queue or the lane is the
problem. With no snapshot, the equivalent is one `gc ready` against the lane's
route plus `gc status` — start there rather than at a box.

## The walk

Go in this order and stop at the first layer whose evidence contradicts the one
above it. Each row is question -> instrument -> the lie that layer tells.

| # | Layer | Question | Instrument | The lie |
|---|-------|----------|-----------|---------|
| 1 | queue | is there claimable work for this lane? | `gc ready --json`, `route-candidates.sh --audit` | a `blocked` bead still on its hook reads as available work |
| 2 | demand | does the pool want another seat? | `pool_starved` in the snapshot, `gc config show` scale check | a scale check that excludes the lane's own bead types wants nothing it is being blamed for |
| 3 | seats | is a seat alive and holding the bead? | **the session bead**, `gc bd show <id>` | `gc session list`'s STATE column is NOT the session bead's state - it printed `asleep` for seats whose beads read `awake` with live boxes — measured on a live lane. Read the bead, not the table |
| 4 | boxes | is there an allocation, and is it this box? | `nomad job status` / `nomad alloc status` on the child job | `Meta.gc_session` names the pool **slot**, not a box. A replacement box wears the same name, so "assignee unchanged" proves nothing - compare `Meta.gc_created_at` and when the work actually ran |
| 5 | pane | did anything get launched inside the box? | `ps` inside the box | `tmux capture-pane` can print `no server running` purely because exec landed on a different socket path. `ps` is the honest instrument: pid 1 `sleep`, a tmux server, and either an agent process or a bare `sh`. Judge on the PROCESS LIST, never on the pane's foreground command - a pane body that wraps the command keeps the shell in front while the agent runs beneath it, so `pane_current_command` reads `sh`/`bash` on healthy seats too. An empty box is `sleep` + tmux + a shell and **nothing else** — measured on a live lane |
| 6 | binding | provisioned-but-never-launched, or launched-then-died? | the sidecar binding | it is the only place that holds `ChildJobID`, `Launched`, `AgentPID`, `StagePending`; every other layer guesses. `StagePending` with no `Launched` is the first case, `Launched` + dead `AgentPID` the second |
| 7 | alloc events | what did the driver actually say? | `nomad alloc status -json` task events | the **newest** event is usually the least informative - Nomad appends `Policy allows no restarts` after the real cause. Scan for the event carrying a driver / download / setup error (pull denied, OOM, constraint) |

Layer 0 sits under all seven: the **pack binary is resolved as a bare name on
PATH** (`pack.toml` `command = gc-runtime-nomad`). Before believing any fix is
live, ask which copy is actually on the *supervisor's* PATH and read it -
`strings` on that binary for a symbol or message unique to your change is the
only proof a patched pack is the one running. And the pack reads its config from
the **supervisor's** environment, never from the allocation, so a key added to a
deployment file is not in effect until that process is cycled.

## Read the ledger skeptically too

- `gc.lease_owner` can name a seat that no longer exists. An assignee is a
  string, not a heartbeat.
- `gc.routed_to` can be silently rewritten by a handoff or by
  `graph-hygiene-sweep`. Post-graph.v2 a handoff can leave `gc.routed_to` empty
  and write `gc.execution_routed_to` instead - so an empty route is not evidence
  of no route. Read `gc bd show <id> --json` and look at both keys.
- A claim made with `bd update --assignee` (a crew, not a gc session) registers
  no lease and no session bead, so every lease-scoped reaper is blind to it.
  That is a convention violation, not a defect - see `AGENTS.md`.

## Before theorizing about an idle lane, ask the controller

`gc trace start` then `gc trace show` records the reconciler's own decisions -
why it did not place, did not scale, did not route. Its JSON keys are
`site_code`, `reason_code`, `outcome_code`; grep those three rather than reading
prose. A lane the controller refused to scale and a lane whose seats all died
look identical from the queue and are different failures.

## Limits

Never reach into namespace `gpu-serving`. Never `gc supervisor stop|uninstall`,
never kill a process by name, never re-apply the control graph blindly. Keep
`GC_HOME` at gc's home for live-city work (`$GC_HOME/cache/repos` holds the
locked pack imports; pointing it at the city path makes every config-loading
command fatal, `gc hook --claim` included). Recover only rollback-safe actions;
a replacement box is fine, a store edit is not.

## When the walk ends

- Pack layer (5-7) misbehaving against documented config -> pack defect: raise it
  with `assets/bin/escalate.sh --bead <id> --symptom <stable-slug> --kind
  ruling --decider human`, never hand-rolled mail.
- gc layer (3, and the ledger above) -> mechanic-review as a typed handoff
  (`bounced:mechanic` + `gc.handoff_target=mechanic-review`, then `gc sling`).
- City layer (1-2) -> fix our own use; the exclusion predicate lives in
  a deployment-local route-audit helper, and the only bulk routing pass is
  `gc order run routing-pass --var target=<pool>`.

## Provenance

Every rule above was found by hand on a live Nomad lane, not derived from the
code: each row's "what lies" column is a mistake that actually cost a
diagnosis. Where a rule names a symptom, that symptom was observed, not
predicted.
