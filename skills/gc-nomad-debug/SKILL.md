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

5. **The `gc` driving the pack is a dependency, and one half of it now fails
   closed.** A seat inside an allocation claims work through the remote-worker
   API. Driven by a `gc` without that family it cannot claim at all: the box
   comes up, the agent launches, the pane looks healthy, and the seat drains
   `no_work` while the city lists its work as ready. The pack refuses to mint a
   box on such a `gc` and names the family
   (`remote-worker-lifecycle`) — so `gc-runtime-nomad check` exiting nonzero on
   this is an answer, not an obstacle. The refusal covers ONE dependency;
   `docs/PIN-DEPENDENCIES.md` tiers the rest, and the others still go dark.

   **There are TWO `gc` binaries and only the second one claims.** The startup
   gate probes the `gc` on the supervisor's PATH — the one driving the pack.
   The one that actually claims work is baked into the agent image. A
   deployment can pass the gate with a compliant host `gc` and still hand every
   box a base-lineage one, which produces green boxes and dark seats with the
   gate certifying them. The runtime probes the in-box `gc` once at launch and
   warns when it cannot claim, naming the AGENT IMAGE rather than the
   supervisor. If `check` passes and seats still drain `no_work`, that warning
   is where to look — and the probe in the table's first row, run inside the
   image.

   **A FAIL alongside boxes that are up and running is normal, not a
   contradiction.** The gate refuses only the ops that MINT a box — `start`,
   `provision`, `relaunch`. `stop`, `is-running`, `list-running` and `peek`
   deliberately keep working, because a box that already exists must stay
   stoppable or a startup refusal becomes stranded allocations. So boxes
   minted before the gate landed, or before the `gc` on PATH changed, go on
   running and looking healthy while `check` fails. Do not discard a correct
   diagnosis because live allocations seem to disprove it.

Exit-code contract: lifecycle ops return 0 success, 1 error, 2 unknown operation
(the RPP forward-compatibility signal callers treat as no-op success). `exec` is
the exception - it returns the remote command's own exit code.

## 1. Classify the symptom before collecting anything

Run `gc-runtime-nomad check` before reading this table, and read its output
bottom-up: a **FAIL supersedes every warning above it.** `check` warns about
several unset keys that are each a real fault on their own, so a reader working
top-down can fix the warning, feel progress, and still have the lane that does
nothing. The FAIL is the answer; the warnings are a to-do list.

| Symptom | Read first | Usual cause |
|---|---|---|
| **Seats drain `no_work` while the city lists work as ready** — box healthy, agent up, pane fine | `gc-runtime-nomad check` **first**. To confirm by hand, or to test the `gc` INSIDE the image, run the gate's own probe: `gc bd claim probe --city-url https://127.0.0.1:1 --city-name probe` | The driving `gc` lacks `remote-worker-lifecycle`, so the seat cannot claim at all. `check` names it and exits nonzero. **Do not reach for `gc version --json`** — it prints a commit and the literal string `dev`, which looks perfectly normal and only incriminates if you already know that commit is the upstream base. The probe answers directly: `remote worker subset` in the refusal means the family is PRESENT; `remote support is being enabled incrementally` means it is ABSENT. If the host passes, suspect the **in-box** `gc` — the gate probes the host's, the image's is the one that claims |
| **Every seat reports HEALTHY and the pool mints nothing, while routed work sits** — no drains, no errors, no starvation alarm | `gc-runtime-nomad check` **first**. It FAILS when it OBSERVES the refusal (403); when it cannot determine the capability at all — no address configured, cluster unreachable — it WARNS instead, and that warning is not an all-clear. Read which of the two you got. Then this pack's own `diagnostics.log` under `GC_NOMAD_SIDECAR_DIR` | The credential cannot LIST ALLOCATIONS. Ops that MINT a box keep working, so nothing looks broken — but `is-running` has no evidence except the allocation list, so it answers last-known-good on every call and every session reports healthy. A pool whose seats all report healthy sees no starvation and mints no replacement, so routed work sits against a lane that looks alive. Distinguish from the first row by the SYMPTOM: there, seats drain `no_work`; here they never drain at all, because nothing observes them. The usual cause is no token reaching the runtime — and note WHERE it is read from: the environment of the process running this pack, i.e. the **supervisor's**, not your shell's. A token you can see in your terminal is not a token the runtime has. Confirm with a bare HTTP status, never by printing the token: `curl -s -o /dev/null -w '%{http_code}' -H "X-Nomad-Token: $TOKEN" "$GC_NOMAD_ADDR/v1/jobs?namespace=$NS"` — 403 with no token and 200 with a good one |
| Every lifecycle op fails at once | `gc-runtime-nomad check` | `GC_NOMAD_AGENT_IMAGE` unset; the required image fails every op closed |
| `parent job "gc-sessions" is stale - re-register with a management token` | parent `gc_jobspec_hash` vs current jobspec | Any task-spec, resolver, or placement change is a jobspec change; the runtime token cannot fix it |
| Child job exists, never places, no error | placement keys and scheduler events | A placement selection with no eligible client stays pending forever; look at placement facts, not dispatch or liveness |
| `provision` succeeded but `is-running` is false | launched marker, then allocation state | Provisioned-never-launched reads false by contract; relaunch into the same alloc, do not dispatch a second child |
| Agent died inside a live box | in-box probe result (`tmux has-session`, `kill -0 <pane pid>`) | Agent death, not box death: the allocation stays non-terminal and `ClientStatus` never shows it |
| Box up, nothing running in it | the PROCESS LIST (`ps -eo comm`), never the pane's foreground command | a pane body that wraps the command keeps the shell in front while the agent runs beneath it, so `pane_current_command` reads `sh`/`bash` on healthy seats too. An empty box is `sleep` + a tmux server + a shell and **nothing else**. This requires `ps` in the image: without it the probe cannot run, and the runtime now refuses the launch and says so rather than reporting health it cannot measure |
| Allocation is terminal and not replaced | allocation events | Box/task-supervisor death is a different case from agent death; a terminal alloc must not be replaced |
| Session names do not appear | `list-running [prefix]` exit code | It exits 1 on any lookup error rather than printing a partial list; a nonzero exit is a transport fault, not an empty fleet |
| Nothing inside the box resolves a name | `GC_NOMAD_DNS_SERVERS` | Unset means no resolver stanza, so the allocation inherits the client host's resolver file - often a loopback stub unreachable from the allocation namespace. Every downstream failure then looks like an agent or network defect |
| Work lands in a box that looks healthy and does nothing, **and `check` passed** | `gc status`, session events, gc pin vs the fix that changed the claim gate | Second-line hypothesis only — rule out the first row above before spending time here. The identities the allocation sends must be a superset of the identities the claim gate matches, and a shipped-but-unpinned fix is not in effect. To test it you need a gascity checkout and both shas: `git -C <gascity-checkout> merge-base --is-ancestor <fix-sha> <pin-sha>` (exit 0 = the fix IS in the pin) |
| The agent inside fails closed on its target | staged `env.sh`, the provider env | The city target must be named exactly once: a complete remote context and a URL together is the fail-closed case, and the local-target family is stripped before launch by design |
| Session logs missing | `check` warning, `GC_NOMAD_LOG_SINK` | Unset, the shipper task is never added at all; disabling shipping is meant to be visible, not silent |
| `evidence_lost` on a stop, or no egress at all | sidecar tombstone, `GC_NOMAD_EGRESS_DIR` | Stop order is agent stop, shipper flush, egress, deregister; up to three egress retries then an explicit loss marker |
| A call you expected to work is silently a no-op | which op the caller actually named | Exit 2 on an unimplemented op is RPP forward compatibility, not a failure |
| A warning that a key is not a boolean, or a duration will not parse | that key's value | Bad config values are errors, and the operation stops naming the variable rather than defaulting |

## 2. Collect evidence with native verbs, in this order

Pack side, FIRST - the pack's own diagnostics answer its contract without
touching a cluster, and when the fault IS the pack or the `gc` driving it, the
city-side verbs below are ten commands spent interrogating a broken instrument:

```bash
gc-runtime-nomad check                 # RUN THIS FIRST. See below - it can FAIL, not just warn
gc-runtime-nomad protocol              # the advertised RPP version and capabilities
gc-runtime-nomad is-running <session>  # and: list-running [prefix]
```

`check` is not advisory. It **warns** on an unset log sink, an unset required
image and an unset resolver list — each a real fault on its own — and it
**FAILS, exiting nonzero**, when the `gc` on PATH cannot claim work from inside
an allocation. That FAIL is the single most decisive line this pack produces
and it supersedes every warning above it. Attach the whole output to any bug
report.

City side, SECOND - these are the record, and they are read-only alongside a live
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

Cluster side, read-only, correlated by child job and allocation id:
`nomad job status`, `nomad job allocations`, `nomad alloc status`,
`nomad alloc logs`, `nomad evaluation status`, `nomad node status`.

**The sidecar binding is the only layer that can tell two faults apart.** Law 3
names it as one of the five signals; this is what is in it. Under
`GC_NOMAD_SIDECAR_DIR`, per session, it holds the child job id, whether the
agent was launched, the pane pid, and whether staging is pending. Every other
layer is inferring:

- `stage_pending` set with no `launched` → **provisioned but never launched.**
  The allocation can be running and healthy; nothing was ever started in it.
- `launched` set with a dead `agent_pid` → **launched, then the agent died**
  inside a live allocation. `ClientStatus` will never show this, because the
  box is fine.

`is-running` answers `false` for BOTH, which is why reading the binding is the
step that splits them.

**`diagnostics.log` lives in that same directory, and it is the first thing to
read after a failed op.** gc's exec transport turns any nonzero op exit into
`tmux exited N` and discards the pack's stdout and stderr, so the cause it
printed never reaches you — a bare exit code is all that survives. The pack
therefore appends its own account there: one line per event, stamped and
attributed to the operation that produced it, with `FAILED: <cause>` for an op
that failed. When an error reads `tmux exited 1:` with nothing after the colon,
that file is where the missing half went. Note also that a deleted binding strands a child job:
`stop` has no orphan reaper, so a scratch sidecar directory removed by hand
leaves the dispatch behind.

Write configuration down as **names and pointers, never values**: key names, the
parent fingerprint, pack and Nomad versions, the fact that a TLS or custody
check passed, and custody-file paths. Allocation and child-job ids only when
they correlate two observations. A token value in a transcript is an incident,
not evidence.

### Reading the binding — the part every decisive claim above depends on

Three things make this unreadable if you guess, and all three have bitten
someone already:

1. **You need `GC_NOMAD_SIDECAR_DIR`, and you cannot read it from your own
   shell** — the pack takes it from the SUPERVISOR's environment. Do not go
   hunting in the process table. Ask the pack:

   ```
   gc-runtime-nomad check
   ```

   It prints `diagnostics: <dir>/diagnostics.log`. That directory IS the
   sidecar dir, and that file is this pack's own durable account of every
   failure and every dark-lane warning — read it before anything else.

2. **The filename is base64url of the session name, with `=` padding
   stripped, plus `.json`.** An `ls` shows opaque names and tells you nothing:

   ```
   SESSION=worker-local-1-pool
   DIR=<the dir check printed>
   F="$DIR/$(printf %s "$SESSION" | basenc --base64url | tr -d '=').json"
   # macOS without basenc: python3 -c 'import base64,sys;print(base64.urlsafe_b64encode(sys.argv[1].encode()).decode().rstrip("="))' "$SESSION"
   cat "$F"
   ```

3. **The on-disk keys are snake_case**, not the field names above:
   `child_job_id`, `launched`, `agent_pid`, `stage_pending`. Grepping for
   a CamelCase field name returns nothing and reads exactly like "the bindings are empty" —
   which is the wrong conclusion, reached twice.

   No binding file at all is itself an answer: the session was never
   provisioned under this sidecar dir, or it was stopped and tombstoned (look
   in `tombstones/`).

**Reaching inside a box.** Several rows above say to run `ps` or
`tmux has-session` in the allocation. Use the pack's own verb, which resolves
the session's current allocation for you and needs no alloc id:

```
printf 'ps -eo pid,comm' | gc-runtime-nomad exec <session>
gc-runtime-nomad peek <session> [lines]
```

**The command rides on STDIN, not argv.** `exec <session> ps -eo pid,comm`
does not work: everything after the session name is ignored, and on a
terminal it simply blocks waiting for stdin — which reads exactly like an
unreachable box, and would send you to escalate on a false premise.

`exec`'s exit code is the remote command's own, not a lifecycle 0/1/2. It
streams the box's stdout verbatim, so never run a command there that prints
the environment.

`peek` is useless once the tmux server is gone — if the pane died, the server
died with it, and there is no scrollback left to read. That is the case where
`diagnostics.log` is the only remaining evidence.

## 3. Bounded checks

Run the narrowest check that can distinguish your two remaining hypotheses, and
stop when one of them dies.

1. **Hermetic, first among CHECKS — but after `check`.**
   `GC_BIN=$(command -v gc) ./conformance.sh` builds the executable and an
   in-memory fake Nomad API and runs the full RPP lifecycle with no cluster and
   no network. It is the pack's CI gate. A defect that reproduces here needs no
   cluster to fix.

   **A green conformance run is NOT evidence the lane can claim.** conformance.sh
   exports `GC_NOMAD_SKIP_GC_CAPABILITY_GATE=1` by design — the ladder drives
   the RPP surface against a fake with no city and nothing to claim, so the
   gate's question is irrelevant there. The consequence is the trap: run this
   ladder on a deployment whose `gc` cannot claim a single bead and it goes
   green. It answers "does the pack implement the protocol", never "can this
   deployment do work". `gc-runtime-nomad check` is the one that answers the
   second question, which is why §1 says to run it before anything.
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

   **The safe spelling, because §9 forbids every obvious one.** Test PRESENCE,
   never value — `${VAR+x}` is set-or-not without expanding the value, so it
   cannot print a token even by accident:

   ```bash
   #!/bin/bash
   # bash: ${!v} is indirect expansion. In zsh the spelling is ${(P)v+x} —
   # ${!v+x} is not portable between them and silently reports garbage in the
   # wrong shell, which is worse than not probing at all.
   for v in GC_NOMAD_AGENT_IMAGE GC_NOMAD_LOG_SINK GC_NOMAD_DNS_SERVERS \
            GC_NOMAD_SKIP_GC_CAPABILITY_GATE NOMAD_SKIP_VERIFY; do
     [ -n "${!v+x}" ] && echo "$v set" || echo "$v unset"
   done
   ```

   **And whose environment?** `check` and this loop read the environment of
   whoever RUNS them. The pack reads the SUPERVISOR's. Run these as the
   supervisor, or treat their warnings as facts about your shell — an unset
   `GC_NOMAD_AGENT_IMAGE` in your terminal says nothing about a deployment
   whose boxes are demonstrably running.

   Almost every key in the table is diagnosable from set-vs-unset alone. Where
   a value genuinely matters (an image tag, a sink URL), read it from the
   deployment record rather than from a live process environment. Two entries in that environment are worth
   naming here because neither is a documented key:
   - `NOMAD_SKIP_VERIFY` is read by the Nomad SDK, not by this pack, and it
     disables certificate verification for the API **and** for the alloc-exec
     channel every staged secret travels over. The runtime warns on it at
     every client construction and in `check`.
   - `GC_NOMAD_SKIP_GC_CAPABILITY_GATE` disables law 5's refusal. It exists for
     the offline ladder. Set in a real deployment it restores the dark lane,
     and the runtime says so on every op.
4. **Placement is two fatal rules, not one.** The parent job constrains
   `${attr.kernel.name} = linux` AND `${attr.cpu.arch} = amd64`. Both are
   deliberate: bridge/CNI is Linux-only, and everything this pack can run is
   amd64 by construction — the image build refuses a non-x86-64 `gc`, and the
   log-shipper's binary is pinned to x86_64. A cluster whose only free clients
   are arm64 places nothing and reports a scheduler outcome, not a pack error.
   Before the arch rule existed, such a client placed the group and ran an
   agent whose logs silently never shipped.
5. **Read-only cluster inspection** is bounded by default: status, logs,
   evaluations, node state. Nothing here mutates the estate.
6. **A live fault drill is not a debugging step.** It is a gated activity with
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
