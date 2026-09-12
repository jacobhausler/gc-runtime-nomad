# Pin dependencies — the gc EDGE families this pack needs in the city pin

This pack ships an executable that gc drives, and it is driven by a **city pin**:
the pinned `gc` binary on the host *and* the `gc` inside every Nomad alloc. Several
behaviours the pack relies on do not exist upstream yet — they live in Westlands'
local EDGE lineage and ride the pin as `pin-roll/candidate-20260907` commits. If the
pin rolls onto plain upstream `main`, those behaviours vanish and the pack breaks in
ways that look like pack bugs.

This file is the checklist: for each family, the upstream twin, its live merge
state, what the runtime loses without it, and the first pin that carried it.

**But start with the tiers below.** Carrying a family is not the same as
requiring it, and reading this table as a flat requirement list overstates the
blocking set by roughly four to one. The measured blocking set is **two filed
PRs plus four unfiled follow-ups that all stack on one of them**.

**Pin line** (verified, not remembered — `git merge-base --is-ancestor <sha> <pin>`
in the maintainer's pin-candidate worktree, HEAD `857ed398a` — these shas are
in the private lineage and will not resolve against `gastownhall/gascity`):

```
6ba434271 (upstream base)
  └─ 3c28c0430 ─ 60543e0db ─ e6c792597 ─ 400a7af53 ─ f75c33003 ─ 1afd6736a ─ 857ed398a
```

`857ed398a` is the live pin since 2026-09-08T19:25Z
(`ops/receipts/edge/pin-roll-20260907/swap-receipt-20260908T1925Z.md`).

**Merge states** were read live on 2026-09-08T21:52Z with
`gh pr view <n> --repo gastownhall/gascity --json state,mergedAt`. **Every PR
referenced below is `OPEN` with `mergedAt: null` and `isDraft: false`.** Only
#5629 and #5735 report `mergeable=CONFLICTING`; the rest report `MERGEABLE`.
Nothing on this list can be retired on "merged" grounds today.

## What actually blocks a stranger — read this before the table

The table below lists every family **this estate's pin carries**. That is not
the same question as **what this pack requires**, and conflating the two made
the dependency look four times larger than it is. The distinction matters
because it changes the upstream ask from "nineteen families, seven unfiled"
into something a person could actually finish.

The pack itself imports **zero** gascity code — its only production dependency
is `hashicorp/nomad/api`. So it cannot depend on gc internals. What depends on
them is *a seat running inside an allocation, doing gc work*. Sorting by that
question, measured against a worktree at `6ba434271` (the upstream base):

### Tier 1 — the pack genuinely cannot function without these

A seat in a box cannot claim, stamp or close work. This is the real blocker.

| family | upstream |
|---|---|
| `remote-worker` / `remote-worker-lifecycle` | [#6146](https://github.com/gastownhall/gascity/pull/6146) |
| `remote-worker-env-city-target` | [#5864](https://github.com/gastownhall/gascity/pull/5864) — filed, MERGEABLE |
| `remote-step-accept-list` | **unfiled** — stacks on #6146 |
| `remote-worker-session-identity` | **unfiled** — stacks on #6146 |
| `remote-worker-claim-dispatch` | **unfiled** — stacks on #6146 |
| `policy-conditional-writer-forwarding` | **unfiled** — stacks on #6146 |

**That is the whole blocking set: two filed PRs, plus four follow-ups that all
stack on one of them.** Not nineteen families.

### Tier 2 — required only for particular usage patterns

Real dependencies *if you use the pattern*, and invisible if you do not. A
deployment routing plain task beads to a pool hits none of them.

- `workrecord-step-gate` ([#6133](https://github.com/gastownhall/gascity/pull/6133)) — only if you close **workflow steps**. The estate runs every unit of work as a `mol-do-work` step; a plain task bead does not take this path.
- `remote-template-route` (unfiled; issue [#5737](https://github.com/gastownhall/gascity/issues/5737)) — only if work is routed to a **pool template** rather than to a seat.
- `tmux-hooks-claim` ([#5599](https://github.com/gastownhall/gascity/pull/5599)) — `gc hook --claim` under tmux. Not box-specific in mechanism: a worker-local seat runs under tmux and makes the same call, so an upstream deployment gets upstream's behaviour on **every** lane, not just this one.
- `beads-v65-5518` ([#5518](https://github.com/gastownhall/gascity/pull/5518)) — a `bd` schema version contract between the city and the `bd` baked into your agent image. Match them and it is moot.

### Tier 3 — not pack dependencies at all

These change pool, dispatch and routing behaviour **identically for a local
lane and a Nomad lane**. Running upstream gc means running upstream's pool
behaviour everywhere; that is an upstream question, not something this pack
requires. Listed so nobody re-derives them as blockers:

`graphroute-one-shot-independence`, `pool-oneshot-asleep-holder-reuse`,
`pool-stranded-holder`, `seat-identity`, `builtinpacks-pack-validation-memo`.

`dispatch-retry-routing` sits here **for its named behaviour only**. Its
commit `b35a078be` also introduces the session current-claim pointer that a
Tier 1 row's commit leans on — see *Upstreaming the blocking four*. Port that
Tier 1 row without the pointer and this row stays a Tier 3 row; port it
verbatim and you have pulled a prerequisite in behind it.

### Tier 4 — performance, or this estate's own features

- `sessionlog-pi-transcript-header-cache` ([#6089](https://github.com/gastownhall/gascity/pull/6089)) — the row's own description: *"it shows up as a slow session/dashboard log surface, not as a failure."*
- `workquery-prefetch` ([#5629](https://github.com/gastownhall/gascity/pull/5629)) — its own description: *"the work query takes the un-prefetched path — dispatch latency."*
- `agent-groups` — group-qualified routing. An estate routing feature; a deployment that declares no `[[agent_groups]]` never exercises it.
- `runtime-provider` — **not required to select this pack.** See the correction below.

### Upstreaming the blocking four — measured, not estimated

Each of the four unfiled Tier 1 rows is exactly one commit in the pin
lineage, and all four were test-applied onto **#6146's head (`9c23324fe`)**
on 2026-09-12:

| family | commit | onto #6146 |
|---|---|---|
| `remote-worker-claim-dispatch` | `7ea324ce38` | conflicts — 1 hunk, `internal/api/handler_worker.go` + `internal/beads/worker_ops.go` |
| `remote-step-accept-list` | `c4ba4511d0` | conflicts — `cmd/gc/remote_worker.go` (+test), `internal/api/client_worker.go` |
| `remote-worker-session-identity` | `1afd6736a2` | conflicts — `internal/api/handler_worker_test.go` only |
| `policy-conditional-writer-forwarding` | `857ed398ad` | conflicts — `cmd/gc/bead_policy_store.go` only |

**None cherry-picks cleanly, including in dependency order.** Anyone planning
this work should budget a port, not four `git cherry-pick`s.

**Three of the four are now ported, built and tested** against `#6146`'s head
(2026-09-12), each with a red proof, `gofmt`, `go vet`, and the full
`./cmd/gc`, `./internal/api` and `./internal/beads/...` suites green:

| family | port | what it needed beyond the commit |
|---|---|---|
| `remote-worker-claim-dispatch` | done | `internal/beads/worker_ops.go` (104 lines) — `beads.ClaimFor` does not exist at `#6146` at all. No session-pointer machinery required; that entanglement drops out when only the stated half is ported. |
| `policy-conditional-writer-forwarding` | done | nothing; `ClaimAs`/`Comment` forwarding included so the capability set stays coherent |
| `remote-worker-session-identity` | done | an OpenAPI regen — the field's doc string feeds the checked-in spec |

**`remote-step-accept-list` cannot be a follow-up on `#6146` at all.** Its
wire half is *already there* — `BeadUpdateBody` carries `Metadata` at
`#6146`'s head. Its CLI half edits `cmd/gc/remote_worker.go`, and **that file
does not exist in `#6146`**: no `remoteBdFlagArity`, no `remoteBdUpdate`, no
`UpdateBeadOpts`, no remote `gc bd` CLI leg of any kind. That leg belongs to
the broad remote-worker family commit `421c99547d`, which `#6146` only
partially carries. So this row stacks on a *larger* upstream contribution, not
on `#6146` — which corrects the "four follow-ups stack on #6146" framing
above: three do, one does not.

**Why they conflict — and the hidden prerequisite.** The estate's
`7ea324ce38` does two separable things at once. Its *stated* purpose is to
route the remote claim through `beads.ClaimFor` and the policy
`ConditionalWriter` instead of a direct assert. But the same hunk also
reserves a **session current-claim pointer** (`workerSessionFront`,
`workerExpectedClaimPointer`, `ReserveCurrentClaim`, `ClearCurrentClaim`).
Measured at `9c23324fe`: **every one of those symbols occurs zero times.**
They enter the lineage in `b35a078be` — the `dispatch-retry-routing` family,
which this file tiers as *not* a pack dependency.

So the table had a gap: a Tier 1 row carried an undeclared dependency on a
Tier 3 row. Recorded rather than quietly fixed, because it is the shape to
watch for in the rest of this list — a family's name describes its purpose,
not necessarily everything its commit touches.

**The good news is that the two halves separate.** #6146's head still
direct-asserts `workerAssignmentClaimer` and calls `claimer.Claim(id,
assignee)` at `internal/api/handler_worker.go:120-128`, naming no
`beads.ClaimFor` — exactly as this file has always said. A minimal upstream
port replaces that call with `beads.ClaimFor` plus the policy writer and
**leaves the session-pointer machinery behind**, which keeps the PR to its
stated subject and drops the prerequisite entirely.

### Correction: `runtime-provider` is not a blocker

This file previously said a pin without the `runtime_provider` seam **drops
`[runtimes.nomad]` from config**, so "the runtime disappears instead of
failing loudly." Measured at `6ba434271`, that is wrong in a way that matters:

- The pack-runtime registry is **fully present upstream**. `internal/config/
  pack.go` carries `Runtimes map[string]PackRuntimeEntry` and
  `mergeCityRuntimes`, byte-identical to the pin, and `cfg.Runtimes` is read
  upstream by `gc doctor`, `gc runtime check` and `runtime_registry.go`.
- Upstream selection already supports pack runtimes explicitly.
  `newSessionProviderForCityByName` documents `"<pack runtime>" → exec proxy
  bound to the pack's declared command`, resolved through `WorkerSpec.Runtime`
  by **session provider name** — which is what this pack's own `pack.toml`
  has always said: *gc resolves `session = "nomad"` to this executable through
  the per-city runtime registry.*

What upstream lacks is the per-agent `runtime_provider` TOML key (zero
occurrences at base; present in `config.go`, `patch.go` and the agent struct
at the pin). That key exists to **separate the runtime axis from the provider
axis** — a better design, and one upstream's own comments say is mid-flight
("the single place the Transport/Upstream axes will be honored as the
de-conflation completes"). But a deployment willing to write `session =
"nomad"` selects this pack on stock gc today. The seam is an improvement to
upstream, not a precondition for this pack.

## Every family the pin carries

| family | upstream PR | state | why the runtime needs it | first pin carrying it |
|---|---|---|---|---|
| `remote-worker-lifecycle` | [#6146](https://github.com/gastownhall/gascity/pull/6146) | OPEN, `mergedAt=null`, head `9c23324fe`, MERGEABLE | The `/v0/city/{city}/worker/*` verbs and the off-host CLI path this pack drives. A seat inside an alloc claims, heartbeats and closes through the API handler instead of a local store. Without it a remote-context seat cannot claim at all — the D3 windows drained `{"action":"drain","reason":"no_work"}` while the city hook listed their steps as ready. | `e6c792597` (repin #3a, swap `20260908T0626Z`) |
| `seat-identity` | [#6143](https://github.com/gastownhall/gascity/pull/6143) · issue [#5716](https://github.com/gastownhall/gascity/issues/5716) | both OPEN, `mergedAt=null` | `cmd/gc/bd_seat_identity.go` — a `bd` claim is attributed to the seat holding it, not to the pool template. Without it the dashboard cannot say who holds a bead and a seat can act under the wrong identity (the "Codex seat that silently ran Qwen" class in `RELEASE-NOTES.md`). Fixups `0178401ec`, `400a7af53` ride the same PR. | `400a7af53` (family commit `5c73b07e7`) |
| `workrecord-step-gate` (formula step-close exemption) | [#6133](https://github.com/gastownhall/gascity/pull/6133) · issue [#5764](https://github.com/gastownhall/gascity/issues/5764) | both OPEN, `mergedAt=null`, MERGEABLE | The ADR-0009 work-record gate must exempt workflow steps (which close with `gc.outcome` alone) from the task vocabulary (`gc.work_outcome`). Every `mol-do-work` step this pack's convoys run is a step close. Without the exemption the gate refuses the step close outright, the workflow records `missing_outcome`, and the same work is re-run forever. Exercised on every run of this pack — see `ops/receipts/edge/pin-roll-20260907/a6-step-seat-arm-20260908.md` arm A6a. | `60543e0db` (swap `20260908T0231Z`) |
| `remote-template-route` (remote template identity) | **not yet filed upstream** — no PR of its own; rides #6146's surface; issue [#5737](https://github.com/gastownhall/gascity/issues/5737) | #6146 OPEN, #5737 OPEN | A template-routed claim has to resolve the canonical rig/seat identity rather than the template it was routed through; `3dd40d411` is the test that pins that. Without it a template-routed remote claim lands on the template and the real seat never owns its work. | `400a7af53` (`d387ac3f8`, `3dd40d411`) |
| `tmux-hooks-claim` | [#5599](https://github.com/gastownhall/gascity/pull/5599) (carrier, head `697cc6a02`) · held twin issue [#5738](https://github.com/gastownhall/gascity/issues/5738) | both OPEN, `mergedAt=null`, MERGEABLE | `gc hook --claim` behaviour under tmux — the first call every seat of this pack makes, from inside an alloc the operator cannot watch. Row 3 (`e4f65a6b4`) carries the held-twin expiry `2026-10-05`. Without it a claim strands, or claims the wrong session — the silent-40-minute strand class (gastownhall/gascity#5693). | `400a7af53` (`266025300`, `e4f65a6b4`) |
| `remote-worker-claim-dispatch` (ClaimFor dispatch / policy writer) | **not yet filed upstream** — not carried by #6146; the PR head still direct-asserts `workerAssignmentClaimer` (`internal/api/handler_worker.go:120`) and names no `beads.ClaimFor`; **no PR of its own — stack on #6146** | #6146 OPEN | The remote claim must go through `beads.ClaimFor` and the policy `ConditionalWriter` rather than a direct assert, so it is subject to the same conditional fence as a local claim. Without it the policy writer is bypassed on the remote claim path, and the fence the rest of this list depends on is not on the write path a Nomad seat actually takes. The `cmd/gc/bead_policy_store.go` half of this family is `policy-conditional-writer-forwarding`, below. | `f75c33003` (`7ea324ce3`, repin #5, swap `20260908T1429Z`) |
| `remote-step-accept-list` | **not yet filed upstream** — no PR of its own; a content follow-up on #6146, which adds `--set-metadata` for `gc bd close` only and never the `remoteBdFlagArity["update"]` widening; #6146's remote-bd surface is `cmd/gc/remote_worker_lifecycle.go` while this row edits `cmd/gc/remote_worker.go` | #6146 OPEN | A box-side `gc bd update --set-metadata` has to be accepted at all. That is how a step inside an alloc stamps its own `gc.outcome` before closing. Without it the flag is rejected on the remote path, the step close loses its outcome stamp, and the terminal bookkeeping the city depends on cannot be written from inside the alloc — which reads as `missing_outcome` and re-runs the work. | `f75c33003` — itself a cherry-pick of `c4ba4511d`, which is **not** an ancestor of the pin line |
| `remote-worker-session-identity` | **not yet filed upstream** — not carried by #6146, no PR of its own; stack on #6146 (the PR owns `internal/api/handler_worker.go` but not this row) | #6146 OPEN | The hook claim stamps `gc.session_id` *inside* the revision-fenced lease write. #6146's `stampWorkerLease` takes no session id and writes only `gc.lease_owner` plus write-once `gc.claimed_at`, while its own `workerOwnership` close check **reads** `gc.session_id`/`gc.session_name` — it reads an identity its claim never stamps. Without this row, a typed close of a hook-claimed `cr` bead from inside a box returns `409 no gc.session_id stamped`. | `1afd6736a` (repin #6) |
| `policy-conditional-writer-forwarding` | **not yet filed upstream** — no PR; stack on #6146, whose 21 paths do not include `cmd/gc/bead_policy_store.go`, the file this row teaches `beads.ConditionalWriterHandleProvider` | #6146 OPEN | Forwards the policy-wrapped `ConditionalWriter` to the remote worker verbs so remote `claim`/`heartbeat` are fenced (compare-and-set) on the `cr` work class. Without it those writes are unfenced: two seats can each believe they hold the same bead, and the lost update is silent. The accepting review rejected an unfenced assignee-plus-session fallback as an alternative. | `857ed398a` (repin #7 — **the live pin**) |
| `graphroute-one-shot-independence` | [#5584](https://github.com/gastownhall/gascity/pull/5584) + [#5586](https://github.com/gastownhall/gascity/pull/5586) (consume #5584 before #5586); upstream #5622/#5697 are MERGED but replaced the mechanism this row re-ports | both OPEN, `mergedAt=null`, MERGEABLE | One-shot graph routing stays independent of pool continuation, so a single dispatched step does not inherit a pool's continuation path. Without it a `one_shot` seat can be routed as though it were a pool member. | `3c28c0430` (`5235dccee`) |
| `pool-oneshot-asleep-holder-reuse` | [#5594](https://github.com/gastownhall/gascity/pull/5594) | OPEN, `mergedAt=null`, MERGEABLE | A `one_shot` seat may be reused when its holder is merely asleep, instead of being treated as a live holder blocking reuse. Without it stranded holders hold pool slots and dispatch quietly stops making progress. | `3c28c0430` (`d04ef488d`) |
| `pool-stranded-holder` | [#5735](https://github.com/gastownhall/gascity/pull/5735) | OPEN, `mergedAt=null`, **`mergeable=CONFLICTING`** | Stranded-holder release: drain-ack markers, the asleep-gated marker lifecycle, and store-ref-qualified release (a holder must not release another store's claim). Without it a claim held by a seat that is gone stays invisible forever — the unrecoverable-`in_progress` class this estate's worker charter warns about. Three rows: `0fedcdc11`, `8553e69a3`, `94561dca1`. | `3c28c0430` (`0fedcdc11`) |
| `builtinpacks-pack-validation-memo` | [#5592](https://github.com/gastownhall/gascity/pull/5592) | OPEN, `mergedAt=null`, MERGEABLE | Pack-validation memo on the builtin-packs path — the surface a runtime pack like this one is loaded through. | `3c28c0430` (`e14612e0e`) |
| `remote-worker-env-city-target` | [#5864](https://github.com/gastownhall/gascity/pull/5864) | OPEN, `mergedAt=null`, MERGEABLE | Env/city-target resolution for a remote worker session (`internal/api/session_runtime.go`). This is the "every `gc` call in the box exits 1 with `conflicting targets`" class — a remote context must not be handed the supervisor's local city env. The pack halves of that fix are `5466530` / `1ecfc50`. | `3c28c0430` (`0a801ba93`) |
| `sessionlog-pi-transcript-header-cache` | [#6089](https://github.com/gastownhall/gascity/pull/6089) (adopted row, by atbrace); stacked follow-up [#6137](https://github.com/gastownhall/gascity/pull/6137) is a follow-up, not a second copy | both OPEN, `mergedAt=null`, MERGEABLE | Session-log transcript header cache — a performance row. Without it session-log reads re-stat and re-parse headers per read; it shows up as a slow session/dashboard log surface, not as a failure. | `3c28c0430` (`07d5b33a6`) |
| `beads-v65-5518` | [#5518](https://github.com/gastownhall/gascity/pull/5518) — wider than the local row: the pin imports the `go.mod`/`go.sum`/v65-guard subset, not `deps.env`, the k8s agent Dockerfile, or the two `scripts/` files | OPEN, `mergedAt=null`, MERGEABLE | beads schema v65 bump. The beads version pin is a hard contract: `TestBDVersionPins` and `TestAgentImageRebuildsBDAndGCWithPatchedGRPC` go red on the subset alone, so a pin carrying the bump without the PR's full surface is measurably incomplete. Paired with promotion commit `aa47d2d0e` (beads + grpc contract), which is a pin-roll promotion and not an EDGE row. | `3c28c0430` (`80d72c43f`) |
| `workquery-prefetch` | [#5629](https://github.com/gastownhall/gascity/pull/5629) (executive-owned); the family twin was **not filed — duplicate** of open issue [#5712](https://github.com/gastownhall/gascity/issues/5712) | OPEN, `mergedAt=null`, **`mergeable=CONFLICTING`** | Priority-first routed claim prefetch, plus its config goldens and graph-anchor ordering (`1ea0b919c`, `a4e0f5b2d`, `3a3acc5c3`). Without it the work query takes the un-prefetched path — dispatch latency — and with the goldens absent the config tiers are unguarded against drift. Family expiry `2026-11-01`, or #5629 merge. | `3c28c0430` (`1ea0b919c`) |
| `runtime-provider` | **not yet filed upstream** — no PR; twin drafted and held (`edge-families/twins/ef180e84e.md`) | not filed upstream | The `runtime_provider` config seam. C6 measured that a pin without it **drops `runtime_provider` and `[[agent_groups]]` from config** — the pack's own `[runtimes.nomad]` declaration path is silently lost, so the runtime disappears instead of failing loudly. Expiry `2026-10-05`. | `3c28c0430` (`b955d3fb3`) |
| `agent-groups` | **not yet filed upstream** — no PR; twin at family level only (`edge-families/twins/132e244f1.md`) | not filed upstream | `[[agent_groups]]`: the config + pure policy package, the leg that makes it act on a route, and its eligibility acceptance floor — one decision in three rows. Without them, group-qualified routing is not enforced. Expiry `2026-10-05`. | `3c28c0430` (`eb207076b`) |
| `remote-worker` (16-patch re-port) | [#6146](https://github.com/gastownhall/gascity/pull/6146) is the family twin; the drafted twin `twins/524e00ae9.md` and the mTLS row `42af1bc27` carry no FILED marker | #6146 OPEN | The broad off-host worker leg (`/v0/city/{city}/worker/*` + CLI) that `remote-worker-lifecycle` and the four `cr-gdeav.5.10*` follow-ups sit on top of. 12 of the 16 rows stand or fall *with the family*, so the pin must carry the family whole, not a subset. Expiry `2026-10-05` or #6146 merge. | `3c28c0430` (`421c99547`) |
| `dispatch-retry-routing` | [#5609](https://github.com/gastownhall/gascity/pull/5609) | OPEN, `mergedAt=null`, MERGEABLE | Retry routing for dispatch: a failed dispatch is retried to a sane target instead of dropping, or re-striking the same one. | `3c28c0430` (`b35a078be`) |

## Not yet filed upstream

Several rows have **no PR at all**, and one is a duplicate by adjudication. These are
the ones that cannot be tracked from an upstream PR list.

**Four of these block; three do not.** The blocking four are
`policy-conditional-writer-forwarding`, `remote-worker-session-identity`,
`remote-step-accept-list` and `remote-worker-claim-dispatch` — all Tier 1, and
all stacking on #6146. `remote-template-route` is Tier 2 (only for
template-routed work); `runtime-provider` and `agent-groups` are Tier 4 and
are improvements to upstream rather than preconditions for this pack. Filing
effort belongs on the four.

- `policy-conditional-writer-forwarding` — no PR; #6146 does not contain the file it changes.
- `remote-worker-session-identity` — no PR; a content follow-up on #6146's own surface.
- `remote-step-accept-list` — no PR; #6146 covers only part of the flag surface it needs.
- `remote-worker-claim-dispatch` — no PR of its own; #6146 does not carry it.
- `remote-template-route` — no PR of its own (issue #5737 only).
- `runtime-provider`, `agent-groups` — twins drafted and held, never filed.
- `workquery-prefetch` — twin deliberately **not filed**, duplicate of open issue #5712.

The practical consequence: #6146 is a single squashed head carrying only part of the
remote-worker surface, so **merging #6146 does not retire this family group.** The
four follow-ups above still have to land on top of it, or the pin keeps carrying them
locally. Treat "#6146 merged" as a signal to re-read this table, not as a reason to
drop rows.

## Retiring a row

A row retires only when its carrier actually merges (`mergedAt` non-null) **and** the
live pin is rebuilt from a commit containing that merge — then the pin's checks receipt
(C1–C6, SMOKE, HOSTABI, OBSERVED_SUITE) is re-run before the local row is dropped.
Rows with a dated expiry (`2026-10-05`, `2026-10-08`, `2026-11-01`) must be re-read on
that date even if nothing changed upstream. `quarantine-dead-runtime` and
`federation-shadow` are staged review receipts with no candidate code and no upstream
twin asserted — they are not pin dependencies and are excluded from the table.

## Is this still a blocker? — anyone can check

The merge states in the table above were true on the date stated and will
rot. This loop needs only `gh` and read access to the public repository, and
it answers the only question an outside reader actually has: **have the
carriers landed yet?**

```bash
for n in 5518 5584 5586 5592 5594 5599 5609 5629 5735 5864 6089 6133 6137 6143 6146; do
  printf '%s\t' "$n"
  gh pr view "$n" --repo gastownhall/gascity \
    --json state,mergedAt,mergeable,isDraft,headRefOid \
    --jq '[.state,.mergedAt,.mergeable,.isDraft,.headRefOid]|@tsv'
done
```

Read the result against the **Not yet filed upstream** list above before
concluding anything: every one of these merging still would not clear the
dependency, because seven families have no PR in that list at all. In
particular #6146 is a single squashed head carrying only part of the
remote-worker surface, so **#6146 merging does not retire its family group.**

## Re-checking the provenance — maintainer-local

The rest of this file's evidence lives in the maintaining estate's receipt
tree and cannot be re-derived from this repository. Recorded so the trail is
honest about where it ends, not as instructions:

```bash
# the C1 row this table is staged from, and the family landing index
sed -n '1,60p' <receipts>/edge/pin-roll-20260907/repin4-c1-table.md
sed -n '1,90p' <receipts>/edge/pin-roll-20260907/edge-families/INDEX

# which pin first carried a family sha, run in the pin-candidate worktree.
# List the pins literally: zsh does not word-split an unquoted variable, so
# a $PINS loop silently reports nothing.
for p in 3c28c0430 60543e0db e6c792597 400a7af53 f75c33003 1afd6736a 857ed398a; do
  git merge-base --is-ancestor <family-sha> "$p" && { echo "first pin: $p"; break; }
done
```

Sources (maintainer-local): `ops/receipts/edge/pin-roll-20260907/edge-families/INDEX` and the per-family
ledgers, `repin4-c1-table.md` (`cr-zt27m.4.71`), and the C1 rows of
`checks-receipt-400a7af53.md`, `checks-receipt-f75c33003.md`,
`checks-receipt-1afd6736a.md`, `checks-receipt-857ed398a.md`.
