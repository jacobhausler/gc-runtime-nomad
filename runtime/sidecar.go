package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// binding is the sidecar's record of a session's current child job (04 §1
// "sidecar state dir": provider-owned KV under the city's private dir,
// stable handles — current-child job ID per session, plus the launched
// marker (04 §1/§6: the provision-vs-launched disambiguator). The fuller
// record (dispatch-attempt counter, disputed ledger, staleness datum, ...)
// is still out of scope here (staging lands it later).
type binding struct {
	SessionName string    `json:"session_name"`
	ChildJobID  string    `json:"child_job_id"`
	Namespace   string    `json:"namespace"`
	Nonce       string    `json:"nonce"`
	CreatedAt   time.Time `json:"created_at"`

	// EgressComplete receipts that the stop-path transcript/evidence fs
	// egress (NRT-P1-07) finished for this binding's current child job
	// before deregister was called. Written to the sidecar (still resident
	// at that point) so a stop that crashes after egress but before
	// deregister does not re-copy files on retry.
	EgressComplete bool `json:"egress_complete,omitempty"`

	// EvidenceLost marks that required stop-path evidence was unavailable
	// after bounded retries and stop proceeded anyway (04 §6 R2b-04:
	// evidence-best-effort beats a wedged fleet). This covers both local
	// egress and the optional log-shipper flush, so it may coexist with
	// EgressComplete when the local bundle was captured but live logs could
	// not be flushed.
	EvidenceLost bool `json:"evidence_lost,omitempty"`

	// StagePending marks that this binding's box was dispatched and its
	// NRT-P1-06 staging (workspace/secrets materialization) was started but
	// never confirmed complete. It is the provision-path analog of
	// dispatch's own intent record: written before the first tar-over-exec
	// call and cleared once every staging call has succeeded, so a binding
	// left by a provision that died mid-staging (the lab's mid-stdin exec
	// half-close, fnrt-t4l.18) says so on its face. Provision's retry no
	// longer needs to consult it — adoption keys off liveChild, which
	// covers every live-but-unlaunched box, not just a half-staged one
	// (fnrt-t4l.46) — so this stays as the durable record of which staging
	// halves never finished. Absent on bindings written before this field
	// existed.
	StagePending bool `json:"stage_pending,omitempty"`

	// Launched distinguishes "provisioned, agent never launched" (false)
	// from "launched" (true) — the two states that are otherwise
	// observationally identical from the Nomad alloc alone (04 §6:
	// RPP-PROVISION-001).
	Launched bool `json:"launched"`

	// AgentPID is the in-box pid of the tmux pane launch created, captured
	// right after buildLaunchCommand succeeds (markLaunched). It lets
	// opIsRunning's liveness probe kill -0 the exact process launch
	// started, not just the tmux session wrapping it — a session that
	// survives with its pane process gone is still an agent-dead answer
	// (04 §3 provision row, in-box agent kill). Zero for bindings recorded
	// before this field existed; the probe falls back to a tmux-session-only
	// check in that case.
	AgentPID int `json:"agent_pid,omitempty"`

	// WorkDir is the pane working directory the launch was given (cr-gdeav.6.10
	// deliverable 3). gc's cfg.WorkDir is a Mac path — the rig's checkout on the
	// supervisor host — which does not exist inside the alloc, so the pack
	// records what it actually used and relaunch/transport-launch reuse it
	// rather than falling back to the exec driver's cwd ("/").
	WorkDir string `json:"work_dir,omitempty"`

	// PromptedAt receipts the moment the startup prompt was actually typed
	// into the pane (cr-gdeav.6.10). It is what makes the delivery exactly-once
	// across processes: gc execs this binary fresh per op, so a retried
	// `start`, an adopt-reuse, and a relaunch are three separate chances to
	// type the same prompt twice — only the sidecar outlives them. Absent
	// means never delivered; see PromptStatus for which failure that was.
	PromptedAt *time.Time `json:"prompted_at,omitempty"`

	// PromptStatus is the last startup-priming outcome: "delivered",
	// "not_ready" (the wait budget ran out with the agent never showing its
	// prompt), or "nudge_failed" (ready, but typing into the pane errored).
	// It exists because gc's GC_STARTUP_PROMPT_DELIVERED env marker reports
	// that the prompt was ROUTED to a delivery mechanism
	// (cmd/gc/prompt_delivery.go:49), not that anything reached the agent —
	// for an RPP lane those two were indistinguishable, which is how
	// cr-gdeav.6.9's canary read as primed for eight minutes while sitting
	// at an empty prompt.
	PromptStatus string `json:"prompt_status,omitempty"`

	// PromptPending carries the startup-prompt/priming subset of the
	// stageConfig gc sent at provision/start time forward to whichever launch
	// path actually runs (cr-gdeav.6.12). It exists because gc's
	// proc.provision path stages the prompt fields separately from the
	// launch it later drives through opExec's transport-launch handshake
	// (markTransportLaunched, ops.go), which carries no stageConfig of its
	// own — gc's canonical tmux launch argv is only ever the transport
	// verb. Nil means no startup prompt was ever staged for this session;
	// startupPrime treats that identically to a zero-value stageConfig
	// (nothing owed). Only the prompt/priming subset is ever stored here —
	// never Env or Files — so this never re-carries a secret past the
	// channel stage() already routes them through.
	PromptPending *stageConfig `json:"prompt_pending,omitempty"`
}

// sidecar is a file-based KV under a directory: one JSON file per session,
// written temp+rename (04 §1 "temp+rename — e1a §4.5 privatedir contract").
type sidecar struct {
	dir string
}

func newSidecar(dir string) (*sidecar, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("nomad sidecar directory is required (set %s)", envSidecarDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating nomad sidecar directory: %w", err)
	}
	return &sidecar{dir: dir}, nil
}

func (s *sidecar) path(sessionName string) string {
	// base64url-encode the session name so arbitrary session-name
	// characters (nothing GC-side guarantees filesystem-safety) never
	// collide or escape the sidecar directory.
	return filepath.Join(s.dir, base64.RawURLEncoding.EncodeToString([]byte(sessionName))+".json")
}

func (s *sidecar) load(sessionName string) (*binding, bool, error) {
	data, err := os.ReadFile(s.path(sessionName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading sidecar binding for %q: %w", sessionName, err)
	}
	var b binding
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, false, fmt.Errorf("decoding sidecar binding for %q: %w", sessionName, err)
	}
	return &b, true, nil
}

func (s *sidecar) save(b binding) error {
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("encoding sidecar binding for %q: %w", b.SessionName, err)
	}
	target := s.path(b.SessionName)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing sidecar binding for %q: %w", b.SessionName, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("committing sidecar binding for %q: %w", b.SessionName, err)
	}
	return nil
}

// remove tombstones a session's binding. Idempotent: a missing binding is
// success (matches the ops' idempotent-stop contract).
func (s *sidecar) remove(sessionName string) error {
	err := os.Remove(s.path(sessionName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing sidecar binding for %q: %w", sessionName, err)
	}
	return nil
}

// list returns every current binding. opListRunning uses it only for the
// launched marker (04 §6 RPP-PROVISION-001) — existence and non-terminal
// status come from the children-of-parent jobs list (04 §2.1 rule 2/3) via
// client.listChildJobs, not this sidecar scan. dispatch's narrower
// positive-attribution adoption (rule 6: recovering a SINGLE session's own
// orphaned child by nonce match, in ops.go) uses the same client call.
func (s *sidecar) list() ([]binding, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("listing sidecar directory: %w", err)
	}
	var out []binding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // removed between ReadDir and ReadFile
			}
			return nil, fmt.Errorf("reading sidecar binding %q: %w", e.Name(), err)
		}
		var b binding
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, fmt.Errorf("decoding sidecar binding %q: %w", e.Name(), err)
		}
		out = append(out, b)
	}
	return out, nil
}

// tombstonesDirName is the subdirectory of the sidecar dir that keeps reap
// records. sidecar.list skips directories, so a tombstone can never be read
// back as a current binding — the record survives, the box does not.
const tombstonesDirName = "tombstones"

// tombstone is the durable record of a session box this pack deregistered
// without being asked to — the adoption gate's receipt (cr-gdeav.6.1,
// executive ruling cr-mnl96 G2). The archival policy governs records, not
// ephemeral runtime allocs, so the alloc is what gets removed and this entry
// (child job, alloc, age, why, when) is what gets kept, written beside the
// binding it describes before the stop removes that binding.
type tombstone struct {
	SessionName string    `json:"session_name"`
	ChildJobID  string    `json:"child_job_id"`
	AllocID     string    `json:"alloc_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// Age is how long the box had been resident when it was reaped, as a Go
	// duration string, so a reader never has to subtract two timestamps to
	// check it against GC_NOMAD_ADOPT_MAX_AGE.
	Age      string `json:"age"`
	Launched bool   `json:"launched"`
	AgentPID int    `json:"agent_pid,omitempty"`
	// Reason is the gate's own words for why this box was refused
	// (age limit exceeded / agent absent), same string the decision log line
	// carries.
	Reason       string    `json:"reason"`
	TombstonedAt time.Time `json:"tombstoned_at"`
}

// tombstone writes one reap record under the sidecar dir. temp+rename like
// every other sidecar write; the name is session+timestamp so repeated reaps
// of one session name accumulate a history instead of overwriting each other.
func (s *sidecar) tombstone(t tombstone) error {
	dir := filepath.Join(s.dir, tombstonesDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating tombstone directory: %w", err)
	}
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encoding tombstone for %q: %w", t.SessionName, err)
	}
	target := filepath.Join(dir, fmt.Sprintf("%s-%d.json",
		base64.RawURLEncoding.EncodeToString([]byte(t.SessionName)), t.TombstonedAt.UTC().UnixNano()))
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing tombstone for %q: %w", t.SessionName, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("committing tombstone for %q: %w", t.SessionName, err)
	}
	return nil
}

// newNonce generates the per-dispatch attribution nonce (04 §2.1: dispatch
// Meta carries gc_session + a random nonce chosen by the pack, never a
// capability).
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating dispatch nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
