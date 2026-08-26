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
