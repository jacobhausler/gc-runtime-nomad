# v1.0.5

Nine gate-0 remediation atoms from epic `cr-u7b0s` (D3/D4 Nomad dogfood
review), cut on top of `v1.0.4`. One launch-path fix, two "use the native
form" removals, the Docker agent-image seam, the session-toolchain pin, and
a receipt path that fails closed.

## What changed

- **The launch payload survives the seam** (`d786990`, `31b2c4e`, `cr-u7b0s.39`/
  `.40`). gc's start wire is consumed as gc writes it; the pack's private
  start-payload encoding is gone, so a payload gc writes and the pack does not
  know can no longer be silently dropped on the way into the box.
- **`GC_NOMAD_AGENT_IMAGE` — the Docker agent image seam** (`a9eb2e3`,
  `cr-u7b0s.41`). With it set, the allocation carries no toolchain host-volume
  mount and the image owns the toolchain; unset, the legacy exec +
  read-only artifact-volume shape is what runs. One variable, one shape.
- **Native Nomad query and dispatch APIs** (`0b76009`, `cr-u7b0s.42`) in place
  of the hand-rolled HTTP calls, and **the RPP capability surface stays
  native** — `GC_NOMAD_FORBID_REGISTER` now fails closed instead of advertising
  ops the runtime will not perform (`441a36d`, `cr-u7b0s.43`).
- **`toolchain/` — the session toolchain as a checksum-pinned build input**
  (`289f258`, `cr-u7b0s.45`): digest-pinned base, upstream-published sha256 per
  component, `bd` rebuilt from checksum-pinned release source (the published
  binary carries the grpc the release rebuild exists to escape), the **complete**
  Codex standalone package with its layout asserted, `check`/`verify`/`build`
  verbs and a rollback tag. It ships no delivery layer and publishes nothing.
- **Runtime bindings are recovered from Nomad `Meta`** (`d2cc306`,
  `cr-u7b0s.46`), so a session→alloc binding survives the sidecar it used to
  depend on.
- **The conformance receipt fails closed** (`fb15c4b`, `cr-u7b0s.48`).
  `receipt.sh` no longer swallows the ladder's exit — the retry-idempotency
  rung runs *after* gc prints both greppable summaries, so green rows used to be
  publishable over a red ladder; that rung is now a first-class row, every row
  is attributed to the pack commit and the gc that ran it (`gc version --json` +
  the `GC_BIN` digest — gc's own machine-readable report carries no gc
  identity), a run that cannot name its gc fails the provenance row rather than
  emitting unattributed rows, and `--json-stdout` puts the result on stdout for
  a lane that has no artifact-return flag. `receipt-selftest.sh` is the
  distinguishing regression: green summaries, then exit 3.
- **One canonical runtime-contract table** (`96b3ee4`, `cr-u7b0s.49`). The keys
  are the contract; the filled-in values are the deployment's, and they moved
  out of this pack to the estate's own admitted card. Nothing there is a default
  to copy.

## Coverage

The pack's CI gate is the offline ladder — `GC_BIN=$(command -v gc)
./conformance.sh` on every push and PR (`.github/workflows/ci.yml`), and
`GC_BIN=$(command -v gc) ./receipt.sh` for the per-item receipt this file links.
`CONFORMANCE-RECEIPT.md` is **not** regenerated at this commit: since
`cr-u7b0s.48` an at-pin receipt must name the gc that produced it, and no
builder lane carries a Linux `gc` at the estate pin. Regenerating it from a stub
or from a different gc identity is what the fail-closed rule forbids, so this
release states the gap instead of papering over it. `receipt-selftest.sh` passed
both directions on the builders lane (`RECEIPT-SELFTEST-OK`, `cr-u7b0s.48`).

# v1.0.4

Four fixes from the D3/D4 Nomad dogfood windows: a remote-context session in
which every `gc` call failed, a Codex seat that silently ran Qwen, and bridge
allocs with no resolvers (`cr-bo8en.12`, `cr-tzsxk.14.1`, `cr-tzsxk.18`).

## What broke

D3 window 1 provisioned two worker-local seats on rhuidean, primed them, and
watched them drain: `{"action":"drain","reason":"no_work"}` five times, while
the city hook listed their steps as ready. Inside the box, every `gc` call
exited 1 with

    conflicting targets: a remote city (GC_CITY_URL/GC_CITY_CONTEXT) cannot be
    combined with a local city env (GC_CITY/GC_CITY_PATH/GC_CITY_ROOT); unset one

The pack was forwarding the supervisor's own local city identity into a
remote-context alloc, and the pin those windows ran on enforces that
exclusivity where the earlier lineage tolerated it. The launch path was patched
around it in place on the shared volume; these are the pack halves
(`cr-bo8en.12`). Same window, second shape: with an mTLS context staged,
`env.sh` exported BOTH `GC_CITY_URL` and `GC_CITY_CONTEXT`, so gc refused the
session for selecting two remote targets.

D4 lane 3 broke twice more, in ways that read as provider failures and are not.
A "Codex lane" ran Qwen, because gc renders a seat's model and reasoning effort
as *command flags* on the argv the pack's launch seam replaces with the
deployment-owned bootstrap — the flags never reached the box — and
`GC_PROVIDER` carries only the provider *family*, so Qwen and Luna both read
"codex" (`cr-tzsxk.14.1`). And the allocs had no resolvers: a bridge alloc with
no `network.DNS` stanza inherits the *client host's* `/etc/resolv.conf`, which
on a systemd-resolved host is the `127.0.0.53` stub, unreachable from the alloc
netns — `getent hosts api.openai.com` empty, `curl` dead in ~1 ms, IP dials
fine. The same window needed a placement fence (`${node.unique.name} !=
prod-nomad`) after seats landed on a node the estate had retired from its relay
firewall, which until now meant re-registering the parent job by hand — and the
runtime's own next register dropped it (`cr-tzsxk.18`, lane-1 finding 1).

## What changed

- `stripLocalCityStoreEnv` (`runtime/staging.go`) drops the local target/store
  family — `GC_CITY`, `GC_CITY_PATH`, `GC_CITY_ROOT`, `GC_CITY_RUNTIME_DIR`,
  `GC_BEADS*` — from any staged environment that names a remote city
  (`GC_CITY_URL` or `GC_CITY_CONTEXT` present), on the `stage` half and on the
  staged-bootstrap launch half (`buildLaunchCommandForTmux`) — the only
  launch path a real session takes; the legacy bare-tmux placeholder is
  untouched. The remote city name, `BEADS_ACTOR`, and the session identity
  keys stay; they are not local target selectors. A local-only session's
  environment is untouched.
- `remoteContextEnvScriptInput` drops `GC_CITY_URL` from `env.sh` once a
  complete staged context owns it — gc reads the URL from `contexts.toml`
  alongside the mTLS file paths, and exporting both selects two remote targets.
  A deployment without a complete staged context keeps the environment it has.
- `mergeLaunchOptionEnv` recovers gc's rendered provider options from the
  launch command (`--model`/`-m`/`--model=`/`-c model=…`/
  `model_reasoning_effort=`) and exports them into the same `env.sh` the
  deployment launcher sources, as `GC_MODEL` / `GC_REASONING_EFFORT`. An
  explicit provider env entry always beats a value recovered from the command,
  and a value containing anything outside `[A-Za-z0-9._:-]` is dropped rather
  than passed through. This is the pack half; the consumer is the
  deployment-owned `ops/nomad/agent-launch.sh` (`55d9ef4`, `cr-tzsxk.14`) — a
  Qwen seat's config stays byte-identical, a non-Qwen seat gets its model and
  effort with no `model_provider` override, so default OpenAI auth comes from
  the staged `auth.json`. A non-Qwen lane needs a launcher at or after that
  commit.
- Two deployment knobs now render into the parent job the runtime registers
  itself: `GC_NOMAD_DNS_SERVERS` (comma-separated resolver addresses the session
  group's bridge network writes into the alloc's `/etc/resolv.conf`) and
  `GC_NOMAD_NODE_CONSTRAINT` (Nomad-syntax constraints, `;`-separated because
  list operands already use commas inside the value), appended *after* the
  pack's linux-kernel fatal rule so an operator fence can never displace it.
  Both ride the provider-env passthrough that already carries
  `GC_NOMAD_LOG_SINK`/`GC_NOMAD_ARTIFACT_VOLUME`/`GC_NOMAD_NODE_POOL`, and both
  are empty by default: an unset deployment renders the jobspec byte-for-byte as
  before, jobspec hash included — which is also the rollback, unset the
  variable. A malformed value is a config error that fails the op closed, not a
  silently dropped line, and `gc runtime check` now warns when
  `GC_NOMAD_DNS_SERVERS` is unset. Placement lands before the jobspec hash is
  stamped, so a placement change is drift-visible to `ensureParentRegistered`
  instead of being ignored on an already-registered parent (`fnrt-t4l.9`).
  Values stay out of the pack (value-blind), like every other `GC_NOMAD_*` knob.

Still open, and not this pack's: the `501` on `bd update --claim`/
`bd heartbeat` and the `409` typed-close identity gap for remote work-class
beads (`cr-gdeav.5.10` family — D4 lane 1 and its rerun both stopped there),
the reconciler reusing session identity across a runtime re-provision so a seat
sleeps until nudged (D3 finding #2, lane-1 finding 2), and the Codex standalone
package layout that the Luna execution host needs (`cr-tzsxk.20` — an estate
toolchain restage). Staging a seat credential needed no pack change: the
existing `GC_NOMAD_EXTRA_SECRET_KEYS` bridge carried `CODEX_AUTH_JSON`
(`cr-tzsxk.10`, `cr-tzsxk.11`).

## Coverage

Each fix rides its own tests in `runtime`: the conditional strip
(`TestRemoteStageStripsLocalCityStoreEnv`, and
`TestLocalStagePreservesLocalCityStoreEnv` proving the local case is
untouched); the remote-context `env.sh` test re-pointed to assert
`GC_CITY_URL` is *absent* once the staged context owns the URL; the
launch-option recovery (`TestMergeLaunchOptionEnv…` for extraction and the
explicit-env-wins precedence, `TestStageEnvScriptIncludesRenderedLaunchOptions`
that it reaches the staged `env.sh`); and the placement pair —
`TestParentJobSpecDeclaresNoDNSByDefault` /
`TestParentJobSpecCarriesDeploymentDNS`,
`TestParentJobSpecAppendsNodeConstraintsAfterKernelFatalRule`,
`TestJobspecHashChangesWhenPlacementConfigured`, `TestParseDNSServers` /
`TestParseNodeConstraints` over the config-error cases, the `gc runtime check`
warning pair, and the two `newLifecycle` tests that a configured value is read
and a malformed one refuses to start.

The proofs are lived, not simulated — see
`ops/receipts/nrt-d3/2026-09-08-d3-window-1.md` and the three
`2026-09-08-d4-lane*-*.md` receipts: window 2 PASS (two pool seats claimed,
worked and typed-closed city beads through the mTLS relay on pin
`e6c792597`), lane 2 PASS (a Nomad verifier closed a formula step and its
source from the box on `1afd6736a`), lane 3 PASS on the runtime path with the
seat's own TUI footer reading `gpt-5.6-luna xhigh` after the launcher/env
pair landed. The estate-side twins are re-runnable:
`ops/tests/agent-launch-provider-model-test.sh` for the launcher half and
`ops/tests/session-dns-env-staging-test.sh` for the staged DNS keys.

`CONFORMANCE-RECEIPT.md` is regenerated by `./receipt.sh`, and this cut was not
run through it: the script calls `go vet` / `go test` directly, which the estate
runs only on the builders pool. The receipt therefore still stands at `4a08d08`
and this section claims no fresh ladder run at the cut commit — the cut's proofs
are the lived D3/D4 receipts named above, plus the `gc runtime` doctor check and
one Nomad seat provision run against the installed binary.

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
