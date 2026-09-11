---
name: gc-nomad-debug
description: Debug the runtime-nomad pack - a Gas City session that will not dispatch, launch, answer, do work, or stop cleanly on Nomad. Use for symptom classification, native gc and Nomad evidence, bounded checks, log and event evidence, rollback-safe recovery, escalation, and the exact stop conditions.
---

# gc-nomad-debug

Front door for debugging `runtime-nomad`, the pack that runs Gas City sessions
as Nomad allocations. It schedules and feeds a box; it does not know or ship
what runs inside it. Scope and interfaces live in `FRONT-DOOR.md`; the one
environment key table lives in `README.md` (Install, part 1, step 6). This page
points at those tables instead of copying them: every hand-copied subset of that
table has drifted, and a debug guide that drifts from the contract sends people
down dead paths.

## Gate 0 - misuse before mystery

Ask first whether the symptom is the pack doing what it is documented to do.
Most suspected provider defects are one of four things: a key edited into a
deployment file the supervisor never re-read, a parent registration that a
jobspec change made stale, placement that names a group with no eligible client,
or a last-known-good liveness read mistaken for a lie. Gate 0 survives only when
the documented path was actually followed and the symptom persists.

## Four laws that make the rest short

1. **One shape.** A session is a Docker task running the image named by the
   required `GC_NOMAD_AGENT_IMAGE`. Unset, every lifecycle op fails closed at
   startup. Rollback is a previous image tag. There is no older session shape to
   fall back to and no host-side launcher to repair.
2. **An executable, not a service.** `gc` execs `gc-runtime-nomad` fresh per
   op, and the pack reads its config from the **supervisor's** environment -
   never from the allocation. A key added to a deployment file is not in effect
   until the supervisor process is cycled: `gc reload` cannot revoke an
   environment the supervisor already composed, and a wrapper that sources a
   file wholesale can keep supplying a key the service definition no longer
   names.
3. **Five separate signals.** Sidecar binding, launched marker, child job,
   allocation `ClientStatus`, and the in-box process (tmux session plus the pane
   pid captured at launch). Each answers a different question; never infer one
   from another. `is-running` is false whenever the launched marker is unset
   even while the allocation runs, and false when the agent dies inside a live
   allocation - those are two different faults with the same answer.
4. **Two tokens, unequal powers.** The management token holds `submit-job` and
   registers the parameterized parent job once. Every session-path op -
   dispatch, launch, peek, stop, is-running, list-running - runs on the narrowed
   runtime token and can never repair parent drift. A missing or drifted parent
   is a re-registration condition, not a reason to widen the runtime token.

Exit-code contract: lifecycle ops return 0 success, 1 error, 2 unknown operation
(the RPP forward-compatibility signal callers treat as no-op success). `exec` is
the exception - it returns the remote command's own exit code.

## 1. Classify the symptom before collecting anything

| Symptom | Read first | Usual cause |
|---|---|---|
| Every lifecycle op fails at once | `gc-runtime-nomad check` | `GC_NOMAD_AGENT_IMAGE` unset; the required image fails every op closed |
| `parent job "gc-sessions" is stale - re-register with a management token` | parent `gc_jobspec_hash` vs current jobspec | Any task-spec, resolver, or placement change is a jobspec change; the runtime token cannot fix it |
| Child job exists, never places, no error | placement keys and scheduler events | A placement selection with no eligible client stays pending forever; look at placement facts, not dispatch or liveness |
| `provision` succeeded but `is-running` is false | launched marker, then allocation state | Provisioned-never-launched reads false by contract; relaunch into the same alloc, do not dispatch a second child |
| Agent died inside a live box | in-box probe result (`tmux has-session`, `kill -0 <pane pid>`) | Agent death, not box death: the allocation stays non-terminal and `ClientStatus` never shows it |
| Allocation is terminal and not replaced | allocation events | Box/task-supervisor death is a different case from agent death; a terminal alloc must not be replaced |
| Session names do not appear | `list-running [prefix]` exit code | It exits 1 on any lookup error rather than printing a partial list; a nonzero exit is a transport fault, not an empty fleet |
| Nothing inside the box resolves a name | `GC_NOMAD_DNS_SERVERS` | Unset means no resolver stanza, so the allocation inherits the client host's resolver file - often a loopback stub unreachable from the allocation namespace. Every downstream failure then looks like an agent or network defect |
| Work lands in a box that looks healthy and does nothing | `gc status`, session events, gc pin vs the fix that changed the claim gate | The identities the allocation sends must be a superset of the identities the claim gate matches; a shipped-but-unpinned fix is not in effect (`git merge-base --is-ancestor <fix-sha> <pin>`) |
| The agent inside fails closed on its target | staged `env.sh`, the provider env | The city target must be named exactly once: a complete remote context and a URL together is the fail-closed case, and the local-target family is stripped before launch by design |
| Session logs missing | `check` warning, `GC_NOMAD_LOG_SINK` | Unset, the shipper task is never added at all; disabling shipping is meant to be visible, not silent |
| `evidence_lost` on a stop, or no egress at all | sidecar tombstone, `GC_NOMAD_EGRESS_DIR` | Stop order is agent stop, shipper flush, egress, deregister; up to three egress retries then an explicit loss marker |
| A call you expected to work is silently a no-op | which op the caller actually named | Exit 2 on an unimplemented op is RPP forward compatibility, not a failure |
| A warning that a key is not a boolean, or a duration will not parse | that key's value | Bad config values are errors, and the operation stops naming the variable rather than defaulting |

## 2. Collect evidence with native verbs, in this order

City side - these are the record, and they are read-only alongside a live
controller:

```bash
gc status; gc doctor; gc rig status; gc agent list
gc trace status; gc trace start <template>; gc trace tail
gc trace reasons; gc trace cycle <tick-id>; gc trace show
gc events --type <type> --since 1h          # JSON Lines; the API is the source of truth
gc analyze reliability
gc runtime check; gc runtime conformance
```

Use `gc trace` first for any reconciler-decision question - why the controller
did or did not do X to a session - and `gc analyze` first for aggregate
questions across sessions. Fall back to raw event greps only where neither can
answer. Trace state persists under the city's runtime directory and stays
manageable even when the controller is offline.

Pack side - the pack's own diagnostics answer its contract without touching a
cluster:

```bash
gc-runtime-nomad check                 # warns on unset sink and unset required image
gc-runtime-nomad protocol              # the advertised RPP version and capabilities
gc-runtime-nomad is-running <session>  # and: list-running [prefix]
```

Cluster side, read-only, correlated by child job and allocation id:
`nomad job status`, `nomad job allocations`, `nomad alloc status`,
`nomad alloc logs`, `nomad evaluation status`, `nomad node status`.

Write configuration down as **names and pointers, never values**: key names, the
parent fingerprint, pack and Nomad versions, the fact that a TLS or custody
check passed, and custody-file paths. Allocation and child-job ids only when
they correlate two observations. A token value in a transcript is an incident,
not evidence.

## 3. Bounded checks

Run the narrowest check that can distinguish your two remaining hypotheses, and
stop when one of them dies.

1. **Hermetic, always first.** `GC_BIN=$(command -v gc) ./conformance.sh` builds
   the executable and an in-memory fake Nomad API and runs the full RPP
   lifecycle with no cluster and no network. It is the pack's CI gate. A defect
   that reproduces here needs no cluster to fix.
2. **Know what the fake cannot see.** The fake accepts loose JSON, so real
   drift classes show only against a real server: wire-level durations must be
   integer nanoseconds; a namespace must be a query parameter on every route
   that reads or mutates namespaced jobs, allocations, filesystem entries, or
   exec connections (a namespace in the registration body, or a header, is not
   equivalent); and a placement selection that no client satisfies is a real
   scheduler outcome the fake does not model. Do not "fix" the fake to match a
   real rejection - record the drift.
3. **Environment probe before code.** Every row of the key table has a failure
   mode that looks like a defect downstream (image, resolvers, egress, target
   naming, adopt age). Read the environment the supervisor actually started
   with before reading source.
4. **Read-only cluster inspection** is bounded by default: status, logs,
   evaluations, node state. Nothing here mutates the estate.
5. **A live fault drill is not a debugging step.** It is a gated activity with
   its own owner bead, isolated target, snapshot, credential custody, and
   rollback path. Absent all of those, stop and escalate instead.

## 4. Logs and events

- Live session-log continuity is opt-in and visible: `GC_NOMAD_LOG_SINK` is the
  one key that adds the `log-shipper` task (a pinned, checksum-verified
  archive). It tails the agent's session JSONL and captured stdout to an HTTP
  JSON-lines sink and exposes the shipper's own metrics through its built-in
  Prometheus exporter on a group-local port - that endpoint is the health
  signal for shipping itself.
- `GC_NOMAD_LOG_SINK_TOKEN_FILE` names an **in-box path** the shipper reads its
  bearer token from; `GC_NOMAD_LOG_LABELS` merges deployment labels onto the
  fixed `session_name` / `alloc_id` / `node` / `runtime` set. Use those labels
  to keep one session's stream attributable without naming a host.
- Stop-path evidence is a different mechanism from streaming: with
  `GC_NOMAD_EGRESS_DIR` set, `stop` copies the transcript and evidence files
  before deregistering, and records either a completion receipt or an explicit
  `evidence_lost` tombstone. Silent loss is a defect; a recorded loss is a
  finding. A flush failure also records `evidence_lost`.
- Client-side allocation garbage collection can reap terminal allocations while
  egress is in flight - correlate alloc id, egress marker, and deregistration
  order before calling it data loss. It must never reap a live allocation.
- On the gc side, `gc events` is the transport record and `gc trace` the
  decision record; keep both, with the tick id of the cycle that made the
  decision you are questioning.

## 5. Rollback-safe recovery

Because there is one shape, recovery is short and always reversible:

1. Name the rollback before touching anything. For an image problem the
   rollback is a previous image tag - confirm the tag exists, then repoint
   `GC_NOMAD_AGENT_IMAGE`, cycle the supervisor, and verify with
   `gc-runtime-nomad check` plus a disposable session.
2. Anything that changes the jobspec - image, resolvers, placement, task spec -
   requires re-registering the parent with the management token, or the pack
   refuses to dispatch. Prove the session path never bootstraps by setting
   `GC_NOMAD_FORBID_REGISTER=true` while testing.
3. Adopt, do not pile up. A live-but-never-launched box is adopted and
   re-staged by a repeated `provision`/`start`; only a live, already-launched
   session is a genuine duplicate. A live-but-unlaunched box older than
   `GC_NOMAD_ADOPT_MAX_AGE` is reaped and replaced rather than adopted.
4. `stop` is idempotent: no binding is a no-op success, and a stop that failed
   after egress does not redo egress on retry. Use the graceful provider stop
   and a zero-non-terminal-child check before reaching for anything heavier.
5. Never re-register the parent with the runtime token, never force a new
   allocation over a live one, and never downgrade a Nomad binary in place where
   the upgrade path calls for restore into a fresh single-server cluster.
6. After any teardown, audit residue at the depth the evidence requires -
   jobs, allocations, evaluations, alloc directories, sidecar records, egress
   store, staged config drops, shipper leftovers, temp files. A clean process
   list is not a clean residual audit.
7. A snapshot restore on a cloud-init image regenerates SSH host keys;
   re-baseline them before trusting SSH to that node again.

## 6. Classify before choosing an owner

| Classification | Evidence pattern | Next owner |
|---|---|---|
| Pack defect | The documented contract fails against valid input, and the mechanism is in this pack's source | Pack fix plus a regression in the conformance ladder |
| Nomad/API behavior | The fake accepts it, a real server rejects or schedules it differently, and the difference is a documented wire, query, ACL, or scheduler contract | Pack compatibility change, with the version evidence attached |
| Gas City defect | Pack and Nomad state are right; `gc` invoked the operation with the wrong contract or misread its result | The gc-side defect path (`gc-debug` and its twin) |
| Configuration or operations | The pack behaved as documented, but the parent fingerprint, namespace, placement, custody, TLS, timing, or rollback precondition was wrong | Fix the owned deployment record - not a product bug until that mismatch is closed |
| Harness gap | The observation is not representable in the fake or the reconciler simulator, or the simulator is being read as real gc | Extend the harness with an explicit scope; the lab receipt stays the authority |

## 7. Escalate onto a durable record

Mail, chat, and terminal scrollback are notifications, not the record: a read
message is swept away within the hour and a closed one leaves every inbox.
Anything that must outlive being read - a pending ruling, a hold, a hand-off -
goes onto the durable work record your city keeps, not into a message body.

- One record per (subject, condition), keyed by a stable name for the condition
  rather than this instance, so a second seat that hits the same defect appends
  evidence instead of opening a second question and a second notification.
- A ruling that must stop the governed work carries a blocking edge onto that
  work and takes it off the surface that re-claims it; a report stops nothing.
- The record carries: the one-sentence contract, expected vs observed with the
  provenance of each observation (pack commit, gc pin, gc and Nomad versions),
  the check ladder you ran and what each result ruled out, the mechanism at
  file and line, and at least two fix candidates with their blast radii.
- Rulings are written back onto the same record; a message only announces its
  id. A condition that genuinely recurred after a ruling escalates again
  pointing at the ruling it followed.

## 8. Stop conditions - exact

Stop immediately, and escalate rather than work around, on any of:

- a command's output would print a secret, token, key, or keyring - including an
  environment dump; use the custody path or a receipt reference instead;
- a session-path operation appears to need management registration, a
  system-wide garbage collection, or a node-level action: that is a boundary
  violation, not a workaround;
- unexplained replacement allocations, data loss on a live allocation, or
  evidence loss with no `evidence_lost` marker;
- a custody file whose permissions drifted, or a certificate at or near expiry;
- any reach toward a shared, production, or otherwise unowned target outside
  the one isolated target named by the owning bead;
- a drill time bound exceeded, or the isolated service cannot be restored
  cleanly;
- two consecutive hypotheses disproved with no new evidence - the classification
  is wrong, and re-running the same ladder is not progress;
- the fix would require editing the test double to match a real rejection.

## 9. Never run

- `nomad operator gossip keyring list`, `nomad operator keygen`, or
  `nomad operator gossip keyring generate` unredirected;
- `cat`, `grep`, `sed`, or any reader against a server config, a file containing
  an encryption key, or any custody file;
- `echo`, `env`, `set`, `printenv`, or a wrapper that prints a token-bearing
  environment;
- a `nomad` write - stop, deregister, system garbage collection - against a
  namespace or job you do not own; a pinned serving job belongs to someone else
  even when your lane has cluster access;
- `gc supervisor stop`, or any process kill selected by name;
- a Go build, test, vet, or lint on a host whose estate rules forbid local Go
  runs - route it through that estate's admitted builder.

Credential-generating commands, where authorized for an isolated lab operation,
write directly to the owner-approved custody path and never into a transcript.

## What this page is not

It is not an upstream-defect filing procedure - that is `gc-debug`, which
demands a disposable reproduction against pinned edge provenance before anything
is published. It is not a fault-drill runbook, and it is not a list of one
deployment's values: endpoint, custody paths, resolver addresses, and placement
state belong to that deployment's own record, and nothing in this pack should
need editing to change them.
