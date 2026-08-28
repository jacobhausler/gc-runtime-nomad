# Runtime implementation reference

This nested Go module builds `gc-runtime-nomad`, the RPP v0 executable described by the [pack README](../README.md). Start at the [front door](../FRONT-DOOR.md) for operator tasks.

## Source map

| Area | Reference |
|---|---|
| RPP argv dispatch and environment | [`main.go`](main.go) |
| Nomad HTTP and alloc-exec transport | [`client.go`](client.go), [`exec_ws.go`](exec_ws.go) |
| Parent jobspec and optional log shipper | [`jobspec.go`](jobspec.go) |
| Session binding and launched marker | [`sidecar.go`](sidecar.go) |
| Workspace and secret staging | [`staging.go`](staging.go) |
| Lifecycle operations, liveness, egress | [`ops.go`](ops.go) |
| Module contract | [`go.mod`](go.mod) |

## Verification map

The focused unit and fault suites are [`main_test.go`](main_test.go), [`jobspec_test.go`](jobspec_test.go), [`ops_test.go`](ops_test.go), [`staging_test.go`](staging_test.go), [`faults_test.go`](faults_test.go), and [`reconcilersim_test.go`](reconcilersim_test.go). The pack-level offline checks are [`../conformance.sh`](../conformance.sh) and [`../receipt.sh`](../receipt.sh); their latest recorded result is [`../CONFORMANCE-RECEIPT.md`](../CONFORMANCE-RECEIPT.md).

Run from this directory:

```bash
go vet ./...
go test -count=1 ./...
```

Keep Nomad credentials out of source, jobspecs, argv, logs, and receipts; use custody-file paths and the environment contract in the [pack README](../README.md#use).
