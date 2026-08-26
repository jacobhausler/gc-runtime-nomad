package main

import (
	"encoding/json"
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
// to Leader (kill_timeout ordering), a pinned+checksummed vector artifact,
// an embedded vector.toml template, the three env-driven config values
// wired through, a group-local metrics port, and the shipper's KillTimeout
// outliving the agent's own (the bounded flush window).
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
		for _, want := range []string{
			logShipperPIDFile,
			logShipperFlushRequest,
			logShipperFlushComplete,
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
		`include = ["${HOME}/.claude/projects/**/*.jsonl"]`,
		`include = ["${NOMAD_ALLOC_DIR}/logs/agent.stdout.*"]`,
		`uri = "${GC_LOG_SINK}"`,
		`type = "prometheus_exporter"`,
		`address = "0.0.0.0:${NOMAD_PORT_metrics}"`,
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
