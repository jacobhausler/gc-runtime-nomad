module github.com/gastownhall/gc-runtime-nomad/drills

go 1.25.9

// fakenomad is this pack's in-memory Nomad API fake (NRT-P1-02), used only
// by _test.go files so the L4 harness stays offline-testable without a live
// Nomad cluster (NRT-P2-06a) — mirrors ../runtime/go.mod's same test-only
// require, keeping the drills module's own production code
// zero-external-Go-dependencies too.
require github.com/gastownhall/gc-runtime-nomad/fakenomad v0.0.0

replace github.com/gastownhall/gc-runtime-nomad/fakenomad => ../fakenomad
