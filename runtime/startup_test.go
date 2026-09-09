package main

// Startup prompt delivery tests (cr-gdeav.6.10).
//
// Cross-boundary tier: these tests carry gc's OWN wire encoding
// (internal/runtime/exec/json.go's startConfig field names) through this
// pack's decoder and then ask the pack what it owes the pane — without
// execing tmux, the Nomad API, or an allocation. The builder image has no
// tmux at all (same constraint TestLaunchCommandPaneSourcesStagedEnv records),
// and the property under test — the prompt is typed ONCE, and NEVER before
// the agent is at its prompt — cannot be observed on a live pane anyway: a
// live pane is only ever already-ready or not-yet-ready, so the ordering half
// needs the probe injected. The sidecar/binding half underneath stays real.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// gcLaunchWire is the `start` stdin body as gc's marshalStartConfig
// (internal/runtime/exec/json.go:45-67) serializes it for a
// `prompt_mode = "none"` provider — the shape that carries the startup prompt
// in nudge. Written out in gc's field names on purpose: this pack cannot
// import gc, so a rename upstream must land here as a failure.
const gcLaunchWire = `{
  "work_dir": "/Users/home/westlands/work/fable-nomad",
  "command": "/bin/sh -c '. \"$NOMAD_SECRETS_DIR/env.sh\"; . /mnt/nomad/gc/agent-launch.sh'",
  "lifecycle": "one_shot",
  "env": {"GC_CITY_URL": "https://192.168.0.107:8443"},
  "process_names": ["codex"],
  "nudge": "Run gc hook --claim --json now.",
  "ready_prompt_prefix": "Ask Codex",
  "ready_delay_ms": 5000,
  "pre_start": ["mkdir -p /mnt/nomad/gc"],
  "session_setup": ["git -C /mnt/nomad/gc status -sb"],
  "session_setup_script": "/tmp/session-setup.sh",
  "session_live": ["gc runtime heartbeat"],
  "pack_overlay_dirs": ["/Users/home/.gc/packs/gc-runtime-nomad/overlay"],
  "overlay_dir": "/tmp/session-overlay",
  "copy_files": [{"src": "/tmp/x", "rel_dst": "x"}]
}`

// gcStartWireFields is gc's complete `start` stdin schema: the JSON keys
// marshalStartConfig (internal/runtime/exec/json.go:45-67 at pin 857ed398a)
// serializes, written out here because this pack cannot import gc.
// gcLaunchWire above carries one of each type, and the test below asserts the
// two directions against this list.
var gcStartWireFields = []string{
	"work_dir", "command", "lifecycle", "env", "process_names", "nudge",
	"ready_prompt_prefix", "ready_delay_ms", "pre_start", "session_setup",
	"session_setup_script", "session_live", "pack_overlay_dirs",
	"overlay_dir", "copy_files",
}

// TestStageConfigCarriesEveryGcLaunchWireField pins the wire schema in BOTH
// directions, and it started life as the RED half of the drop cr-gdeav.6.9
// receipt §1.4 named: the pack decoded only work_dir/env/files, so gc's
// command, nudge, process_names and ready_* arrived and were thrown away.
//
// The second half (cr-u7b0s.40) is the one that keeps a private schema from
// growing back inside the wire: a key this pack decodes that gc's
// marshalStartConfig does not serialize is a field nothing will ever send,
// and `files`, `prompt_suffix` and `prompt_flag` were exactly that. It
// asserts on tags rather than fields so it compiles (and fails) against any
// pack whose struct disagrees with the pin.
func TestStageConfigCarriesEveryGcLaunchWireField(t *testing.T) {
	typ := reflect.TypeOf(stageConfig{})
	have := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" || name == "" {
			t.Errorf("field %s carries no json tag, so it cannot match the wire", typ.Field(i).Name)
			continue
		}
		if have[name] {
			t.Errorf("two fields decode json key %q", name)
		}
		have[name] = true
	}
	for _, field := range gcStartWireFields {
		if !have[field] {
			t.Errorf("stageConfig drops the %q field gc sends on the start wire", field)
		}
	}
	if len(have) != len(gcStartWireFields) {
		for field := range have {
			if !contains(gcStartWireFields, field) {
				t.Errorf("stageConfig decodes %q, which gc's marshalStartConfig never serializes", field)
			}
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// TestReadStageConfigDecodesGcLaunchWire proves the wire reaches Go, not just
// the struct definition.
func TestReadStageConfigDecodesGcLaunchWire(t *testing.T) {
	cfg, err := readStageConfig(strings.NewReader(gcLaunchWire))
	if err != nil {
		t.Fatalf("readStageConfig: %v", err)
	}
	if cfg.WorkDir != "/Users/home/westlands/work/fable-nomad" {
		t.Errorf("WorkDir = %q", cfg.WorkDir)
	}
	if !strings.Contains(cfg.Command, "agent-launch.sh") {
		t.Errorf("Command = %q, want the deployment bootstrap gc routes through", cfg.Command)
	}
	if cfg.Nudge != "Run gc hook --claim --json now." {
		t.Errorf("Nudge = %q", cfg.Nudge)
	}
	if cfg.ReadyPromptPrefix != "Ask Codex" || cfg.ReadyDelayMs != 5000 {
		t.Errorf("readiness hints = %q / %d", cfg.ReadyPromptPrefix, cfg.ReadyDelayMs)
	}
	if len(cfg.ProcessNames) != 1 || cfg.ProcessNames[0] != "codex" {
		t.Errorf("ProcessNames = %v", cfg.ProcessNames)
	}
	if cfg.Lifecycle != "one_shot" {
		t.Errorf("Lifecycle = %q, want gc's one_shot", cfg.Lifecycle)
	}
	if len(cfg.CopyFiles) != 1 || cfg.CopyFiles[0].Src != "/tmp/x" || cfg.CopyFiles[0].RelDst != "x" {
		t.Errorf("CopyFiles = %+v, want the one {src, rel_dst} entry gc sent", cfg.CopyFiles)
	}
	for name, got := range map[string][]string{
		"pre_start":         cfg.PreStart,
		"session_setup":     cfg.SessionSetup,
		"session_live":      cfg.SessionLive,
		"pack_overlay_dirs": cfg.PackOverlayDirs,
	} {
		if len(got) != 1 || got[0] == "" {
			t.Errorf("%s = %v, want the one entry gc sent", name, got)
		}
	}
	if cfg.SessionSetupScript != "/tmp/session-setup.sh" {
		t.Errorf("SessionSetupScript = %q", cfg.SessionSetupScript)
	}
	if cfg.OverlayDir != "/tmp/session-overlay" {
		t.Errorf("OverlayDir = %q", cfg.OverlayDir)
	}
	text, source := cfg.startupPrompt()
	if text != cfg.Nudge || source != "nudge" {
		t.Errorf("startupPrompt() = (%q, %q), want the nudge text from source nudge", text, source)
	}
}

// TestStartupPromptIgnoresKeysGcDoesNotSerialize is the other half of the
// cr-u7b0s.40 deletion: prompt_suffix / prompt_flag exist on runtime.Config
// and the native tmux adapter consumes them in-process, but gc's
// marshalStartConfig never puts either on the exec wire - so a caller that
// sends them must NOT get a prompt out of them. `nudge` is the pack's only
// prompt source, which is precisely why the provider runs
// `prompt_mode = "none"`. (If a pinned-EDGE wire change ever lands the key,
// this test is the place the decision has to be revisited, not a field
// quietly re-added.)
func TestStartupPromptIgnoresKeysGcDoesNotSerialize(t *testing.T) {
	cfg, err := readStageConfig(strings.NewReader(`{"prompt_suffix": "Do the work.", "prompt_flag": "-c", "files": [{"path": "x", "content": "eA=="}]}`))
	if err != nil {
		t.Fatalf("readStageConfig: %v", err)
	}
	if text, source := cfg.startupPrompt(); text != "" || source != "" {
		t.Errorf("startupPrompt() = (%q, %q), want no prompt from keys gc never sends", text, source)
	}
	if !reflect.DeepEqual(cfg, stageConfig{}) {
		t.Errorf("a wire body of keys gc does not serialize decoded to %+v, want the zero value", cfg)
	}
}

func TestUnshellQuoteRoundTripsShellQuote(t *testing.T) {
	for _, value := range []string{"simple", "two words", "it's", "a'b'c", "run;\necho $(date)", ""} {
		if got := unshellQuote(shellQuote(value)); got != value {
			t.Errorf("unshellQuote(shellQuote(%q)) = %q", value, got)
		}
	}
	// Outside the grammar: returned verbatim rather than guessed at.
	for _, value := range []string{"", "unquoted", "'unbalanced"} {
		if got := unshellQuote(value); got != value {
			t.Errorf("unshellQuote(%q) = %q, want it unchanged", value, got)
		}
	}
}

// primingRecorder is the injected transport for the tests below: it scripts
// the pane readiness answers, records the ORDER of every probe and nudge, and
// advances a virtual clock on each wait so no test sleeps.
type primingRecorder struct {
	events      []string
	paneAnswers []bool // consumed in order; the last answer repeats
	clock       time.Time
	probes      int
	nudges      []string
	nudgeErr    error
	waits       []time.Duration
}

func (r *primingRecorder) io() startupIO {
	return startupIO{
		alloc: func(context.Context, string) (string, error) { return "alloc-1", nil },
		pane: func(context.Context, string, int) (bool, string, error) {
			idx := r.probes
			if idx >= len(r.paneAnswers) {
				idx = len(r.paneAnswers) - 1
			}
			r.probes++
			ready := r.paneAnswers[idx]
			r.events = append(r.events, "probe")
			if ready {
				return true, "Ask Codex to do anything", nil
			}
			return true, "", nil
		},
		nudge: func(_ context.Context, _, text string) error {
			r.nudges = append(r.nudges, text)
			r.events = append(r.events, "nudge")
			return r.nudgeErr
		},
		wait: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			r.waits = append(r.waits, d)
			r.clock = r.clock.Add(d)
			return nil
		},
		now:      func() time.Time { return r.clock },
		interval: time.Second,
		budget:   5 * time.Second,
	}
}

func readyBinding(t *testing.T, sc *sidecar, name string, mutate func(*binding)) *binding {
	t.Helper()
	b := binding{SessionName: name, ChildJobID: "child-1", Namespace: "default", Launched: true, AgentPID: 4242}
	if mutate != nil {
		mutate(&b)
	}
	if err := sc.save(b); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	return &b
}

func startupTestSidecar(t *testing.T) *sidecar {
	t.Helper()
	sc, err := newSidecar(t.TempDir())
	if err != nil {
		t.Fatalf("newSidecar: %v", err)
	}
	return sc
}

func quietLog(*testing.T) func(string, ...any) {
	return func(string, ...any) {}
}

// TestStartupPrimeTypesPromptOnceAfterReadiness is the bead's RED/GREEN
// cross-boundary proof: nudge never precedes readiness, and it happens exactly
// once, with the receipt written where the next process can see it.
func TestStartupPrimeTypesPromptOnceAfterReadiness(t *testing.T) {
	sc := startupTestSidecar(t)
	const name = "sess-prime"
	readyBinding(t, sc, name, nil)

	rec := &primingRecorder{clock: time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC), paneAnswers: []bool{false, false, true}}
	cfg := stageConfig{Nudge: "Run gc hook --claim --json now.", ReadyPromptPrefix: "Ask Codex"}

	if err := startupPrime(context.Background(), sc, rec.io(), quietLog(t), name, cfg); err != nil {
		t.Fatalf("startupPrime: %v", err)
	}

	lastProbe, nudges := -1, 0
	for i, event := range rec.events {
		if event == "probe" {
			lastProbe = i
		}
		if event == "nudge" {
			nudges++
			if i < lastProbe {
				t.Fatalf("event order %v: the prompt was typed before a readiness probe", rec.events)
			}
		}
	}
	if nudges != 1 {
		t.Fatalf("nudge calls = %d, want exactly 1 (events %v)", nudges, rec.events)
	}
	if rec.probes < 3 {
		t.Fatalf("probes = %d, want the not-ready answers to have been waited out", rec.probes)
	}
	for i, event := range rec.events {
		if event == "nudge" && i != len(rec.events)-1 {
			t.Fatalf("event order %v: a probe followed the nudge", rec.events)
		}
	}
	if len(rec.nudges) != 1 || rec.nudges[0] != cfg.Nudge {
		t.Fatalf("nudged %q, want the wire prompt text %q", rec.nudges, cfg.Nudge)
	}

	got, ok, err := sc.load(name)
	if err != nil || !ok {
		t.Fatalf("load: %v %v", err, ok)
	}
	if got.PromptedAt == nil {
		t.Fatal("the sidecar carries no prompted_at receipt")
	}
	if got.PromptStatus != promptStatusDelivered {
		t.Fatalf("prompt_status = %q, want %q", got.PromptStatus, promptStatusDelivered)
	}
	if !got.Launched || got.AgentPID != 4242 {
		t.Fatalf("priming rewrote the launch record: %+v", got)
	}
}

// TestStartupPrimeRunsExactlyOncePerSession is the idempotence half: gc execs
// this binary fresh per op, so a retried start, an adopt-reuse and a relaunch
// are three separate chances to type the same prompt twice.
func TestStartupPrimeRunsExactlyOncePerSession(t *testing.T) {
	sc := startupTestSidecar(t)
	const name = "sess-twice"
	stamped := time.Date(2026, 9, 7, 0, 59, 0, 0, time.UTC)
	readyBinding(t, sc, name, func(b *binding) {
		b.PromptedAt = &stamped
		b.PromptStatus = promptStatusDelivered
	})

	rec := &primingRecorder{clock: stamped.Add(time.Hour), paneAnswers: []bool{true}}
	if err := startupPrime(context.Background(), sc, rec.io(), quietLog(t), name, stageConfig{Nudge: "again"}); err != nil {
		t.Fatalf("startupPrime: %v", err)
	}
	if rec.probes != 0 || len(rec.nudges) != 0 {
		t.Fatalf("an already-primed session was probed %d times and nudged %d times", rec.probes, len(rec.nudges))
	}
}

// TestStartupPrimeWithoutPromptTextTouchesNothing: prompt_mode arg/flag (or no
// startup prompt at all) means nothing is owed, and a session that owes
// nothing must not gain a prompt_status it never earned.
func TestStartupPrimeWithoutPromptTextTouchesNothing(t *testing.T) {
	sc := startupTestSidecar(t)
	const name = "sess-unowed"
	readyBinding(t, sc, name, nil)

	rec := &primingRecorder{clock: time.Now(), paneAnswers: []bool{true}}
	if err := startupPrime(context.Background(), sc, rec.io(), quietLog(t), name, stageConfig{Command: "codex"}); err != nil {
		t.Fatalf("startupPrime: %v", err)
	}
	if rec.probes != 0 || len(rec.nudges) != 0 {
		t.Fatalf("a session owed no prompt was probed %d times and nudged %d times", rec.probes, len(rec.nudges))
	}
	got, _, err := sc.load(name)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PromptStatus != "" || got.PromptedAt != nil {
		t.Fatalf("unowed session gained prompt state: %+v", got)
	}
}

// TestStartupPrimeBudgetExhaustedRecordsNotReady: readiness that never
// arrives must not wedge start and must not type into an agent that is not
// there — but it must also not read as success afterwards.
func TestStartupPrimeBudgetExhaustedRecordsNotReady(t *testing.T) {
	sc := startupTestSidecar(t)
	const name = "sess-never-ready"
	readyBinding(t, sc, name, nil)

	rec := &primingRecorder{clock: time.Now(), paneAnswers: []bool{false}}
	if err := startupPrime(context.Background(), sc, rec.io(), quietLog(t), name, stageConfig{Nudge: "hello?", ReadyPromptPrefix: "Ask Codex"}); err != nil {
		t.Fatalf("startupPrime must not fail start: %v", err)
	}
	if len(rec.nudges) != 0 {
		t.Fatalf("typed %q into a pane that never showed its prompt", rec.nudges)
	}
	if rec.probes < 2 {
		t.Fatalf("probes = %d, want the budget polled, not skipped", rec.probes)
	}
	got, _, err := sc.load(name)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PromptedAt != nil || got.PromptStatus != promptStatusNotReady {
		t.Fatalf("binding = %+v, want prompt_status=%q and no prompted_at", got, promptStatusNotReady)
	}
}

// TestStartupPrimeNudgeFailureIsNonFatalAndRecorded: the pane was ready and
// tmux errored. gc's own providers treat the post-launch nudge as
// best-effort; this pack may swallow the error only if it leaves a record.
func TestStartupPrimeNudgeFailureIsNonFatalAndRecorded(t *testing.T) {
	sc := startupTestSidecar(t)
	const name = "sess-nudge-fail"
	readyBinding(t, sc, name, nil)

	rec := &primingRecorder{clock: time.Now(), paneAnswers: []bool{true}, nudgeErr: errors.New("tmux send-keys exited 1")}
	if err := startupPrime(context.Background(), sc, rec.io(), quietLog(t), name, stageConfig{Nudge: "hello"}); err != nil {
		t.Fatalf("startupPrime must not fail start: %v", err)
	}
	got, _, err := sc.load(name)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PromptedAt != nil || got.PromptStatus != promptStatusNudgeFailed {
		t.Fatalf("binding = %+v, want prompt_status=%q", got, promptStatusNudgeFailed)
	}
}

// TestStartupPrimeHonoursReadyDelay: gc's ready_delay_ms hint is honored as
// the fixed-delay fallback (the same fallback its own adapter takes), and it
// is clamped to the wait budget rather than extending it.
func TestStartupPrimeHonoursReadyDelay(t *testing.T) {
	sc := startupTestSidecar(t)
	const name = "sess-delay"
	readyBinding(t, sc, name, nil)

	rec := &primingRecorder{clock: time.Now(), paneAnswers: []bool{true}}
	cfg := stageConfig{Nudge: "hi", ReadyDelayMs: 1000}
	if err := startupPrime(context.Background(), sc, rec.io(), quietLog(t), name, cfg); err != nil {
		t.Fatalf("startupPrime: %v", err)
	}
	if len(rec.waits) == 0 || rec.waits[0] != time.Second {
		t.Fatalf("waits = %v, want the 1000ms ready_delay_ms honored first", rec.waits)
	}

	const clampedName = "sess-delay-clamped"
	readyBinding(t, sc, clampedName, nil)
	rec2 := &primingRecorder{clock: time.Now(), paneAnswers: []bool{true}}
	cfg2 := stageConfig{Nudge: "hi", ReadyDelayMs: 999999}
	if err := startupPrime(context.Background(), sc, rec2.io(), quietLog(t), clampedName, cfg2); err != nil {
		t.Fatalf("startupPrime: %v", err)
	}
	if len(rec2.waits) == 0 {
		t.Fatal("the clamped ready_delay_ms was skipped entirely")
	}
	if rec2.waits[0] > rec2.io().budget {
		t.Fatalf("ready_delay_ms escaped the wait budget: %v > %v", rec2.waits[0], rec2.io().budget)
	}
	if rec2.waits[0] != rec2.io().budget {
		t.Fatalf("ready_delay_ms = %v, want it clamped to the budget %v", rec2.waits[0], rec2.io().budget)
	}
}

// TestPaneShowsReadyPrompt pins the gc-parity matching rules
// (internal/runtime/tmux/tmux.go's matchesPromptPrefix) the pack had to
// reimplement because it cannot import gc.
func TestPaneShowsReadyPrompt(t *testing.T) {
	cases := []struct {
		name   string
		pane   string
		prefix string
		want   bool
	}{
		{"no prefix configured gates on liveness alone", "anything at all", "", true},
		{"prompt line present", "some banner\nAsk Codex to do anything\n", "Ask Codex", true},
		{"non-breaking spaces normalize", "  Ask Codex ", "Ask Codex", true},
		{"box-drawn input line", "╭─────────╮\n│ Ask Codex to do anything\n", "Ask Codex", true},
		{"exact trimmed match", "  Ask Codex  ", "Ask Codex", true},
		{"booting pane", "", "Ask Codex", false},
		{"unrelated pane", "codex is thinking really hard", "Ask Codex", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsReadyPrompt(tc.pane, tc.prefix); got != tc.want {
				t.Fatalf("paneShowsReadyPrompt(%q, %q) = %t, want %t", tc.pane, tc.prefix, got, tc.want)
			}
		})
	}
}
