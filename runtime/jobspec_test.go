package main

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestParentJobSpecLogShipperDisabledByDefault confirms an unset
// GC_NOMAD_LOG_SINK (logShipperConfig{}) leaves the parent job spec exactly
// as it was before fnrt-t4l.13 — a single "agent" task, no extra ports, no
// Leader flag — so every existing deployment's jobspec (and its
// jobspecHash-based drift detection, fnrt-t4l.9) is unaffected.
func TestParentJobSpecLogShipperDisabledByDefault(t *testing.T) {
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{})

	group := spec.TaskGroups[0]
	if len(group.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1 (log-shipper must not be added when disabled)", len(group.Tasks))
	}
	if group.Tasks[0].Name != "agent" {
		t.Fatalf("task[0].Name = %q, want %q", group.Tasks[0].Name, "agent")
	}
	if group.Tasks[0].Leader {
		t.Fatalf("task[0].Leader = true, want false (no second task to order against)")
	}
	if len(group.Networks[0].DynamicPorts) != 0 {
		t.Fatalf("DynamicPorts = %v, want none", group.Networks[0].DynamicPorts)
	}
}

// TestParentJobSpecAddsLogShipperTask confirms the full shape fnrt-t4l.13
// scopes: a second "log-shipper" task, driver exec, the agent task promoted
// to Leader (kill_timeout ordering), a poststart+sidecar lifecycle, a
// pinned+checksummed vector artifact, an embedded vector.toml template, the
// three env-driven config values wired through, a group-local metrics port,
// and the shipper's KillTimeout outliving the agent's own (the bounded flush
// window).
func TestParentJobSpecAddsLogShipperTask(t *testing.T) {
	cfg := logShipperConfig{
		Sink:      "https://logs.example.internal/ingest",
		TokenFile: "/etc/gc/log-sink-token",
		Labels:    "env=lab,team=nrt",
	}
	spec := parentJobSpec("default", "", "gc-sessions", cfg)
	group := spec.TaskGroups[0]

	if len(group.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(group.Tasks))
	}
	agent, shipper := group.Tasks[0], group.Tasks[1]

	if agent.Name != "agent" || !agent.Leader {
		t.Fatalf("agent task = {Name:%q Leader:%v}, want {agent true}", agent.Name, agent.Leader)
	}
	if shipper.Name != logShipperTaskName {
		t.Fatalf("shipper.Name = %q, want %q", shipper.Name, logShipperTaskName)
	}
	if shipper.Driver != "exec" {
		t.Fatalf("shipper.Driver = %q, want %q", shipper.Driver, "exec")
	}
	if command, ok := shipper.Config["args"].([]string); !ok || len(command) != 2 || command[0] != "-c" {
		t.Fatalf("shipper.Config[args] = %#v, want shell wrapper args", shipper.Config["args"])
	} else {
		wrapper := command[1]
		if !strings.Contains(wrapper, `$${GC_LOG_SINK_TOKEN_FILE:-}`) {
			t.Errorf("log-shipper wrapper does not escape Nomad template interpolation: %q", wrapper)
		}
		for _, want := range []string{
			logShipperPIDFile,
			logShipperFlushRequest,
			logShipperFlushComplete,
			"mkdir -p /var/lib/vector",
			`wait "$vector_pid" || vector_status=$?`,
		} {
			if !strings.Contains(wrapper, want) {
				t.Errorf("log-shipper wrapper missing %q\ngot:\n%s", want, wrapper)
			}
		}
	}
	if shipper.KillTimeout <= agent.KillTimeout {
		t.Fatalf("shipper.KillTimeout = %d, want > agent.KillTimeout = %d (bounded flush window after the leader exits)", shipper.KillTimeout, agent.KillTimeout)
	}

	if len(shipper.Artifacts) != 1 {
		t.Fatalf("shipper.Artifacts = %d, want 1", len(shipper.Artifacts))
	}
	art := shipper.Artifacts[0]
	if art.GetterSource != vectorURL {
		t.Fatalf("artifact source = %q, want %q", art.GetterSource, vectorURL)
	}
	if art.GetterOptions["checksum"] != "sha256:"+vectorSHA256 {
		t.Fatalf("artifact checksum = %q, want %q", art.GetterOptions["checksum"], "sha256:"+vectorSHA256)
	}

	if len(shipper.Templates) != 1 || shipper.Templates[0].DestPath != "local/vector.toml" {
		t.Fatalf("shipper.Templates = %+v, want one entry at local/vector.toml", shipper.Templates)
	}
	toml := shipper.Templates[0].EmbeddedTmpl
	for _, want := range []string{
		`include = ["{{ env "NOMAD_ALLOC_DIR" }}/data/*.jsonl", "$${HOME}/.claude/projects/**/*.jsonl"]`,
		`include = ["{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout.*"]`,
		`uri = "{{ env "GC_LOG_SINK" }}"`,
		`type = "prometheus_exporter"`,
		`address = "0.0.0.0:{{ env "NOMAD_PORT_metrics" }}"`,
		`strategy = "bearer"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("vector.toml missing %q\ngot:\n%s", want, toml)
		}
	}

	if shipper.Env["GC_LOG_SINK"] != cfg.Sink {
		t.Errorf("Env[GC_LOG_SINK] = %q, want %q", shipper.Env["GC_LOG_SINK"], cfg.Sink)
	}
	if shipper.Env["GC_LOG_LABELS"] != cfg.Labels {
		t.Errorf("Env[GC_LOG_LABELS] = %q, want %q", shipper.Env["GC_LOG_LABELS"], cfg.Labels)
	}
	if shipper.Env["GC_LOG_SINK_TOKEN_FILE"] != cfg.TokenFile {
		t.Errorf("Env[GC_LOG_SINK_TOKEN_FILE] = %q, want %q", shipper.Env["GC_LOG_SINK_TOKEN_FILE"], cfg.TokenFile)
	}

	if len(group.Networks[0].DynamicPorts) != 1 || group.Networks[0].DynamicPorts[0].Label != logShipperMetricsPortLabel {
		t.Fatalf("DynamicPorts = %v, want one port labeled %q", group.Networks[0].DynamicPorts, logShipperMetricsPortLabel)
	}
}

// TestVectorConfigEscapesVectorOwnPlaceholders is the plain string-level
// check that vectorConfigTOML still doubles the "${VAR}" placeholders that
// deliberately remain on Vector's own runtime substitution — "${HOME}"
// (no Nomad-visible source for it) and, when TokenFile is set,
// "${GC_LOG_SINK_TOKEN}" (only exported into Vector's process env by
// logShipperWrapperScript, after Nomad's template render has already run).
// Nomad never gets a chance to consume these as long as the source has a
// doubled dollar, so asserting the doubled form is present is the whole
// contract — no simulated interpolation pass is needed to prove it.
func TestVectorConfigEscapesVectorOwnPlaceholders(t *testing.T) {
	got := vectorConfigTOML(logShipperConfig{
		Sink:      "http://sink.example/ingest",
		TokenFile: "/etc/gc/token",
	})
	for _, want := range []string{
		`$${HOME}/.claude/projects/**/*.jsonl`,
		`token = "$${GC_LOG_SINK_TOKEN}"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("vector.toml missing escaped placeholder %q\ngot:\n%s", want, got)
		}
	}
}

// TestVectorConfigRoutesTaskAndAllocValuesThroughNomadTemplate is the
// fnrt-3bvg regression: gc_log_sink's URI (and every other value Vector was
// previously expected to resolve from its own process environment, other
// than the two documented exceptions above) must be rendered by Nomad's own
// template `env` function instead, matching fnrt-t4l.24's prom_exporter fix.
// A live-Nomad proof (ops/receipts/nrt-t4l-20-lab-proof.md) showed Vector's
// own "${GC_LOG_SINK}" substitution does not reliably resolve inside the
// exec-driver task's environment, producing an invalid-URI Exit 78 that
// kills the whole session allocation.
func TestVectorConfigRoutesTaskAndAllocValuesThroughNomadTemplate(t *testing.T) {
	got := vectorConfigTOML(logShipperConfig{
		Sink:      "http://sink.example/ingest",
		Labels:    "env=lab,team=nrt",
		TokenFile: "/etc/gc/token",
	})
	for _, want := range []string{
		`{{ env "NOMAD_ALLOC_DIR" }}/data/*.jsonl`,
		`{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout.*`,
		`.session_name = "{{ env "NOMAD_META_GC_SESSION" }}"`,
		`.alloc_id = "{{ env "NOMAD_ALLOC_ID" }}"`,
		`.node = "{{ env "GC_LOG_NODE_NAME" }}"`,
		`parse_key_value("{{ env "GC_LOG_LABELS" }}"`,
		`uri = "{{ env "GC_LOG_SINK" }}"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("vector.toml missing Nomad template interpolation %q\ngot:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		`$${NOMAD_ALLOC_DIR}`,
		`$${NOMAD_META_GC_SESSION}`,
		`$${NOMAD_ALLOC_ID}`,
		`$${GC_LOG_NODE_NAME}`,
		`$${GC_LOG_LABELS}`,
		`$${GC_LOG_SINK}`,
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("vector.toml still relies on Vector's own substitution for %q\ngot:\n%s", unwanted, got)
		}
	}
}

// TestVectorConfigPromExporterAddressRendersThroughFakenomad is the
// fnrt-t4l.24 regression, RUN-2: a hand-rolled nomadTemplatePass simulator
// that string-matched "{{ env "NOMAD_PORT_metrics" }}" and substituted a
// constant was rejected as insufficient evidence — it never exercised
// fakenomad's own rendering of a Template stanza, so it could not tell a
// real fix from one that merely satisfied the simulator. This test instead
// drives the REAL production parentJobSpec through the REAL provider
// client's dispatch path, lets fakenomad render the log-shipper task's
// EmbeddedTmpl template for real (Go's text/template with a live `env`
// function, fakenomad.renderTemplates), and reads the rendered file back
// off disk over the same alloc-exec channel a real deployment uses — then
// asserts the address is a real, net.SplitHostPort-parseable socket address
// using the exact dynamic port fakenomad itself assigned, and that the
// log-shipper task's own command actually started.
func TestVectorConfigPromExporterAddressRendersThroughFakenomad(t *testing.T) {
	l, srv := newTestLifecycle(t)
	l.logShipper = logShipperConfig{Sink: "http://sink.example/ingest"}

	ctx := context.Background()
	const session = "sess-prom-exporter-render"
	if err := l.opStartWithConfig(ctx, session, stageConfig{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	allocID, err := l.currentAlloc(ctx, session)
	if err != nil {
		t.Fatalf("resolve current alloc: %v", err)
	}

	state, ok := srv.TaskState(allocID, logShipperTaskName)
	if !ok || state != "running" {
		t.Fatalf("log-shipper task state = (%q, %v), want (\"running\", true) — the task never started", state, ok)
	}

	port, ok := srv.AssignedPort(allocID, logShipperMetricsPortLabel)
	if !ok {
		t.Fatalf("fakenomad assigned no dynamic port for label %q", logShipperMetricsPortLabel)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"cat", "local/vector.toml"})
	if err != nil || exitCode != 0 {
		t.Fatalf("cat rendered vector.toml: exit=%d err=%v out=%s", exitCode, err, out)
	}
	rendered := string(out)

	const marker = `address = "`
	start := strings.Index(rendered, marker)
	if start < 0 {
		t.Fatalf("no prom_exporter address line in rendered config:\n%s", rendered)
	}
	start += len(marker)
	end := strings.IndexByte(rendered[start:], '"')
	if end < 0 {
		t.Fatalf("unterminated address line in rendered config:\n%s", rendered)
	}
	address := rendered[start : start+end]

	if strings.Contains(address, "${") || strings.Contains(address, "{{") {
		t.Fatalf("prom_exporter address still contains an unresolved template expression: %q", address)
	}
	host, gotPort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("prom_exporter address %q is not a valid socket address: %v", address, err)
	}
	if host != "0.0.0.0" {
		t.Errorf("prom_exporter address host = %q, want 0.0.0.0", host)
	}
	if gotPort != strconv.Itoa(port) {
		t.Errorf("prom_exporter address port = %q, want fakenomad's assigned port %d", gotPort, port)
	}
}

// TestVectorConfigLogSinkURIRendersThroughFakenomad is the fnrt-3bvg
// regression, mirroring TestVectorConfigPromExporterAddressRendersThroughFakenomad
// for the sibling defect at gc_log_sink's URI: it drives the REAL
// parentJobSpec through fakenomad's own Template-stanza rendering and reads
// the rendered file back off disk, then asserts the sink URI is the real,
// literal GC_NOMAD_LOG_SINK value rather than an unresolved placeholder —
// exactly what the live-Nomad lab proof (ops/receipts/nrt-t4l-20-lab-proof.md)
// showed failing as "invalid uri character" (Exit 78, allocation killed). It
// also confirms the alloc-scoped values (.alloc_id, .session_name) resolved
// to fakenomad's real values, and that no Nomad template expression in the
// file survived unresolved.
func TestVectorConfigLogSinkURIRendersThroughFakenomad(t *testing.T) {
	const sink = "http://sink.example/ingest"
	l, srv := newTestLifecycle(t)
	l.logShipper = logShipperConfig{Sink: sink, Labels: "env=lab,team=nrt"}

	ctx := context.Background()
	const session = "sess-log-sink-render"
	if err := l.opStartWithConfig(ctx, session, stageConfig{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	allocID, err := l.currentAlloc(ctx, session)
	if err != nil {
		t.Fatalf("resolve current alloc: %v", err)
	}

	state, ok := srv.TaskState(allocID, logShipperTaskName)
	if !ok || state != "running" {
		t.Fatalf("log-shipper task state = (%q, %v), want (\"running\", true) — the task never started", state, ok)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"cat", "local/vector.toml"})
	if err != nil || exitCode != 0 {
		t.Fatalf("cat rendered vector.toml: exit=%d err=%v out=%s", exitCode, err, out)
	}
	rendered := string(out)

	if strings.Contains(rendered, "{{") {
		t.Fatalf("rendered vector.toml still contains an unresolved Nomad template expression:\n%s", rendered)
	}

	uri, ok := quotedValueAfter(rendered, `uri = "`)
	if !ok {
		t.Fatalf("no gc_log_sink uri line in rendered config:\n%s", rendered)
	}
	if uri != sink {
		t.Fatalf("gc_log_sink uri = %q, want configured GC_NOMAD_LOG_SINK %q", uri, sink)
	}

	allocIDField, ok := quotedValueAfter(rendered, `.alloc_id = "`)
	if !ok {
		t.Fatalf("no .alloc_id line in rendered config:\n%s", rendered)
	}
	if allocIDField != allocID {
		t.Errorf(".alloc_id = %q, want fakenomad's real alloc ID %q", allocIDField, allocID)
	}

	sessionField, ok := quotedValueAfter(rendered, `.session_name = "`)
	if !ok {
		t.Fatalf("no .session_name line in rendered config:\n%s", rendered)
	}
	if sessionField != session {
		t.Errorf(".session_name = %q, want dispatched session name %q", sessionField, session)
	}
}

// quotedValueAfter returns the double-quoted value immediately following
// marker in s — the same substring-scraping approach the tests above use to
// pull one field out of a rendered vector.toml without a TOML parser.
func quotedValueAfter(s, marker string) (string, bool) {
	start := strings.Index(s, marker)
	if start < 0 {
		return "", false
	}
	start += len(marker)
	end := strings.IndexByte(s[start:], '"')
	if end < 0 {
		return "", false
	}
	return s[start : start+end], true
}

func TestVectorConfigDisablesFileCheckpoints(t *testing.T) {
	toml := vectorConfigTOML(logShipperConfig{Sink: "http://sink.example/ingest"})
	if got := strings.Count(toml, "ignore_checkpoints = true"); got != 2 {
		t.Fatalf("ignore_checkpoints occurrences = %d, want one for each file source\n%s", got, toml)
	}
}

func TestParentJobSpecUsesConfiguredLogShipperArtifact(t *testing.T) {
	const artifact = "/var/lib/nrt-p3-02/vector-http/vector-0.58.0-x86_64-unknown-linux-gnu.tar.gz"
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{
		Sink:     "http://127.0.0.1:18081/ingest",
		Artifact: artifact,
	})

	got := spec.TaskGroups[0].Tasks[1].Artifacts[0].GetterSource
	if got != artifact {
		t.Fatalf("artifact source = %q, want configured local path %q", got, artifact)
	}
}

func TestParentJobSpecMarksLogShipperAsNonFatalSidecar(t *testing.T) {
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{
		Sink: "http://127.0.0.1:18081/ingest",
	})

	wire, err := json.Marshal(spec.TaskGroups[0].Tasks[1])
	if err != nil {
		t.Fatalf("marshal log-shipper task: %v", err)
	}
	var task map[string]any
	if err := json.Unmarshal(wire, &task); err != nil {
		t.Fatalf("unmarshal log-shipper task: %v", err)
	}
	lifecycle, ok := task["Lifecycle"].(map[string]any)
	if !ok {
		t.Fatalf("log-shipper Lifecycle = %#v, want poststart sidecar lifecycle", task["Lifecycle"])
	}
	if lifecycle["Hook"] != "poststart" || lifecycle["Sidecar"] != true {
		t.Fatalf("log-shipper Lifecycle = %#v, want Hook=poststart Sidecar=true", lifecycle)
	}
}

// TestVectorConfigOmitsAuthBlockWithoutTokenFile confirms an unset
// GC_NOMAD_LOG_SINK_TOKEN_FILE produces an unauthenticated sink (no bearer
// block at all) rather than a literal empty token — vector has no
// token-file primitive of its own for the http sink's auth block.
func TestVectorConfigOmitsAuthBlockWithoutTokenFile(t *testing.T) {
	toml := vectorConfigTOML(logShipperConfig{Sink: "https://logs.example.internal/ingest"})
	if strings.Contains(toml, "[sinks.gc_log_sink.auth]") {
		t.Fatalf("vector.toml has an auth block with no token file configured:\n%s", toml)
	}
}

// TestJobspecHashChangesWhenLogShipperToggled ties fnrt-t4l.13 into
// fnrt-t4l.9's drift-detection mechanism: ensureParentRegistered decides
// whether to re-register purely from jobspecHash, so toggling
// GC_NOMAD_LOG_SINK on an already-registered parent must change the hash —
// otherwise a deployment that turns the feature on would never see its
// parent job actually get the log-shipper task added.
func TestJobspecHashChangesWhenLogShipperToggled(t *testing.T) {
	off := parentJobSpec("default", "", "gc-sessions", logShipperConfig{})
	on := parentJobSpec("default", "", "gc-sessions", logShipperConfig{Sink: "https://logs.example.internal/ingest"})

	if off.Meta[jobspecHashMetaKey] == on.Meta[jobspecHashMetaKey] {
		t.Fatalf("jobspec hash unchanged after enabling the log-shipper task")
	}
}

// TestParentJobSpecIsJSONMarshalable guards jobspecHash's own panic
// invariant (jobspec.go: "job is built entirely from static Go types... a
// panic would only ever fire from a future edit that adds an unmarshalable
// field") now that the log-shipper task adds several new field types.
func TestParentJobSpecIsJSONMarshalable(t *testing.T) {
	cfg := logShipperConfig{Sink: "https://logs.example.internal/ingest", TokenFile: "/etc/tok", Labels: "a=b"}
	spec := parentJobSpec("default", "", "gc-sessions", cfg)
	if _, err := json.Marshal(spec); err != nil {
		t.Fatalf("marshal parent job spec with log-shipper enabled: %v", err)
	}
}
