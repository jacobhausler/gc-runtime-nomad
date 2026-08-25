package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lifecycle implements the RPP ops this pack scopes so far: the four
// dispatch/deregister/blocking-read lifecycle ops (start, stop, is-running,
// list-running, NRT-P1-03), plus the provision/launch split and warm
// relaunch (provision, exec, relaunch, NRT-P1-08) — using the sidecar for
// the session_name -> child-job-ID binding (04 §2.1: "sidecar binding is
// primary") and its launched marker (04 §1/§6). Staging (workspace/secrets
// data contracts, 04 §5) is still out of scope.
type lifecycle struct {
	client      *client
	sidecar     *sidecar
	parentJobID string

	// egressDir is the local sink directory for the stop-path transcript/
	// evidence egress (NRT-P1-07). Empty disables egress entirely — a
	// deployment that never sets GC_NOMAD_EGRESS_DIR gets the pre-egress
	// stop behavior unchanged. Sinks beyond a local directory (remote
	// upload, etc.) are out of scope for this bead.
	egressDir string
}

// egressFile is one well-known file a stop-path egress reads out of every
// allocation before its job is deregistered. Discovery beyond this fixed
// pair (e.g. a directory listing) is out of scope for this bead.
var egressFiles = []struct {
	allocPath string
	destName  string
}{
	{"alloc/logs/transcript.log", "transcript.log"},
	{"alloc/data/evidence.json", "evidence.json"},
}

// egressAllocFiles copies childJobID's per-allocation transcript/evidence
// files into l.egressDir/<session>/ via the client fs API. It runs before
// opStop's deregister call, since deregister is what makes those files
// unreachable. A no-op when egress is disabled (l.egressDir == "").
func (l *lifecycle) egressAllocFiles(ctx context.Context, sessionName, childJobID string) error {
	if l.egressDir == "" {
		return nil
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, childJobID, 0, 0)
	if err != nil {
		return fmt.Errorf("egress: listing allocations for %q: %w", childJobID, err)
	}

	destDir := filepath.Join(l.egressDir, base64.RawURLEncoding.EncodeToString([]byte(sessionName)))
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("egress: creating sink directory: %w", err)
	}

	for _, a := range allocs {
		for _, f := range egressFiles {
			data, err := l.client.readAllocFile(ctx, a.ID, f.allocPath)
			if errors.Is(err, errAllocFileGone) {
				continue
			}
			if err != nil {
				return fmt.Errorf("egress: reading %s for alloc %q: %w", f.allocPath, a.ID, err)
			}
			dest := filepath.Join(destDir, a.ID+"-"+f.destName)
			tmp := dest + ".tmp"
			if err := os.WriteFile(tmp, data, 0o600); err != nil {
				return fmt.Errorf("egress: writing %s: %w", dest, err)
			}
			if err := os.Rename(tmp, dest); err != nil {
				return fmt.Errorf("egress: committing %s: %w", dest, err)
			}
		}
	}
	return nil
}

// execTaskName is the name of the single task in sessionTaskGroup (jobspec.go)
// -- every alloc-exec call targets it.
const execTaskName = "agent"

// tmuxSessionName is the wire-contract constant in-box tmux session name
// (04 §5 R1a-08/-09): every carrier verb and the launch command target
// exactly this session.
const tmuxSessionName = "main"

// launchCommand is the detached tmux-client command that turns a
// provisioned (tmux-only) box into a launched one (04 §3 provision row +
// R1c-04 launch invariant): it returns immediately, and the agent it starts
// is parented to the tmux server inside the task cgroup, never to the exec
// session that issued it. Building the real agent bootstrap command that
// replaces the placeholder session below is staging work, out of scope
// here.
var launchCommand = []string{"tmux", "new-session", "-d", "-s", tmuxSessionName}

// isTerminalStatus reports whether a Nomad alloc ClientStatus is terminal.
func isTerminalStatus(status string) bool {
	switch status {
	case "complete", "failed", "lost":
		return true
	default:
		return false
	}
}

// opProvision dispatches a new child job for sessionName and stops there —
// no launch. The dispatched task runs a tmux-only supervisor (jobspec.go's
// sessionTask); no agent process exists in the box yet, and the sidecar
// binding's launched marker stays false, so is-running reports false even
// though the box already answers exec (04 §3 provision row, RPP-PROVISION-001).
// A session with a live (non-terminal) child already bound is rejected —
// the stderr message MUST contain "already exists" (04 §6 wire-contract
// constant, R1a-02: gc's exec proxy infers ErrSessionExists from that exact
// phrase).
func (l *lifecycle) opProvision(ctx context.Context, sessionName string) error {
	return l.dispatch(ctx, sessionName)
}

// opStart is provision + launch (04 §3 start row): dispatch a fresh child,
// then launch the agent into it over exec, and only then mark the binding
// launched.
func (l *lifecycle) opStart(ctx context.Context, sessionName string) error {
	if err := l.dispatch(ctx, sessionName); err != nil {
		return err
	}
	return l.markLaunched(ctx, sessionName)
}

// opRelaunch re-execs the agent inside the SAME alloc without a fresh
// dispatch — the warm-box mechanism for launch-only fingerprint drift (04
// §7 Relaunch): tmux-respawn analog via exec kill+relaunch, no re-dispatch.
// It requires an existing provisioned box with a live alloc; a session with
// nothing running has nothing to relaunch into.
func (l *lifecycle) opRelaunch(ctx context.Context, sessionName string) error {
	if _, err := l.currentAlloc(ctx, sessionName); err != nil {
		return fmt.Errorf("relaunching session %q: %w", sessionName, err)
	}
	return l.markLaunched(ctx, sessionName)
}

// opExec runs command inside sessionName's current alloc over the Nomad
// alloc-exec WebSocket (04 §3 exec row: "op exit = command exit",
// RPP-CONN-001). It works regardless of the launched marker — this is
// exactly the "box exec-able" half of the provision contract
// (RPP-PROVISION-001): a provisioned-but-not-launched box still answers
// exec.
func (l *lifecycle) opExec(ctx context.Context, sessionName string, command []string) (int, []byte, error) {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return 0, nil, err
	}
	exitCode, stdout, err := l.client.execAlloc(ctx, allocID, execTaskName, command)
	if err != nil {
		return 0, nil, fmt.Errorf("exec on session %q: %w", sessionName, err)
	}
	return exitCode, stdout, nil
}

// dispatch registers the parent job (idempotent upsert) if needed, then
// dispatches a tmux-only child for sessionName — the mechanism shared by
// opProvision directly and opStart (provision half).
func (l *lifecycle) dispatch(ctx context.Context, sessionName string) error {
	if existing, ok, err := l.sidecar.load(sessionName); err != nil {
		return err
	} else if ok && existing.ChildJobID != "" {
		allocs, _, err := l.client.listAllocsForJob(ctx, existing.ChildJobID, 0, 0)
		if err == nil {
			for _, a := range allocs {
				if !isTerminalStatus(a.ClientStatus) {
					return fmt.Errorf("session %q already exists", sessionName)
				}
			}
		}
		// A lookup error (unavailable/gone) is NOT proof of absence — but
		// wedging start behind an unreachable API is worse than risking a
		// duplicate here; the pre-dispatch child query a full cluster-side
		// implementation would add (04 §6 duplicate-start suppression) is
		// out of scope (provision split).
	}

	if err := l.client.registerJob(ctx, parentJobSpec(l.client.namespace, l.parentJobID)); err != nil {
		return fmt.Errorf("registering parent job: %w", err)
	}

	nonce, err := newNonce()
	if err != nil {
		return err
	}

	// Dispatch-intent record BEFORE the call (04 §2.1 rule 1): Nomad mints
	// the child ID in the dispatch response, so the binding cannot exist
	// before the call — this placeholder is what a crash between dispatch
	// and the binding write would otherwise lose.
	intent := binding{SessionName: sessionName, Namespace: l.client.namespace, Nonce: nonce, CreatedAt: time.Now().UTC()}
	if err := l.sidecar.save(intent); err != nil {
		return err
	}

	childID, err := l.client.dispatchChild(ctx, l.parentJobID, sessionName, nonce)
	if err != nil {
		return err
	}

	final := intent
	final.ChildJobID = childID
	return l.sidecar.save(final)
}

// launch execs launchCommand into sessionName's current alloc — the
// detached tmux-client call that starts the agent (04 §3 provision row
// launch invariant).
func (l *lifecycle) launch(ctx context.Context, sessionName string) error {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return err
	}
	exitCode, _, err := l.client.execAlloc(ctx, allocID, execTaskName, launchCommand)
	if err != nil {
		return fmt.Errorf("launching session %q: %w", sessionName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("launching session %q: launch command exited %d", sessionName, exitCode)
	}
	return nil
}

// markLaunched launches sessionName's agent and, only once that succeeds,
// records the launched marker in the sidecar binding — shared by opStart
// (provision then launch) and opRelaunch (launch only, no re-dispatch).
func (l *lifecycle) markLaunched(ctx context.Context, sessionName string) error {
	if err := l.launch(ctx, sessionName); err != nil {
		return err
	}
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before launch could be recorded", sessionName)
	}
	b.Launched = true
	return l.sidecar.save(*b)
}

// currentAlloc resolves the non-terminal Nomad alloc ID backing
// sessionName's current child job, sidecar-binding-primary (04 §2.1 rule 1).
func (l *lifecycle) currentAlloc(ctx context.Context, sessionName string) (string, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return "", err
	}
	if !ok || b.ChildJobID == "" {
		return "", fmt.Errorf("session %q has no provisioned box", sessionName)
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return "", fmt.Errorf("resolving alloc for session %q: %w", sessionName, err)
	}
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("session %q has no non-terminal alloc", sessionName)
}

// opStop egresses sessionName's transcript/evidence files (if egress is
// configured, NRT-P1-07), deregisters its child job without purge, confirms
// terminal via a blocking read, and tombstones the sidecar binding. Stop on
// a session with no binding (never started, or already stopped) is a
// success no-op — Stop stays idempotent (04 §6, E1a §6.1).
func (l *lifecycle) opStop(ctx context.Context, sessionName string) error {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if b.ChildJobID == "" {
		// Crashed between the pre-dispatch intent write and the dispatch
		// call: nothing was ever created cluster-side.
		return l.sidecar.remove(sessionName)
	}

	// Egress transcript/evidence before deregister makes them unreachable,
	// then receipt completion in the sidecar (still resident) so a stop
	// that crashes after egress but before deregister does not re-copy on
	// retry (NRT-P1-07).
	if !b.EgressComplete {
		if err := l.egressAllocFiles(ctx, sessionName, b.ChildJobID); err != nil {
			return err
		}
		b.EgressComplete = true
		if err := l.sidecar.save(*b); err != nil {
			return err
		}
	}

	idx, err := l.client.deregisterJob(ctx, b.ChildJobID, false)
	if err != nil {
		return err
	}
	if idx > 0 {
		// Confirm terminal via blocking read (04 §3 stop row ordering
		// invariant). fakenomad drives allocs terminal synchronously inside
		// deregister, so this resolves immediately rather than genuinely
		// blocking — it still exercises the blocking-read code path a real
		// cluster would need.
		_, _, _ = l.client.listAllocsForJob(ctx, b.ChildJobID, idx-1, blockingWait)
	}
	return l.sidecar.remove(sessionName)
}

// opIsRunning reports whether sessionName has a non-terminal child alloc
// AND is past the launched marker — a provisioned-but-not-launched box
// (alloc running, launched marker unset) answers false here even though it
// already answers exec (04 §6 decision table, RPP-PROVISION-001: the
// launched marker is what distinguishes "provisioned, agent never
// launched" from "launched, agent died"). Once launched, the per-op
// honesty split (04 §6) applies: API unavailability answers last-known-good
// (true, since a binding is still on record) rather than false — flipping
// to false here would read as confirmed death to gc's heal/quarantine
// ladders.
func (l *lifecycle) opIsRunning(ctx context.Context, sessionName string) (bool, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return false, err
	}
	if !ok || b.ChildJobID == "" || !b.Launched {
		return false, nil
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return true, nil
	}
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			return true, nil
		}
	}
	return false, nil
}

// opListRunning enumerates every launched, non-terminal session, per 04
// §2.1 rule 2/3: the children-of-parent jobs list (meta=true) is the source
// of existence — never the sidecar, which can drift stale — filtered to
// non-terminal children, decoded via each child's `gc_session` Meta key
// (never ID-string parsing, e2a-job-id-charset-gap), and then to prefix, if
// one was given (ListRunning(prefix), E2a amendment A-1). The sidecar is
// still consulted for exactly one thing the children list cannot answer:
// the launched marker (04 §6 RPP-PROVISION-001) — a provisioned-but-not-
// launched box is not "running", same gate as opIsRunning. On ANY lookup
// error it returns an error rather than a partial list — matching the
// "list-based reap arms defer on ANY error" contract (04 §6).
func (l *lifecycle) opListRunning(ctx context.Context, prefix string) ([]string, error) {
	children, err := l.client.listChildJobs(ctx, l.parentJobID)
	if err != nil {
		return nil, fmt.Errorf("list-running: listing children of %q: %w", l.parentJobID, err)
	}
	bindings, err := l.sidecar.list()
	if err != nil {
		return nil, err
	}
	launched := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if b.ChildJobID != "" && b.Launched {
			launched[b.ChildJobID] = true
		}
	}

	var names []string
	for _, c := range children {
		if c.Terminal || !launched[c.ID] {
			continue
		}
		name := c.Meta["gc_session"]
		if name == "" || (prefix != "" && !strings.HasPrefix(name, prefix)) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
