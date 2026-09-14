# The typed-close bridge — what a box can and cannot do over the remote API

A box session run by this pack reaches the city over `gc`'s remote worker API
(`/v0/city/{city}/worker/*`, [PR #6146](https://github.com/gastownhall/gascity/pull/6146))
rather than a local store — it has no store to open. This note states, once,
which of a session's own typed operations that bridge actually carries, so a
deployment does not have to rediscover the gap from a box that closed wrong.

**BRIDGE — not implemented in this pack.** The bridge is native `gc`/API
surface, not something `gc-runtime-nomad` builds or can build; this pack has
no seam to add one behind. That is unchanged by anything below.

## What is served remotely today

- **Claim, heartbeat, release, plain close** — served. A box-side `gc`
  invocation goes through the same HTTP handler path a local store call would
  take.
- **Typed close via metadata + close** — served. The atomic form is
  `gc bd close <id> --set-metadata gc.work_outcome=<outcome> [--set-metadata
  gc.work_commit=<sha> --set-metadata gc.work_branch=<branch>] --reason ...`:
  one call stamps the typed outcome fields as bead metadata and closes in the
  same request.
- **The remote worker close handler now enforces the ADR-0009 ancestry
  gate.** As of upstream follow-up commit `e156d730c` on PR #6146, a remote
  typed close naming `shipped` with a commit is checked against the
  bead-owned checkout the same way a local close already was — an
  unreachable commit is refused (`409`) under `GC_WORK_RECORD_ENFORCE`, and a
  resolved bead with no hosted checkout closes with an explicit "reachability
  unverified" log rather than a silent pass. Before that follow-up, the
  remote path stamped and closed without ever calling the shared ancestry
  predicate the local gate always ran — a real gap, not a documented
  deferral.

## What is NOT served remotely today

- **`--outcome` on `gc bd close`.** The single-flag typed-close form a local
  seat can use does not have a remote equivalent; a box must use the
  metadata + close form above instead. Do not script a box session against
  `--outcome` and expect it to reach the remote handler — it will not.
- **Escalation creation.** A box cannot open an escalation against the city
  through the remote worker surface. A session that needs to escalate has to
  hand that off to a host-side step (description, metadata, or a receipt
  convention the deployment defines) for a lane with real write access to
  file it.
- **Some metadata writes.** The remote surface accepts the metadata keys the
  close path above needs; it does not carry every `gc bd update
  --set-metadata` shape a local caller has. Treat an untried metadata key as
  unserved until it is proven against the pinned `gc`, not as "probably
  fine" — the failure mode (a write that silently lands nowhere, or is
  rejected late) looks identical to a working call until read back.

## What a session run by this pack should do

End every atom with the metadata + close form, not `--outcome`. When a
session needs to escalate, or needs a metadata write this list has not
proven, route that need to a host-side step rather than assuming the remote
API will carry it — the bridge for those paths does not exist in this pack
and is not implied by anything the city-checkout knob
(`GC_NOMAD_CITY_MIRROR_URL`, see `COMPATIBILITY.md`) added.

## Keeping this current

This list is a function of the `gc` pin, not of this pack's own code — a pin
roll that lands more of the remote worker surface (or regresses it) changes
the "served" column above without a single line in this repository changing.
Re-verify against the live pin's `gc version --json` commit and
`docs/PIN-DEPENDENCIES.md`'s remote-worker family rows before trusting this
file across a pin roll.
