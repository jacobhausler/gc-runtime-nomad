module github.com/gastownhall/gc-runtime-nomad

go 1.25.9

// fakenomad is this pack's own in-memory Nomad API fake (NRT-P1-02), used
// only by _test.go files (`gc runtime check` offline lifecycle round-trip,
// NRT-P1-03) — it never reaches the shipped gc-runtime-nomad binary, so the
// pack keeps its zero-external-Go-dependencies contract for production code.
require github.com/gastownhall/gc-runtime-nomad/fakenomad v0.0.0

replace github.com/gastownhall/gc-runtime-nomad/fakenomad => ../fakenomad
