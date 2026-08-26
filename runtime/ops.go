package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sessionLockPollInterval is how often lockSession retries a contended
// session lock while waiting on ctx. Short enough that a start/provision
// call is not visibly delayed once the lock frees up; long enough not to
// spin.
const sessionLockPollInterval = 20 * time.Millisecond

// lifecycle implements the RPP ops this pack scopes so far: the four
// dispatch/deregister/blocking-read lifecycle ops (start, stop, is-running,
// list-running, NRT-P1-03), the provision/launch split and warm relaunch
// (provision, exec, relaunch, NRT-P1-08), and workspace/secret staging
// (stage, NRT-P1-06, 04 §5) — using the sidecar for the session_name ->
// child-job-ID binding (04 §2.1: "sidecar binding is primary") and its
// launched marker (04 §1/§6).
type lifecycle struct {
	client      *client
	sidecar     *sidecar
	parentJobID string

	// nodePool is the Nomad node pool the parent job registers into (empty
	// keeps Nomad's own "default" pool). Unlike namespace it carries no
	// per-request query param (04 §2.1's nsQuery has no node-pool analog on
	// any route this pack calls) — it only ever rides the parent job body
	// parentJobSpec builds, so it lives here rather than on client.
	nodePool string

	// egressDir is the local sink directory for the stop-path transcript/
	// evidence egress (NRT-P1-07). Empty disables egress entirely — a
	// deployment that never sets GC_NOMAD_EGRESS_DIR gets the pre-egress
	// stop behavior unchanged. Sinks beyond a local directory (remote
	// upload, etc.) are out of scope for this bead.
	egressDir string

	// forbidRegistration, when true, makes ensureParentRegistered fail
	// closed instead of ever attempting to register the parent job — for a
	// deployment whose runtime token deliberately lacks the submit-job
	// capability (04 §4 lab ACL model, NRT-P2-06.1) and wants a clear
	// config-time guarantee that dispatch never even probes that capability
	// boundary, trading a start failure with a precise cause for the
	// default behavior of attempting register and surfacing its 403.
	forbidRegistration bool
}

// lockSession acquires an exclusive, cross-process advisory file lock for
// sessionName under the sidecar directory — the single-flight lock
// concurrent same-name Start must go through (R2b-03). It has to be
// cross-process, not merely in-process: `gc` execs this binary fresh per
// op (main.go's calling convention, "gc execs the binary directly — no
// shell wrapping"), so two concurrent `start` calls for one session are
// two separate OS processes racing, never two goroutines sharing one
// `lifecycle` value. flock is associated with the open file description
// rather than the process, so it also serializes correctly within a
// single process (this pack's own in-process L2 fault-suite tests), and it
// is released automatically by the kernel when that descriptor closes —
// including on a crash — so there is no stale-lock cleanup to get wrong.
func (l *lifecycle) lockSession(ctx context.Context, sessionName string) (func(), error) {
	path := filepath.Join(l.sidecar.dir, base64.RawURLEncoding.EncodeToString([]byte(sessionName))+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening session lock for %q: %w", sessionName, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK {
			_ = f.Close()
			return nil, fmt.Errorf("locking session %q: %w", sessionName, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("locking session %q: %w", sessionName, ctx.Err())
		case <-time.After(sessionLockPollInterval):
		}
	}
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

// buildLaunchCommand is the detached tmux-client command that turns a
// provisioned (tmux-only) box into a launched one (04 §3 provision row +
// R1c-04 launch invariant): it returns immediately, and the agent it starts
// is parented to the tmux server inside the task cgroup, never to the exec
// session that issued it. The command itself is still a placeholder tmux
// session, not a real agent bootstrap command line — replacing it is out of
// scope here (jobspec.go's sessionTask has the same caveat).
//
// env's argvSafe-classified entries (envArgvSafe, NRT-P1-06) ride as `-e
// KEY=VALUE` on this argv — safe by construction, since envArgvSafe's whole
// job is excluding anything credential-shaped from ever reaching argv
// (E1a §4.5). Everything else in env was already routed to the secrets dir
// by stage before launch runs; it never appears here. Keys are sorted so
// the command is deterministic across calls with the same env.
func buildLaunchCommand(env map[string]string) []string {
	cmd := []string{"tmux", "new-session", "-d", "-s", tmuxSessionName}
	if len(env) == 0 {
		return cmd
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if envArgvSafe(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd = append(cmd, "-e", k+"="+env[k])
	}
	return cmd
}

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
	return l.opProvisionWithConfig(ctx, sessionName, stageConfig{})
}

// opProvisionWithConfig is opProvision plus NRT-P1-06 staging: once the box
// is dispatched (and therefore exec-able, RPP-PROVISION-001), cfg's
// workspace files and secret env vars are materialized into it before this
// returns — staging does not wait for launch, since a provisioned-but-not-
// launched box already answers exec and the agent bootstrap command started
// at launch time expects its workspace and secrets to already be in place.
func (l *lifecycle) opProvisionWithConfig(ctx context.Context, sessionName string, cfg stageConfig) error {
	unlock, err := l.lockSession(ctx, sessionName)
	if err != nil {
		return err
	}
	defer unlock()
	if err := l.dispatch(ctx, sessionName); err != nil {
		return err
	}
	return l.stage(ctx, sessionName, cfg)
}

// opStart is provision + launch (04 §3 start row): dispatch a fresh child,
// then launch the agent into it over exec, and only then mark the binding
// launched. Held under sessionName's single-flight lock end-to-end
// (R2b-03) so two concurrent Start calls for the same session cannot both
// observe an absent/terminal binding and each dispatch a child.
//
// Before dispatching, it checks for a binding left by a prior attempt that
// crashed after the dispatch call committed but before the launched marker
// was recorded (04 §2.1 rule 6 territory, the "after binding, before
// ConfirmStarted" crash point): that binding IS the positive attribution
// (no cluster lookup needed, since this pack already wrote it), so the
// retry resumes straight to launch instead of dispatching a second child or
// being rejected by dispatch's already-exists check.
func (l *lifecycle) opStart(ctx context.Context, sessionName string) error {
	return l.opStartWithConfig(ctx, sessionName, stageConfig{})
}

// opStartWithConfig is opStart plus NRT-P1-06 staging: cfg's workspace
// files and secret env vars are materialized (l.stage) after the box is
// dispatched (or adopted via resumeUnlaunched) and before launch, so the
// agent bootstrap command that launch execs finds its workspace and secrets
// already in place. cfg.Env's argvSafe subset also rides launch's tmux
// argv (buildLaunchCommand) — never the secrets dir.
func (l *lifecycle) opStartWithConfig(ctx context.Context, sessionName string, cfg stageConfig) error {
	unlock, err := l.lockSession(ctx, sessionName)
	if err != nil {
		return err
	}
	defer unlock()

	if resume, err := l.resumeUnlaunched(ctx, sessionName); err != nil {
		return err
	} else if resume {
		if err := l.stage(ctx, sessionName, cfg); err != nil {
			return err
		}
		return l.markLaunched(ctx, sessionName, cfg.Env)
	}

	if err := l.dispatch(ctx, sessionName); err != nil {
		return err
	}
	if err := l.stage(ctx, sessionName, cfg); err != nil {
		return err
	}
	return l.markLaunched(ctx, sessionName, cfg.Env)
}

// resumeUnlaunched reports whether sessionName has a binding whose child
// was dispatched but never marked launched, and that child still has a
// live (non-terminal) alloc. Returns false (with no error) for anything
// that should fall through to a normal fresh dispatch, including a lookup
// failure — a stuck lookup must not block start from ever proceeding.
func (l *lifecycle) resumeUnlaunched(ctx context.Context, sessionName string) (bool, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return false, err
	}
	if !ok || b.ChildJobID == "" || b.Launched {
		return false, nil
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return false, nil
	}
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			return true, nil
		}
	}
	return false, nil
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
	// No fresh env rides a relaunch (04 §7: it re-execs into the SAME alloc
	// with no fresh dispatch) — the box's workspace/secrets were already
	// staged at start/provision time and are still there.
	return l.markLaunched(ctx, sessionName, nil)
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

// opNudge sends text into sessionName's tmux session as if typed at the
// keyboard, followed by Enter — the turn-start driving verb, realized as
// tmux commands over exec rather than a bespoke protocol (fnrt-szx). text
// is carried as a positional shell argument ($1), never interpolated into
// the script string, so arbitrary content (quotes, backticks, newlines)
// can never break out of the intended two tmux calls.
func (l *lifecycle) opNudge(ctx context.Context, sessionName, text string) error {
	command := []string{
		"/bin/sh", "-c",
		`tmux send-keys -t "$1" -l -- "$2" && tmux send-keys -t "$1" Enter`,
		"nudge", tmuxSessionName, text,
	}
	return bestEffort(l.runDrivingVerb(ctx, sessionName, "nudge", command))
}

// opPeek captures sessionName's tmux pane content and returns it. lines <= 0
// captures only the visible pane; lines > 0 also captures that many lines of
// scrollback history (tmux capture-pane -S).
func (l *lifecycle) opPeek(ctx context.Context, sessionName string, lines int) ([]byte, error) {
	command := []string{"tmux", "capture-pane", "-t", tmuxSessionName, "-p"}
	if lines > 0 {
		command = append(command, "-S", "-"+strconv.Itoa(lines))
	}
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("peeking session %q: %w", sessionName, err)
	}
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, command)
	if err != nil {
		return nil, fmt.Errorf("peeking session %q: %w", sessionName, err)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("peeking session %q: tmux capture-pane exited %d: %s", sessionName, exitCode, out)
	}
	return out, nil
}

// opInterrupt sends Ctrl-C into sessionName's tmux session — a best-effort,
// idempotent interrupt of whatever is currently running in the pane
// (mirrors runtime-cloudflare's interrupt op).
func (l *lifecycle) opInterrupt(ctx context.Context, sessionName string) error {
	command := []string{"tmux", "send-keys", "-t", tmuxSessionName, "C-c"}
	return bestEffort(l.runDrivingVerb(ctx, sessionName, "interrupt", command))
}

// bestEffort turns an errSessionNotFound failure into success — the
// protocol's best-effort convention for interrupt/nudge (docs/reference/
// exec-session-provider.md), which must return 0 even when the session was
// never provisioned or has already stopped. Any other error (transport
// fault, nonzero tmux exit) still propagates.
func bestEffort(err error) error {
	if errors.Is(err, errSessionNotFound) {
		return nil
	}
	return err
}

// opSendKeys forwards keys verbatim to sessionName's tmux session (tmux
// send-keys key syntax — e.g. "Enter", "C-c", literal text), unlike opNudge
// which always types literal text followed by Enter.
func (l *lifecycle) opSendKeys(ctx context.Context, sessionName string, keys []string) error {
	command := append([]string{"tmux", "send-keys", "-t", tmuxSessionName}, keys...)
	return l.runDrivingVerb(ctx, sessionName, "send-keys", command)
}

// opClearScrollback discards sessionName's tmux scrollback history, leaving
// the visible pane untouched.
func (l *lifecycle) opClearScrollback(ctx context.Context, sessionName string) error {
	command := []string{"tmux", "clear-history", "-t", tmuxSessionName}
	return l.runDrivingVerb(ctx, sessionName, "clear-scrollback", command)
}

// runDrivingVerb execs command (a tmux invocation targeting tmuxSessionName)
// into sessionName's current alloc and turns a nonzero exit or transport
// failure into an error — the shared tail of every driving verb above.
func (l *lifecycle) runDrivingVerb(ctx context.Context, sessionName, verb string, command []string) error {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return fmt.Errorf("%s session %q: %w", verb, sessionName, err)
	}
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, command)
	if err != nil {
		return fmt.Errorf("%s session %q: %w", verb, sessionName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s session %q: tmux command exited %d: %s", verb, sessionName, exitCode, out)
	}
	return nil
}

// dispatch registers the parent job (idempotent upsert) if needed, then
// dispatches a tmux-only child for sessionName — the mechanism shared by
// opProvision directly and opStart (provision half).
func (l *lifecycle) dispatch(ctx context.Context, sessionName string) error {
	existing, existingOK, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if existingOK && existing.ChildJobID != "" {
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

	if err := l.ensureParentRegistered(ctx); err != nil {
		return err
	}

	// Positive-attribution adoption (04 §2.1 rule 6): a prior attempt may
	// have crashed after its dispatch call returned but before the binding
	// commit that records ChildJobID, leaving an orphaned child cluster-side
	// the sidecar only half-remembers (an intent record with a nonce and no
	// ChildJobID). Minting a second dispatch here would leave two
	// non-terminal children for one session, violating rule 4 — so look for
	// a child whose dispatch Meta nonce matches ours first, and adopt it
	// instead of dispatching fresh if one is still alive.
	if existingOK && existing.Nonce != "" && existing.ChildJobID == "" {
		if adopted, ok, err := l.findOrphanByNonce(ctx, sessionName, existing.Nonce); err == nil && ok {
			final := *existing
			final.ChildJobID = adopted
			return l.sidecar.save(final)
		}
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

// ensureParentRegistered makes sure the parent job dispatch's children come
// from exists, in the current namespace, and matches the jobspec this build
// is configured for — the NRT-P2-06.1 fix for the lab ACL model (04 §4): the
// runtime token dispatch normally runs under may hold read-job and
// dispatch-job but deliberately NOT submit-job, since registering the
// parent is a one-time management-token setup step, not a per-Start action.
// Previously dispatch called registerJob unconditionally on every Start,
// which a narrowed dispatch-only token could never satisfy.
//
// It looks the parent up first via a GET (dispatch-scoped tokens can always
// read-job) and only calls register when the parent is missing or has
// drifted from this build's config on either signal getJob reports: NodePool
// (the one parentJobSpec field beyond namespace that varies at runtime,
// NRT-P2-05) or the jobspecHashMetaKey fingerprint (fnrt-t4l.9) — added
// because a NodePool-only comparison missed a registered parent whose TASK
// had gone stale (e.g. t4l.7's supervisor-script swap): the parent still
// existed and still matched on NodePool, so Start silently kept dispatching
// onto the old task instead of re-registering. A token that is genuinely
// dispatch-only, against a parent that already matches on both signals,
// never calls register at all. l.forbidRegistration skips the register
// attempt entirely for a deployment that wants a config-time guarantee of
// that, rather than discovering it from a 403 at the first drifted Start.
func (l *lifecycle) ensureParentRegistered(ctx context.Context) error {
	spec := parentJobSpec(l.client.namespace, l.nodePool, l.parentJobID)

	nodePool, meta, ok, err := l.client.getJob(ctx, l.parentJobID)
	if err != nil {
		return fmt.Errorf("looking up parent job %q: %w", l.parentJobID, err)
	}
	if ok && nodePool == l.nodePool && meta[jobspecHashMetaKey] == spec.Meta[jobspecHashMetaKey] {
		return nil
	}

	if l.forbidRegistration {
		if ok {
			return fmt.Errorf("parent job %q exists but has drifted from this build's jobspec (node pool and/or task spec), and GC_NOMAD_FORBID_REGISTER forbids registering to fix it: register it out-of-band with a token holding the submit-job capability", l.parentJobID)
		}
		return fmt.Errorf("parent job %q does not exist in namespace %q, and GC_NOMAD_FORBID_REGISTER forbids registering it: register it out-of-band with a token holding the submit-job capability", l.parentJobID, l.client.namespace)
	}

	if err := l.client.registerJob(ctx, spec); err != nil {
		if errors.Is(err, errJobForbidden) {
			if ok {
				return fmt.Errorf("parent job %q is stale — re-register with a management token: the registered parent no longer matches this build's jobspec (node pool and/or task spec drifted) and the runtime token lacks the submit-job capability to fix it in place (04 §4 lab ACL model): %w", l.parentJobID, err)
			}
			return fmt.Errorf("registering parent job %q: token lacks the submit-job capability required to register jobs in namespace %q — register it once with a management token (04 §4 lab ACL model), or set GC_NOMAD_FORBID_REGISTER and do so out-of-band: %w", l.parentJobID, l.client.namespace, err)
		}
		return fmt.Errorf("registering parent job %q: %w", l.parentJobID, err)
	}
	return nil
}

// findOrphanByNonce looks up the parent job's dispatched children (04 §2.1
// rule 2: "list the city parent's children with meta=true") for one whose
// Meta gc_session/gc_nonce match, and reports it only if it is still
// non-terminal — a matched-but-terminal child died along with the crashed
// attempt that dispatched it and is not adopted.
func (l *lifecycle) findOrphanByNonce(ctx context.Context, sessionName, nonce string) (string, bool, error) {
	children, err := l.client.listChildJobs(ctx, l.parentJobID)
	if err != nil {
		return "", false, err
	}
	for _, c := range children {
		if c.Terminal || c.Meta["gc_session"] != sessionName || c.Meta["gc_nonce"] != nonce {
			continue
		}
		return c.ID, true, nil
	}
	return "", false, nil
}

// launch execs buildLaunchCommand(env) into sessionName's current alloc —
// the detached tmux-client call that starts the agent (04 §3 provision row
// launch invariant).
func (l *lifecycle) launch(ctx context.Context, sessionName string, env map[string]string) error {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return err
	}
	exitCode, dbgOut, err := l.client.execAlloc(ctx, allocID, execTaskName, buildLaunchCommand(env))
	if err != nil {
		return fmt.Errorf("launching session %q: %w", sessionName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("launching session %q: launch command exited %d DEBUGOUT=%q", sessionName, exitCode, string(dbgOut))
	}
	return nil
}

// capturePanePID reads back the pid tmux assigned the pane launch just
// created inside sessionName's current alloc — the in-box liveness probe
// (opIsRunning) later kill -0's this exact pid rather than trusting the
// tmux session's mere existence, which alone cannot tell "agent died,
// nothing reaped the empty session" apart from "agent alive" (08 §3 in-box
// agent kill row). A lookup failure here is non-fatal to launch itself: it
// just leaves AgentPID unset, and the probe falls back to a tmux-session-
// only check.
func (l *lifecycle) capturePanePID(ctx context.Context, sessionName string) (int, error) {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return 0, err
	}
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"tmux", "list-panes", "-t", tmuxSessionName, "-F", "#{pane_pid}"})
	if err != nil {
		return 0, err
	}
	if exitCode != 0 {
		return 0, fmt.Errorf("tmux list-panes exited %d: %s", exitCode, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parsing pane pid %q: %w", strings.TrimSpace(string(out)), err)
	}
	return pid, nil
}

// markLaunched launches sessionName's agent and, only once that succeeds,
// records the launched marker (and the pane pid the probe will later
// kill -0) in the sidecar binding — shared by opStart (provision then
// launch) and opRelaunch (launch only, no re-dispatch). env's argvSafe
// subset rides the launch command (buildLaunchCommand); opRelaunch passes
// nil since no fresh env accompanies a relaunch.
func (l *lifecycle) markLaunched(ctx context.Context, sessionName string, env map[string]string) error {
	if err := l.launch(ctx, sessionName, env); err != nil {
		return err
	}
	// Best-effort: a failure recording the pid does not fail the launch
	// itself, since the tmux session already exists and answers exec — it
	// just leaves the probe without a pid to kill -0 (falls back to a
	// tmux-session-only check).
	pid, _ := l.capturePanePID(ctx, sessionName)
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before launch could be recorded", sessionName)
	}
	b.Launched = true
	b.AgentPID = pid
	return l.sidecar.save(*b)
}

// stage materializes cfg's workspace files and secret env vars into
// sessionName's current alloc (NRT-P1-06, 04 §5 data contract) over two
// tar-over-exec-stdin calls: cfg.Files (the CopyFiles/workspace-in analog)
// extracted under cfg.WorkDir, and cfg.Env's non-argvSafe entries (the
// secret ones — envArgvSafe's whole point, E1a §4.5) extracted as individual
// files under $NOMAD_SECRETS_DIR. Neither channel ever touches the job spec,
// argv, or the sidecar — the only place secret BYTES appear is the
// alloc-exec WebSocket stream itself (accepted residual, 05 §7 R9). A
// zero-value cfg is a no-op, so start/provision behave exactly as before
// staging landed when a caller sends no config.
func (l *lifecycle) stage(ctx context.Context, sessionName string, cfg stageConfig) error {
	if len(cfg.Files) > 0 {
		data, err := buildTar(cfg.Files)
		if err != nil {
			return fmt.Errorf("staging workspace for %q: %w", sessionName, err)
		}
		workDir := cfg.WorkDir
		if workDir == "" {
			workDir = "."
		}
		command := []string{
			"/bin/sh", "-c",
			`mkdir -p "$1" && tar -x -f - -C "$1"`,
			"stage-workspace", workDir,
		}
		if err := l.execStage(ctx, sessionName, command, data); err != nil {
			return fmt.Errorf("staging workspace for %q: %w", sessionName, err)
		}
	}

	var secretFiles []stageFile
	for k, v := range cfg.Env {
		if envArgvSafe(k) {
			continue
		}
		secretFiles = append(secretFiles, stageFile{Path: k, Content: []byte(v), Mode: 0o600})
	}
	if len(secretFiles) > 0 {
		data, err := buildTar(secretFiles)
		if err != nil {
			return fmt.Errorf("staging secrets for %q: %w", sessionName, err)
		}
		command := []string{
			"/bin/sh", "-c",
			`mkdir -p "$NOMAD_SECRETS_DIR" && tar -x -f - -C "$NOMAD_SECRETS_DIR"`,
			"stage-secrets",
		}
		if err := l.execStage(ctx, sessionName, command, data); err != nil {
			return fmt.Errorf("staging secrets for %q: %w", sessionName, err)
		}
	}
	return nil
}

// execStage runs command inside sessionName's current alloc with stdin
// attached (execAllocStdin) and turns a nonzero exit into an error — the
// shared tail stage's two tar-extraction calls need. Deliberately does not
// echo stdin (the tar bytes) into any error message: a failing tar's own
// stderr/stdout (out) is safe to surface, but stdin here may carry secret
// content the error path must never leak.
func (l *lifecycle) execStage(ctx context.Context, sessionName string, command []string, stdin []byte) error {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return err
	}
	exitCode, out, err := l.client.execAllocStdin(ctx, allocID, execTaskName, command, stdin)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("tar exited %d: %s", exitCode, out)
	}
	return nil
}

// errSessionNotFound marks a currentAlloc failure caused by the session
// simply not existing (never provisioned, or already stopped) rather than a
// transport/lookup fault — the distinction opNudge and opInterrupt need to
// honor the protocol's best-effort convention (docs/reference/exec-session-
// provider.md "Best-effort interrupt/nudge: Return 0 even if the session
// doesn't exist").
var errSessionNotFound = errors.New("session not found")

// currentAlloc resolves the non-terminal Nomad alloc ID backing
// sessionName's current child job, sidecar-binding-primary (04 §2.1 rule 1).
func (l *lifecycle) currentAlloc(ctx context.Context, sessionName string) (string, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return "", err
	}
	if !ok || b.ChildJobID == "" {
		return "", fmt.Errorf("session %q has no provisioned box: %w", sessionName, errSessionNotFound)
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
	return "", fmt.Errorf("session %q has no non-terminal alloc: %w", sessionName, errSessionNotFound)
}

// egressMaxAttempts bounds the stop-path transcript/evidence egress retries
// before opStop gives up and falls back to an evidence_lost marker (04 §6
// R2b-04): evidence-best-effort beats a wedged fleet.
const egressMaxAttempts = 3

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

	// Best-effort: end the agent's tmux session before the task itself is
	// killed below (NRT-P2-06.1). The task's own exit is independent of
	// this — jobspec.go's sessionSupervisorScript traps SIGTERM and exits 0
	// on its own once deregister signals it — but tmux's server process is
	// a detached daemon outside the task's process tree once launch starts
	// it (buildLaunchCommand), so nothing else ever reaps it. Any failure
	// here (alloc already terminal, transport fault) is ignored — the
	// deregister call below is what actually confirms terminal either way.
	_ = l.runDrivingVerb(ctx, sessionName, "stop-kill-session", []string{"tmux", "kill-session", "-t", tmuxSessionName})

	// Egress transcript/evidence before deregister makes them unreachable.
	// A failing egress gets egressMaxAttempts bounded retries (R2b-04); if
	// it still hasn't succeeded, stop PROCEEDS rather than wedging behind
	// evidence collection — it just marks the tombstone evidence_lost
	// instead. Either outcome is receipted in the sidecar (still resident)
	// before deregister, so a stop that then crashes before deregister
	// retries idempotently: it neither re-attempts a marker that already
	// gave up nor re-copies files it already egressed (NRT-P1-07).
	if !b.EgressComplete && !b.EvidenceLost {
		var egressErr error
		for attempt := 0; attempt < egressMaxAttempts; attempt++ {
			if egressErr = l.egressAllocFiles(ctx, sessionName, b.ChildJobID); egressErr == nil {
				break
			}
		}
		if egressErr == nil {
			b.EgressComplete = true
		} else {
			b.EvidenceLost = true
		}
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

// probeAgentAlive execs an in-box liveness check into allocID: the tmux
// session launch created (tmuxSessionName) must still exist, AND — when
// pid was captured at launch (capturePanePID) — that exact process must
// still answer kill -0. A box whose alloc stays non-terminal but whose
// agent died inside it (the tmux session and/or its pane process gone) is
// otherwise indistinguishable from a healthy session to an alloc-status-
// only check (08 §3 in-box agent kill row). A transport error execing the
// probe itself is returned to the caller rather than collapsed to
// alive/dead here — opIsRunning applies its own honesty split to that case.
func (l *lifecycle) probeAgentAlive(ctx context.Context, allocID string, pid int) (bool, error) {
	check := fmt.Sprintf("tmux has-session -t %s", tmuxSessionName)
	if pid > 0 {
		check += fmt.Sprintf(" && kill -0 %d", pid)
	}
	exitCode, _, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{"/bin/sh", "-c", check})
	if err != nil {
		return false, err
	}
	return exitCode == 0, nil
}

// opIsRunning reports whether sessionName has a non-terminal child alloc,
// is past the launched marker, AND the in-box agent itself is still alive —
// a provisioned-but-not-launched box (alloc running, launched marker unset)
// answers false here even though it already answers exec (04 §6 decision
// table, RPP-PROVISION-001: the launched marker is what distinguishes
// "provisioned, agent never launched" from "launched, agent died"). Once
// launched, alive=true requires BOTH the alloc to be non-terminal AND
// probeAgentAlive to confirm the agent process/tmux session is still there
// — alive=false with the box otherwise healthy means the agent died inside
// a surviving box (08 §3 in-box agent kill row), which the alloc's
// ClientStatus alone can never see. The per-op honesty split (04 §6) still
// applies to every transport fault along the way — listing allocs or
// execing the probe itself — answering last-known-good (true, since a
// binding is still on record) rather than false; flipping to false on mere
// unavailability would read as confirmed death to gc's heal/quarantine
// ladders, indistinguishable from a genuine agent-dead answer.
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
	allocID := ""
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			allocID = a.ID
			break
		}
	}
	if allocID == "" {
		return false, nil
	}
	alive, err := l.probeAgentAlive(ctx, allocID, b.AgentPID)
	if err != nil {
		// Transport fault probing in-box liveness: unknown, not confirmed
		// death — same honesty split as the alloc lookup above.
		return true, nil
	}
	return alive, nil
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
