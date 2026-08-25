# runtime-nomad

A Gas City **runtime pack** scaffold: it ships a runtime *executable*, not a
service. The executable `gc-runtime-nomad` speaks the Runtime Provider
Protocol (RPP v0) protocol handshake. This is the phase-1 scaffold
(`NRT-P1-01`): it proves the pack installs and answers the handshake against
a stub. It does not implement any Nomad API call or any op beyond `protocol`.

## Layout

```
runtime-nomad/
├── pack.toml     # declares [runtimes.nomad] -> gc-runtime-nomad
├── install.sh    # installs the executable onto PATH
└── runtime/      # nested Go module (zero gascity imports)
    ├── go.mod    # module github.com/gastownhall/gc-runtime-nomad
    └── main.go   # RPP protocol-op stub
```

## Use

```toml
# city.toml
[imports.runtime-nomad]
source = "<path-or-registry>/runtime-nomad"

[session]
provider = "nomad"
```

```bash
./install.sh   # put gc-runtime-nomad on PATH
gc doctor      # the pack-runtimes check verifies install + handshake
```

## RPP operations

| Op | Notes |
|----|-------|
| `protocol` | `{"version":0,"capabilities":[]}` |

Every other operation exits 2 — the RPP forward-compatibility signal the
caller treats as a no-op success. This stub intentionally implements no
lifecycle op and makes no Nomad API call.

## Out of scope

Real Nomad API calls and lifecycle op implementations (`start`, `stop`,
`is-running`, ...) are out of scope for this scaffold. They land in a later
phase against `fakenomad` (see `fnrt-s0p`, `fnrt-8yh`).
