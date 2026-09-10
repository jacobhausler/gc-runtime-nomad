package main

import (
	"context"
	"encoding/json"
	"net"
	"path"
	"regexp"
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
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})

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

// TestSessionTaskReservesSecretsSpace protects the in-memory CODEX_HOME used
// by the deployment bootstrap. Nomad's unset default is only 1 MiB; Codex
// creates its state database there before it can begin a session.
func TestSessionTaskReservesSecretsSpace(t *testing.T) {
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})
	got := spec.TaskGroups[0].Tasks[0].Resources.SecretsMB
	if got != sessionSecretsMB {
		t.Fatalf("agent SecretsMB = %d, want %d", got, sessionSecretsMB)
	}
	if memory := spec.TaskGroups[0].Tasks[0].Resources.MemoryMB; got > memory {
		t.Fatalf("agent SecretsMB = %d exceeds MemoryMB = %d (Nomad rejects this jobspec)", got, memory)
	}
}

func TestParentJobSpecUsesConfiguredDockerAgentImage(t *testing.T) {
	const image = "registry.example/gc-agent:2026-09-08"
	spec := parentJobSpec("gc", "lab-session", "gc-sessions", logShipperConfig{}, image, placementConfig{})

	agent := spec.TaskGroups[0].Tasks[0]
	if agent.Driver != "docker" {
		t.Fatalf("agent Driver = %q, want docker", agent.Driver)
	}
	if got := agent.Config["image"]; got != image {
		t.Fatalf("agent Config[image] = %#v, want %q", got, image)
	}
}

func TestParentJobSpecDockerImagePreservesNomadControls(t *testing.T) {
	constraint := nomadConstraint{LTarget: "${node.unique.name}", RTarget: "nomad-a", Operand: "="}
	spec := parentJobSpec("gc", "lab-session", "gc-sessions", logShipperConfig{}, "gc-agent:test", placementConfig{
		dnsServers:      []string{"192.168.0.202"},
		nodeConstraints: []nomadConstraint{constraint},
	})

	if spec.NodePool != "lab-session" || len(spec.Constraints) != 2 || spec.Constraints[1] != constraint {
		t.Fatalf("placement = node_pool=%q constraints=%+v, want node_pool lab-session plus configured constraint", spec.NodePool, spec.Constraints)
	}
	group := spec.TaskGroups[0]
	if len(group.Networks) != 1 || group.Networks[0].DNS == nil || group.Networks[0].DNS.Servers[0] != "192.168.0.202" {
		t.Fatalf("network = %+v, want bridge DNS preserved", group.Networks)
	}
	agent := group.Tasks[0]
	if agent.Resources.CPU != sessionCPU || agent.Resources.MemoryMB != sessionMemoryMB || agent.Resources.SecretsMB != sessionSecretsMB {
		t.Fatalf("agent resources = %+v, want CPU=%d MemoryMB=%d SecretsMB=%d", agent.Resources, sessionCPU, sessionMemoryMB, sessionSecretsMB)
	}
	if group.RestartPolicy != (nomadRestartPolicy{Attempts: 0, Mode: "fail"}) {
		t.Fatalf("restart policy = %+v, want no Nomad restart", group.RestartPolicy)
	}
	if group.ReschedulePolicy != (nomadReschedulePolicy{Attempts: 0, Unlimited: false}) {
		t.Fatalf("reschedule policy = %+v, want no Nomad reschedule", group.ReschedulePolicy)
	}
	if group.Disconnect == nil || group.Disconnect.Replace == nil || *group.Disconnect.Replace || group.Disconnect.Reconcile != "keep_original" || group.Disconnect.LostAfter != lostAfter.Nanoseconds() {
		t.Fatalf("disconnect = %+v, want keep_original/replace=false/lost_after=%d", group.Disconnect, lostAfter.Nanoseconds())
	}
}

func TestParentJobSpecDockerImageKeepsOptionalLogShipper(t *testing.T) {
	spec := parentJobSpec("gc", "lab-session", "gc-sessions", logShipperConfig{Sink: "https://logs.example/ingest"}, "gc-agent:test", placementConfig{})
	group := spec.TaskGroups[0]
	if len(group.Tasks) != 2 || group.Tasks[0].Name != "agent" || group.Tasks[1].Name != logShipperTaskName {
		t.Fatalf("Docker tasks = %+v, want agent plus optional log shipper", group.Tasks)
	}
	if !group.Tasks[0].Leader {
		t.Fatalf("Docker agent Leader = false, want the existing sidecar shutdown ordering")
	}
	if group.Tasks[1].Lifecycle == nil || !group.Tasks[1].Lifecycle.Sidecar {
		t.Fatalf("Docker log-shipper lifecycle = %+v, want the existing sidecar lifecycle", group.Tasks[1].Lifecycle)
	}
}

// TestJobspecHashChangesWhenAgentImageToggled is the drift guard for the one
// session shape there is: pointing the pack at a different agent image must
// land a different jobspec hash, so ensureParentRegistered sees the
// already-registered parent as stale instead of letting boxes keep launching
// from the old task (cr-3vvnb5.2 retires the no-image variant this row once
// compared against).
func TestJobspecHashChangesWhenAgentImageToggled(t *testing.T) {
	off := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})
	on := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, "gc-agent:other", placementConfig{})
	if off.Meta[jobspecHashMetaKey] == on.Meta[jobspecHashMetaKey] {
		t.Fatalf("jobspec hash unchanged after changing the Docker agent image")
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
	spec := parentJobSpec("default", "", "gc-sessions", cfg, testAgentImage, placementConfig{})
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
		`include = ["{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout", "{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout.*"]`,
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
	if err := l.opStartWithConfig(ctx, session, withPane(stageConfig{})); err != nil {
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
	if err := l.opStartWithConfig(ctx, session, withPane(stageConfig{})); err != nil {
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
	fileSources := strings.Count(toml, "type = \"file\"")
	if got := strings.Count(toml, "ignore_checkpoints = true"); got != fileSources {
		t.Fatalf("ignore_checkpoints occurrences = %d, want one for each of the %d file sources\n%s", got, fileSources, toml)
	}
}

// shippedAgentStdoutRel is the path, relative to $NOMAD_ALLOC_DIR, of the file
// the deployment bootstrap actually writes inside the allocation:
// ops/nomad/agent-launch.sh taps the agent's pane with `tmux pipe-pane -o
// "cat >> '$LOGS/agent.stdout'"` (LOGS = ${NOMAD_ALLOC_DIR}/logs) and
// runtime/ops.go's egressFiles copies alloc/logs/agent.stdout out verbatim.
// It is UNSUFFIXED — the whole of cr-gdeav.6.5.5.
const shippedAgentStdoutRel = "logs/agent.stdout"

// includePatternList is the `"..."` entries of one bracketed include list.
var includePatternList = regexp.MustCompile(`"([^"]*)"`)

// sessionStdoutIncludes pulls the [sources.session_stdout] include patterns out
// of a rendered vector.toml — the same substring-scraping style as
// quotedValueAfter, extended to a bracketed list, so the test below measures
// the shipped config rather than a copy of it.
func sessionStdoutIncludes(t *testing.T, rendered string) []string {
	t.Helper()
	return sourceIncludes(t, rendered, "session_stdout")
}

// sourceIncludes is sessionStdoutIncludes generalized to any named source, so
// every capture channel can be measured against the same rendered config.
func sourceIncludes(t *testing.T, rendered, source string) []string {
	t.Helper()
	block := strings.Index(rendered, "[sources."+source+"]")
	if block < 0 {
		t.Fatalf("no [sources.%s] source in rendered config:\n%s", source, rendered)
	}
	rest := rendered[block:]
	start := strings.Index(rest, "include = [")
	if start < 0 {
		t.Fatalf("sources.%s has no include list:\n%s", source, rest)
	}
	rest = rest[start:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		t.Fatalf("unterminated sources.%s include list:\n%s", source, rest)
	}
	var patterns []string
	for _, m := range includePatternList.FindAllStringSubmatch(rest[:end], -1) {
		patterns = append(patterns, m[1])
	}
	if len(patterns) == 0 {
		t.Fatalf("sources.%s include list is empty:\n%s", source, rest[:end])
	}
	return patterns
}

// TestVectorSessionStdoutSourceMatchesShippedCapture is the cr-gdeav.6.5.5
// regression: [sources.session_stdout] declared only
// "{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout.*". Vector's glob treats that
// "." as a literal and "*" as not crossing "/", so the pattern REQUIRES a
// suffix and never matched the unsuffixed logs/agent.stdout the launcher
// writes and the stop-path egress names. The pane capture was therefore
// written, egressed, and silently never shipped.
//
// Like the two render tests above, this drives the REAL parentJobSpec through
// fakenomad's own Template-stanza rendering and reads the rendered file back,
// so what gets matched is what Vector would receive on a real node; the alloc
// dir is recovered from the sibling source in that same rendered file rather
// than restated here. Matching then happens on the allocation-relative remainder
// of each pattern, which keeps a temp-path alloc dir (whatever characters it
// contains) out of the glob entirely.
func TestVectorSessionStdoutSourceMatchesShippedCapture(t *testing.T) {
	rendered, allocDir := renderedShipperConfig(t, "sess-stdout-source-match")

	// The two names that can exist under the allocation's logs dir: the file
	// the bootstrap writes, and Nomad's rotation of the "agent" task's stream.
	wanted := map[string]bool{
		shippedAgentStdoutRel:        false,
		shippedAgentStdoutRel + ".1": false,
	}
	for _, pattern := range sessionStdoutIncludes(t, rendered) {
		// The credential boundary: the shipper may read nothing outside the
		// allocation's logs dir (runtime/ops.go's egress allowlist is the same
		// boundary, and must not be out-wide-listed here).
		if !strings.HasPrefix(pattern, allocDir+"/logs/") {
			t.Errorf("session_stdout pattern %q is outside %s/logs/ — the shipper must stay inside the allocation's logs dir", pattern, allocDir)
			continue
		}
		rel := strings.TrimPrefix(pattern, allocDir+"/")
		for candidate := range wanted {
			matched, err := path.Match(rel, candidate)
			if err != nil {
				t.Fatalf("pattern %q is not a valid glob: %v", rel, err)
			}
			if matched {
				wanted[candidate] = true
			}
		}
	}
	for candidate, matched := range wanted {
		if !matched {
			t.Errorf("no session_stdout pattern matches the allocation-relative path %q (patterns: %v)", candidate, sessionStdoutIncludes(t, rendered))
		}
	}

	// Canary: the pattern this bead fixed must NOT match the shipped file. If it
	// ever does, the assertion above is a tautology and this test has stopped
	// proving the bug it was written for.
	if stale, err := path.Match("logs/agent.stdout.*", shippedAgentStdoutRel); err != nil {
		t.Fatalf("canary glob: %v", err)
	} else if stale {
		t.Fatalf("canary broken: the suffix-only pattern logs/agent.stdout.* matches %q — this test no longer proves the cr-gdeav.6.5.5 regression", shippedAgentStdoutRel)
	}
}

func TestParentJobSpecUsesConfiguredLogShipperArtifact(t *testing.T) {
	const artifact = "/var/lib/nrt-p3-02/vector-http/vector-0.58.0-x86_64-unknown-linux-gnu.tar.gz"
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{
		Sink:     "http://127.0.0.1:18081/ingest",
		Artifact: artifact,
	}, testAgentImage, placementConfig{})

	got := spec.TaskGroups[0].Tasks[1].Artifacts[0].GetterSource
	if got != artifact {
		t.Fatalf("artifact source = %q, want configured local path %q", got, artifact)
	}
}

func TestParentJobSpecMarksLogShipperAsNonFatalSidecar(t *testing.T) {
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{
		Sink: "http://127.0.0.1:18081/ingest",
	}, testAgentImage, placementConfig{})

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
	off := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})
	on := parentJobSpec("default", "", "gc-sessions", logShipperConfig{Sink: "https://logs.example.internal/ingest"}, testAgentImage, placementConfig{})

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
	spec := parentJobSpec("default", "", "gc-sessions", cfg, testAgentImage, placementConfig{})
	if _, err := json.Marshal(spec); err != nil {
		t.Fatalf("marshal parent job spec with log-shipper enabled: %v", err)
	}
}

// renderedShipperConfig drives the REAL parentJobSpec through fakenomad's own
// Template-stanza rendering and returns the vector.toml Vector would receive on
// a real node — so the capture tests measure the shipped config rather than a
// copy of it. NOMAD_ALLOC_DIR is recovered from the sibling session_jsonl
// source in that same rendered file rather than restated by the caller, which
// keeps a temp-path alloc dir (whatever characters it contains) out of the
// globs entirely.
func renderedShipperConfig(t *testing.T, session string) (rendered, allocDir string) {
	t.Helper()
	l, srv := newTestLifecycle(t)
	l.logShipper = logShipperConfig{Sink: "http://sink.example/ingest"}

	ctx := context.Background()
	if err := l.opStartWithConfig(ctx, session, withPane(stageConfig{})); err != nil {
		t.Fatalf("start: %v", err)
	}
	allocID, err := l.currentAlloc(ctx, session)
	if err != nil {
		t.Fatalf("resolve current alloc: %v", err)
	}
	if state, ok := srv.TaskState(allocID, logShipperTaskName); !ok || state != "running" {
		t.Fatalf("log-shipper task state = (%q, %v), want (\"running\", true) — the task never started", state, ok)
	}

	exitCode, out, err := l.opExec(ctx, session, []string{"cat", "local/vector.toml"})
	if err != nil || exitCode != 0 {
		t.Fatalf("cat rendered vector.toml: exit=%d err=%v out=%s", exitCode, err, out)
	}
	rendered = string(out)
	if strings.Contains(rendered, "{{") {
		t.Fatalf("rendered vector.toml still contains an unresolved Nomad template expression:\n%s", rendered)
	}
	jsonlPattern, ok := quotedValueAfter(rendered, `include = ["`)
	if !ok {
		t.Fatalf("no session_jsonl include line in rendered config:\n%s", rendered)
	}
	allocDir, found := strings.CutSuffix(jsonlPattern, "/data/*.jsonl")
	if !found || allocDir == "" {
		t.Fatalf("could not recover NOMAD_ALLOC_DIR from rendered include %q:\n%s", jsonlPattern, rendered)
	}
	return rendered, allocDir
}

// renderedSection returns the body of one rendered TOML table, stopping at the
// next table header.
func renderedSection(t *testing.T, rendered, table string) string {
	t.Helper()
	header := "[" + table + "]"
	i := strings.Index(rendered, header)
	if i < 0 {
		t.Fatalf("no %s table in rendered config:\n%s", header, rendered)
	}
	rest := rendered[i+len(header):]
	if next := strings.Index(rest, "\n["); next >= 0 {
		rest = rest[:next]
	}
	return rest
}

// quotedValuesInList pulls the quoted entries of one `key = [ ... ]` list out
// of a rendered TOML body — the bracketed-list half of quotedValueAfter.
func quotedValuesInList(t *testing.T, block, key, what string) []string {
	t.Helper()
	start := strings.Index(block, key+" = [")
	if start < 0 {
		t.Fatalf("%s has no %s list:\n%s", what, key, block)
	}
	rest := block[start:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		t.Fatalf("unterminated %s %s list:\n%s", what, key, block)
	}
	var out []string
	for _, m := range includePatternList.FindAllStringSubmatch(rest[:end], -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s %s list is empty:\n%s", what, key, block)
	}
	return out
}

// TestVectorSessionCaptureSources is cr-gdeav.6.5.8: runtime/ops.go's
// egressFiles names THREE agent-capture files inside the allocation's logs dir
// — agent.stdout, agent.stderr, agent.exit — but vectorConfigTOML shipped file
// patterns for stdout alone. The other two channels were written by
// ops/nomad/agent-launch.sh (`"$AGENT_CMD" 2>>"$LOGS/agent.stderr"`, and the
// agent.exit.$$ rename), copied out on the stop path, and then silently never
// shipped.
//
// Each named file must be matched by a source, and every capture source must
// reach the sink through the one label transform — and every capture pattern
// must stay a named file under the allocation's logs dir. That boundary is the
// credential boundary: runtime/ops.go's egress allowlist exists because a
// recursive sweep of an allocation reaches the staged secrets, and the shipper
// must not be wider than the copy-out it mirrors.
func TestVectorSessionCaptureSources(t *testing.T) {
	rendered, allocDir := renderedShipperConfig(t, "sess-capture-sources")
	logsDir := allocDir + "/logs/"

	// The two names the launcher's own stream and Nomad's rotation can each
	// produce, per source; agent.exit only ever has the final renamed name.
	captureSources := map[string][]string{
		"session_stdout": {"agent.stdout", "agent.stdout.1"},
		"session_stderr": {"agent.stderr", "agent.stderr.1"},
		"agent_exit":     {"agent.exit"},
	}
	for _, source := range []string{"session_stdout", "session_stderr", "agent_exit"} {
		patterns := sourceIncludes(t, rendered, source)
		seen := map[string]bool{}
		for _, pattern := range patterns {
			if !strings.HasPrefix(pattern, logsDir) {
				t.Errorf("sources.%s pattern %q is outside %s — the capture shipper must stay inside the allocation's logs dir", source, pattern, logsDir)
				continue
			}
			if strings.Contains(pattern, "**") {
				t.Errorf("sources.%s pattern %q is recursive discovery; the capture channels are named files", source, pattern)
			}
			rel := strings.TrimPrefix(pattern, logsDir)
			for _, candidate := range captureSources[source] {
				matched, err := path.Match(rel, candidate)
				if err != nil {
					t.Fatalf("sources.%s pattern %q is not a valid glob: %v", source, rel, err)
				}
				if matched {
					seen[candidate] = true
				}
			}
		}
		for _, candidate := range captureSources[source] {
			if !seen[candidate] {
				t.Errorf("sources.%s has no pattern matching logs/%s (patterns: %v)", source, candidate, patterns)
			}
		}
	}

	// One labeling path, one sink: the stderr and exit records must carry the
	// same session_name/alloc_id/node fields as the stdout captured beside them.
	labelInputs := quotedValuesInList(t, renderedSection(t, rendered, "transforms.label"), "inputs", "transforms.label")
	for _, source := range []string{"session_jsonl", "session_stdout", "session_stderr", "agent_exit"} {
		var wired bool
		for _, in := range labelInputs {
			if in == source {
				wired = true
			}
		}
		if !wired {
			t.Errorf("sources.%s is not an input of the label transform — it would reach the sink unlabeled, or not at all (label inputs: %v)", source, labelInputs)
		}
	}
	sinkInputs := quotedValuesInList(t, renderedSection(t, rendered, "sinks.gc_log_sink"), "inputs", "sinks.gc_log_sink")
	if len(sinkInputs) != 1 || sinkInputs[0] != "label" {
		t.Errorf("gc_log_sink inputs = %v, want [label] — every capture channel must ride the labeled stream", sinkInputs)
	}
}

// TestParentJobSpecDeclaresNoDNSByDefault protects every deployment that never
// sets GC_NOMAD_DNS_SERVERS: the session group must render exactly the network
// stanza it rendered before cr-tzsxk.18 — no DNS key at all — so that deploy-
// ment's jobspecHash (and therefore ensureParentRegistered's drift decision) is
// unchanged by this pack's upgrade.
func TestParentJobSpecDeclaresNoDNSByDefault(t *testing.T) {
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})
	data, err := json.Marshal(spec.TaskGroups[0].Networks)
	if err != nil {
		t.Fatalf("marshal networks: %v", err)
	}
	if strings.Contains(string(data), `"DNS"`) {
		t.Fatalf("networks = %s, want no DNS stanza with no DNS servers configured", data)
	}
	baseline := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})
	if baseline.Meta[jobspecHashMetaKey] != spec.Meta[jobspecHashMetaKey] {
		t.Fatalf("jobspec hash changed for a deployment that configured no placement")
	}
}

// TestParentJobSpecCarriesDeploymentDNS is the bug cr-tzsxk.18 filed: an alloc
// on a systemd-resolved client host inherits the 127.0.0.53 stub, which is
// unreachable from its bridge netns, so no name resolves inside the box. The
// configured resolvers must reach Nomad's network.DNS.Servers on the wire —
// fakenomad ignores that stanza, so the assertion is on the marshalled shape.
func TestParentJobSpecCarriesDeploymentDNS(t *testing.T) {
	dep := placementConfig{dnsServers: []string{"192.168.0.202", "1.1.1.1"}}
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, dep)

	nets := spec.TaskGroups[0].Networks
	if len(nets) != 1 || nets[0].Mode != "bridge" {
		t.Fatalf("networks = %+v, want one bridge network", nets)
	}
	if nets[0].DNS == nil || len(nets[0].DNS.Servers) != 2 || nets[0].DNS.Servers[0] != "192.168.0.202" {
		t.Fatalf("networks[0].DNS = %+v, want the configured resolvers in order", nets[0].DNS)
	}
	data, err := json.Marshal(nets)
	if err != nil {
		t.Fatalf("marshal networks: %v", err)
	}
	want := `"DNS":{"Servers":["192.168.0.202","1.1.1.1"]}`
	if !strings.Contains(string(data), want) {
		t.Fatalf("networks json = %s, want it to contain %s", data, want)
	}
}

// TestParentJobSpecAppendsNodeConstraintsAfterKernelFatalRule pins the ordering
// an operator fence must never disturb: the host-network fatal rule stays first
// and a deployment constraint rides beside it, not in its place.
func TestParentJobSpecAppendsNodeConstraintsAfterKernelFatalRule(t *testing.T) {
	constraint, err := parseNodeConstraints(`${node.unique.name} != prod-nomad`)
	if err != nil {
		t.Fatalf("parseNodeConstraints: %v", err)
	}
	spec := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{nodeConstraints: constraint})

	if len(spec.Constraints) != 2 {
		t.Fatalf("constraints = %+v, want the kernel rule plus the deployment fence", spec.Constraints)
	}
	if first := spec.Constraints[0]; first.LTarget != "${attr.kernel.name}" || first.RTarget != "linux" || first.Operand != "=" {
		t.Fatalf("constraints[0] = %+v, want the linux-kernel fatal rule first", first)
	}
	want := nomadConstraint{LTarget: "${node.unique.name}", RTarget: "prod-nomad", Operand: "!="}
	if got := spec.Constraints[1]; got != want {
		t.Fatalf("constraints[1] = %+v, want %+v", got, want)
	}
}

// TestJobspecHashChangesWhenPlacementConfigured wires the two knobs into
// fnrt-t4l.9's drift detection: ensureParentRegistered decides to re-register
// purely from jobspecHash, so a deployment that configures a resolver or a
// fence on an already-registered parent must see the parent actually change —
// which is the whole point of cr-tzsxk.18 (an out-of-band re-registration is
// not needed once the runtime's own register carries the placement).
func TestJobspecHashChangesWhenPlacementConfigured(t *testing.T) {
	off := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{})

	dns := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{dnsServers: []string{"192.168.0.202"}})
	if off.Meta[jobspecHashMetaKey] == dns.Meta[jobspecHashMetaKey] {
		t.Fatalf("jobspec hash unchanged after configuring DNS servers")
	}

	fence := parentJobSpec("default", "", "gc-sessions", logShipperConfig{}, testAgentImage, placementConfig{nodeConstraints: []nomadConstraint{
		{LTarget: "${node.unique.name}", RTarget: "prod-nomad", Operand: "!="},
	}})
	if off.Meta[jobspecHashMetaKey] == fence.Meta[jobspecHashMetaKey] {
		t.Fatalf("jobspec hash unchanged after configuring a node constraint")
	}
}

func TestParseDNSServers(t *testing.T) {
	t.Run("empty configures nothing", func(t *testing.T) {
		for _, raw := range []string{"", "  ", ",", " , "} {
			got, err := parseDNSServers(raw)
			if err != nil || got != nil {
				t.Fatalf("parseDNSServers(%q) = %v, %v; want nil, nil", raw, got, err)
			}
		}
	})
	t.Run("list of addresses", func(t *testing.T) {
		got, err := parseDNSServers(" 192.168.0.202 , 10.0.0.1 ")
		if err != nil {
			t.Fatalf("parseDNSServers: %v", err)
		}
		if len(got) != 2 || got[0] != "192.168.0.202" || got[1] != "10.0.0.1" {
			t.Fatalf("parseDNSServers = %v, want [192.168.0.202 10.0.0.1]", got)
		}
	})
	t.Run("a hostname is a config error", func(t *testing.T) {
		// dns.servers takes addresses; a name here silently reintroduces the
		// unresolvable-box bug, so it must fail the op rather than register.
		_, err := parseDNSServers("resolver.example.internal")
		if err == nil || !strings.Contains(err.Error(), envDNSServers) {
			t.Fatalf("parseDNSServers(hostname) error = %v, want one naming %s", err, envDNSServers)
		}
	})
}

func TestParseNodeConstraints(t *testing.T) {
	t.Run("empty configures nothing", func(t *testing.T) {
		got, err := parseNodeConstraints("  ")
		if err != nil || got != nil {
			t.Fatalf("parseNodeConstraints = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("nomad syntax", func(t *testing.T) {
		got, err := parseNodeConstraints(`${node.unique.name} != prod-nomad`)
		if err != nil {
			t.Fatalf("parseNodeConstraints: %v", err)
		}
		want := nomadConstraint{LTarget: "${node.unique.name}", RTarget: "prod-nomad", Operand: "!="}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("parseNodeConstraints = %+v, want [%+v]", got, want)
		}
	})
	t.Run("semicolon separates; a comma list stays in the value", func(t *testing.T) {
		got, err := parseNodeConstraints(`${node.class} in gpu,large; ${attr.kernel.name} != darwin`)
		if err != nil {
			t.Fatalf("parseNodeConstraints: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("parseNodeConstraints = %+v, want 2 constraints", got)
		}
		if got[0].Operand != "in" || got[0].RTarget != "gpu,large" {
			t.Fatalf("constraints[0] = %+v, want the comma list kept intact in RTarget", got[0])
		}
	})
	t.Run("malformed shapes fail closed", func(t *testing.T) {
		for _, raw := range []string{
			`${node.unique.name} !=`,            // no right-hand side
			`prod-nomad`,                        // no operand at all
			`${node.unique.name} ~= prod-nomad`, // an operand Nomad does not accept
		} {
			_, err := parseNodeConstraints(raw)
			if err == nil || !strings.Contains(err.Error(), envNodeConstraint) {
				t.Fatalf("parseNodeConstraints(%q) error = %v, want one naming %s", raw, err, envNodeConstraint)
			}
		}
	})
}
