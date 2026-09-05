# v1.0.3

Provision idempotency and start-retry adoption (`fnrt-t4l.46`).

## What broke

v1.0.2 taught `provision` to resume the box it had already dispatched, but
only for a binding it had positively marked `stage_pending` — a provision
that died mid-staging. That is one of two ways a live, unlaunched box gets
left bound to a session. The other one is what the DF-03 canary hit: the
provision half succeeded and finished staging, the launch was never
confirmed, and gc re-drove the verb every tick. `stage_pending` was already
cleared, so no resume applied, dispatch's already-exists guard rejected every
tick — 61 `session ... already exists` rejections while the alloc kept
running healthily underneath. The lane could never converge.

## What changed

- `liveChild` is now the single "this session already has a box" test that
  `provision`, `start`, and `dispatch` all key off, so the three cannot drift
  apart on what counts as an existing session (`resumeUnlaunched` and
  dispatch's guard are rewritten on it with no behavior change).
- `provision` adopts before dispatching: a live child that is NOT launched is
  adopted and staging is re-run against it. Re-staging is safe — stage's
  channels are overwriting tar extractions into fixed paths — so the adopted
  box converges on the state a fresh provision would have produced. This
  subsumes v1.0.2's narrower `stage_pending` resume, which is retired;
  the marker stays as the durable record of an unfinished staging half.
- A live child that IS launched is a genuine duplicate, not a retry, and is
  still rejected with the 04 §6 wire-contract `already exists` error gc reads
  as `ErrSessionExists`. The v1.0.2 contract test is rewritten onto that
  edge — the line this release moved.

`start` needed no change: its `resumeUnlaunched` path already resumed into a
provisioned box; this release pins that with tests at both rungs.

## Coverage

Ops-level tests for the adopting re-provision (asserting exactly one child
dispatch, via the fakenomad request trace), the launched-session rejection,
and start resuming into a provisioned box; the v1.0.2 fault-suite
already-exists test re-pointed at the launched edge. Plus a `conformance.sh`
rung that drives the binary's own CLI wire, since the failure was a
controller re-driving the verb every tick — negative-tested: against the
pre-fix `ops.go` it fails with exactly the incident's error.

The gc-side half (gc minted a NEW session bead per tick instead of resuming
on `provider_error: already exists`) is an upstream twin, drafted but not
filed — its code pointers need confirming against gascity `main` first.

# v1.0.2

Recovery fix for a provision wedge seen on a live cluster (`fnrt-t4l.18`).

## What broke

A session's NRT-P1-06 staging (workspace/secrets materialized over
tar-over-exec-stdin) runs AFTER its child job has been dispatched. When the
exec stream was torn down while the runtime was still WRITING that payload,
two defects compounded:

1. `writing exec stdin-close frame: broken pipe` (EPIPE) was not classified
   as an alloc-readiness failure, so staging did not retry it. Only the
   read-side shapes (EOF, ECONNRESET) were.
2. The failed staging left a live child bound to the session, so every later
   `provision` was rejected by dispatch's already-exists guard —
   `provision: session already exists`, every tick, forever. The session
   could never finish being provisioned and never be started.

## What changed

- `allocReadyRetryable` now also classifies the write-side and mid-frame
  tear-downs: `EPIPE`/`broken pipe` and `io.ErrUnexpectedEOF`. A genuinely
  dead stream still just exhausts the same bounded retry.
- Bindings carry a `stage_pending` marker, written before the first staging
  exec call and cleared once every staging call has succeeded. `provision`
  resumes staging into the box it already dispatched when — and only when —
  it finds its own binding in that state. A session whose staging completed
  is still a duplicate provision and is still rejected with `already exists`
  (04 §6 / R1a-02 wire contract, pinned by test). Bindings written before
  this field existed have no marker and are never resumed.

## Coverage

New `fakenomad` fault `HangupExecNext` resets the alloc-exec stream
immediately after the handshake, before any stdin frame is consumed — the
write-side twin of `ResetExecNext`, which can only break a stream after the
client has finished writing. Three fault-suite regressions ride it (wedge
recovery, transient absorption, the already-exists contract) plus a
classification unit test. Full offline ladder green — see
`CONFORMANCE-RECEIPT.md`.

# v1.0.0

First public release of `runtime-nomad`: a Gas City RPP v0 runtime pack that
drives session lifecycle (`start`/`stop`/`is-running`/`list-running`,
`provision`/`exec`/`relaunch`, plus the tmux-backed driving verbs) over a
HashiCorp Nomad cluster's job dispatch/deregister/blocking-read and
alloc-exec WebSocket APIs. See `README.md` for the full interface and
`FRONT-DOOR.md` for the operative summary.

## Maintenance offer

Posted to `gastownhall/gascity#5606` (question 1, 2026-08-25) and accepted by
maintainer csells on 2026-08-26:

> Registry admission and post-merge ownership. Would you take a
> third-party-maintained `runtime-nomad` into the registry, and after merge
> does the pack's CI and upkeep sit with me or with you? I'm volunteering to
> own it, with conformance running offline against a fake Nomad API.

Disposition: packs are published self-serve at `registry.gascity.com/publish`
from a repo the authors own and maintain -- no `gascity-packs` PR. Ownership
is Jacob Hausler and atbrace, in this repo (`jacobhausler/gc-runtime-nomad`),
with offline conformance run in this repo's own CI.

## Conformance

Full offline ladder (check, golden suite, env probes, `go vet`/`go test`
across both modules, secrets-grep) green at the tagged commit -- see
`CONFORMANCE-RECEIPT.md`, regenerated by `./receipt.sh` at this release.

## Program status

Runtime program gate G3 promoted 2026-08-27 (owner-delegated: lab proof +
clean soak round 1 + all five pack env-substitution bugs merged to `main`).
Fault-drill soak rounds 2-3 and the rollback drill are deferred post-v1.0
fast-follows, blocked on an unrelated city soak-infra defect, not a
runtime-nomad defect.
