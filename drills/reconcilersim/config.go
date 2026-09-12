package reconcilersim

import "os"

// Env vars the harness reads. GC_NOMAD_ADDR/TOKEN/NAMESPACE/PARENT_JOB
// match the pack's own contract (README.md) so a drill's environment is a
// strict superset of the pack CLI's own — the harness passes those three
// straight through to the pack CLI subprocess unchanged (rewriting only
// ADDR when TLS fronting is in play, see Driver). The GC_NOMAD_TLS_* trio
// and GC_RUNTIME_NOMAD_BIN are new: the pack's own env contract deliberately
// excludes TLS config, and the pack never needs to
// name its own binary path to itself.
const (
	EnvAddr       = "GC_NOMAD_ADDR"
	EnvToken      = "GC_NOMAD_TOKEN"
	EnvNamespace  = "GC_NOMAD_NAMESPACE"
	EnvParentJob  = "GC_NOMAD_PARENT_JOB"
	EnvTLSCACert  = "GC_NOMAD_TLS_CACERT"
	EnvTLSCert    = "GC_NOMAD_TLS_CERT"
	EnvTLSKey     = "GC_NOMAD_TLS_KEY"
	EnvRuntimeBin = "GC_RUNTIME_NOMAD_BIN"
	// EnvAgentImage is the pack's required agent image. v1.0.7 made it
	// required and deleted the fallback session shape, so every
	// lifecycle op now fails closed without it — including every drill. The
	// harness supplies defaultAgentImg when the operator has not, because
	// these scenarios run against the in-memory fake, which models no driver
	// and no image: the value is a label the fake never reads. A live drill
	// still names its own real image through the environment, which wins.
	EnvAgentImage = "GC_NOMAD_AGENT_IMAGE"
	// EnvSkipGCCapabilityGate is the pack's own documented escape for an
	// offline harness — its refusal names this variable by name. Set by
	// newDriver because this harness mints no real sessions, so the gate has
	// nothing it could usefully measure.
	EnvSkipGCCapabilityGate = "GC_NOMAD_SKIP_GC_CAPABILITY_GATE"
	defaultParentJ          = "gc-sessions"
	defaultBin              = "gc-runtime-nomad"
	// Deliberately not a plausible registry reference: if this ever reaches a
	// real Docker driver, the pull must fail loudly rather than resolve.
	defaultAgentImg = "gc-runtime-nomad-drill-placeholder:offline"
)

// Config is the harness's full environment-derived configuration: the
// Nomad API target (plus optional mTLS, L4 mode) and the pack CLI binary
// to exec for lifecycle ops.
type Config struct {
	Addr       string
	Token      string
	Namespace  string
	ParentJob  string
	TLS        TLSConfig
	RuntimeBin string
}

// ConfigFromEnv reads Config from the process environment. It never fails
// on missing values — NewClient/Driver.New surface a clear error at the
// point an empty Addr is actually required, matching the pack's own
// newLifecycle/newClient error shape (main.go).
func ConfigFromEnv() Config {
	parentJob := os.Getenv(EnvParentJob)
	if parentJob == "" {
		parentJob = defaultParentJ
	}
	bin := os.Getenv(EnvRuntimeBin)
	if bin == "" {
		bin = defaultBin
	}
	return Config{
		Addr:      os.Getenv(EnvAddr),
		Token:     os.Getenv(EnvToken),
		Namespace: os.Getenv(EnvNamespace),
		ParentJob: parentJob,
		TLS: TLSConfig{
			CACertPath: os.Getenv(EnvTLSCACert),
			CertPath:   os.Getenv(EnvTLSCert),
			KeyPath:    os.Getenv(EnvTLSKey),
		},
		RuntimeBin: bin,
	}
}
