module github.com/gastownhall/gc-runtime-nomad

go 1.25.9

// fakenomad is this pack's own in-memory Nomad API fake (NRT-P1-02), used
// only by _test.go files (`gc runtime check` offline lifecycle round-trip,
// NRT-P1-03). The production client uses the pinned Nomad API module below;
// the pack still keeps its zero-gascity-imports contract.
require github.com/gastownhall/gc-runtime-nomad/fakenomad v0.0.0

require github.com/hashicorp/nomad/api v0.0.0-20260821123358-aa026cc99cfb

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/hashicorp/cronexpr v1.1.3 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-rootcerts v1.0.2 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/gastownhall/gc-runtime-nomad/fakenomad => ../fakenomad
