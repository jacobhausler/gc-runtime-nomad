package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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

const (
	// allocExecReadyAttempts and allocExecReadyDelay bound the small window
	// between Nomad dispatch returning and the allocation's exec endpoint
	// becoming usable. This is deliberately a short readiness retry, not a
	// general retry for provider or command failures.
	allocExecReadyAttempts = 20
	allocExecReadyDelay    = 250 * time.Millisecond
)

var (
	// errBindingNoMatch is returned when Nomad's children-of-parent read has
	// no child attributable to the requested session. Callers that are
	// looking for a current allocation translate this into the existing
	// session-not-found contract; the recovery helper keeps it distinct so a
	// caller cannot mistake an empty read for a recovered binding.
	errBindingNoMatch = errors.New("sidecar binding recovery: no matching child")
	// errBindingAmbiguous prevents a recovery from guessing when more than one
	// child carries the same session Meta. The safe answer is a loud refusal.
	errBindingAmbiguous = errors.New("sidecar binding recovery: matching children are ambiguous")
	// errBindingTerminal prevents a terminal Meta-matching child from being
	// silently skipped. A terminal match is evidence of a stale or disputed
	// binding, not permission to dispatch another child.
	errBindingTerminal = errors.New("sidecar binding recovery: matching child is terminal")
)

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

	// logShipper configures the optional log-shipper task added to the
	// session task group's jobspec (fnrt-t4l.13). Zero value (Sink=="")
	// disables it — parentJobSpec's default single-task group is
	// unaffected. It only ever rides the parent job body parentJobSpec
	// builds, same as nodePool above.
	logShipper logShipperConfig

	// artifactVolumeSource is the optional Nomad client host-volume name used
	// to deliver the pinned Linux gc artifact. It is deployment input, never a
	// host name hard-coded into the runtime pack; empty means no mount.
	artifactVolumeSource string

	// agentImage selects the provider-neutral Docker task shape. When empty,
	// the legacy exec task and optional artifact volume remain the rollback
	// path; when set, the image supplies the task toolchain and the host-volume
	// shape is omitted.
	agentImage string

	// placement is the deployment-configured DNS servers and node constraints
	// that ride the parent job body parentJobSpec builds (cr-tzsxk.18), read
	// from GC_NOMAD_DNS_SERVERS / GC_NOMAD_NODE_CONSTRAINT by newLifecycle.
	// Zero value renders the pre-cr-tzsxk.18 jobspec unchanged — same rollback
	// shape as logShipper and artifactVolumeSource above.
	placement placementConfig

	// adoptMaxAge bounds how old a live-but-unlaunched box may be before
	// provision/start refuse to ADOPT it and reap it instead (cr-gdeav.6.1).
	// Before it existed, adoption was unbounded: the DF-03 canary's box,
	// dispatched 2026-09-05T20:54Z, was still being adopted at 11:15Z the
	// next day (14h20m old) and re-staged as if it were this tick's work.
	// newLifecycle sets it from GC_NOMAD_ADOPT_MAX_AGE. The zero value means
	// defaultAdoptMaxAge (30m) — a limit that can be switched off by omitting
	// a knob is the bug this field exists to close, so there is no opt-out
	// state; adoptAgeLimit resolves the two.
	adoptMaxAge time.Duration

	// logf, when set, receives the pack's decision lines — notably WHICH
	// adopt/reap path a provision or start call chose, which is otherwise
	// invisible from outside the box. nil-safe by design: the lifecycle is
	// built without an output sink in tests, and newLifecycle wires it to
	// stderr, the only channel gc's exec proxy captures from this binary.
	logf func(format string, args ...any)
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
// allowlist (e.g. a directory listing) is out of scope by design: the list is
// also the credential boundary. Only named files are ever copied out of an
// allocation, so a recursive sweep can never carry the session's staged
// secrets (alloc/secrets/, tmp/) into a durable sink.
//
// The agent-capture trio (cr-gdeav.6.5) is written inside the allocation by
// the deployment bootstrap ops/nomad/agent-launch.sh:
//
//	alloc/logs/agent.exit   exit= started= ended= pane= argv0= alloc= — the
//	                        agent command NAME and timestamps only, never env
//	                        values (that is what makes this file safe to ship)
//	alloc/logs/agent.stdout raw pane output, tapped with `tmux pipe-pane` so
//	                        the agent keeps its tty (gc drives it as an
//	                        interactive client: peek/nudge/interrupt)
//	alloc/logs/agent.stderr the agent's stderr, appended
//
// All three must be copied before deregister. The pane dies with the session,
// `nomad alloc logs` reads the *task's* streams (the pause shell — empty), and
// `gc session logs` cannot resolve a session written with `codex --ephemeral`,
// so after deregistration the three channels are simply gone (cr-gdeav.6.3
// forensics found all of them empty). A lane whose capture block has not
// landed yet 404s on these paths, which egressAllocFiles already skips.
var egressFiles = []struct {
	allocPath string
	destName  string
}{
	{"alloc/logs/transcript.log", "transcript.log"},
	{"alloc/data/evidence.json", "evidence.json"},
	{"alloc/logs/agent.exit", "agent.exit"},
	{"alloc/logs/agent.stdout", "agent.stdout"},
	{"alloc/logs/agent.stderr", "agent.stderr"},
}

// egressAllocFiles copies childJobID's per-allocation declared egress
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
var execTaskName = "agent"

func init() {
	configureExecTaskName()
}

func configureExecTaskName() {
	if task := os.Getenv("GC_NOMAD_EXEC_TASK"); task != "" {
		execTaskName = task
	}
}

// tmuxSessionName is the wire-contract constant in-box tmux session name
// (04 §5 R1a-08/-09): every carrier verb and the launch command target
// exactly this session.
const tmuxSessionName = "main"

// launchEnvScript is the small POSIX wrapper around the detached tmux-client
// command. The environment script is optional so an empty/legacy allocation
// remains a valid local-mode session.
const launchEnvScript = `set -eu
if [ -f "$NOMAD_SECRETS_DIR/env.sh" ]; then
	. "$NOMAD_SECRETS_DIR/env.sh"
fi
exec "$@"`

// paneStagedEnvPrelude is the shell prelude a launched pane runs BEFORE the
// deployment-configured agent script. tmux builds a pane's environment from
// the tmux SERVER and never from the client that issued the verb, so the
// client-side sourcing above reaches neither new-session nor respawn-pane
// (cr-gdeav.6.8, measured on tmux 3.7b): an agent launched that way had no
// GC_CITY_URL, no session identity and no relay pointers while still
// reporting launched=true. Sourcing the staged env.sh inside the pane, where
// the tmpfs already holds it, is the one delivery path every provider shares,
// which makes a provider's `command` shim redundant rather than load-bearing.
// The prelude names only $NOMAD_SECRETS_DIR — a Nomad-provided task path, not a
// secret — so no credential value is interpolated into argv (envArgvSafe is
// untouched), and it drops the client-side city/rig labels the pane must not
// act on, the same trio the proof's shim carried.
const paneStagedEnvPrelude = `if [ -f "$NOMAD_SECRETS_DIR/env.sh" ]; then . "$NOMAD_SECRETS_DIR/env.sh"; fi; ` +
	`unset GC_CITY GC_CITY_PATH GC_RIG; `

// paneCwdPrelude is the shell the launched pane runs to choose its working
// directory (cr-gdeav.6.10 deliverable 3). Nothing cd'd the pane before, so
// it inherited the exec driver's cwd - "/" on the D3 attempt-3 canary, which
// is why a rig-shaped session could not run one git command in the box.
// The rule: use gc's work_dir when the alloc actually has it (gc sends its
// OWN WorkDir, usually a supervisor-host path that does not exist inside the
// alloc, so "exists" is the test, not "was sent"); otherwise use a per-alloc
// scratch directory under $NOMAD_ALLOC_DIR, which is writable, is per-session
// (one alloc per session job), and sits beside the logs/ the Vector shipper
// already reads. Both legs degrade to "stay where you are" rather than fail
// the launch: an agent that cannot start is worse than an agent in an
// unexpected directory, and the directory it got is recorded in the binding.
//
// workDir is gc/deployment input, never a credential, and shellQuote keeps it
// data when it reaches the pane's shell.
func paneCwdPrelude(workDir string) string {
	scratch := `if [ -n "${NOMAD_ALLOC_DIR:-}" ]; then mkdir -p "$NOMAD_ALLOC_DIR/work" 2>/dev/null && cd "$NOMAD_ALLOC_DIR/work" 2>/dev/null; fi; `
	if wd := strings.TrimSpace(workDir); wd != "" {
		return "cd " + shellQuote(wd) + " 2>/dev/null || " + scratch
	}
	return scratch
}

// agentLaunchScript is the POSIX shell script the launch command execs
// inside the tmux pane once the staged environment is sourced — the bounded
// seam a deployment uses to turn the placeholder session into a real agent
// bootstrap (PATH prepend to a pinned toolchain, an agent-specific HOME
// pointed inside $NOMAD_SECRETS_DIR, installing a staged auth file, then
// exec-ing the agent). Empty means no real agent command line is configured:
// buildLaunchCommand keeps the legacy bare-tmux placeholder session, which
// is both the generic-command-execution default and this seam's rollback —
// unsetting GC_NOMAD_AGENT_LAUNCH_SCRIPT reverts every alloc to the prior
// behavior with no code change. The pack never hardcodes an agent's PATH,
// HOME variable, or auth filename; those are entirely deployment config, so
// this file stays agent-agnostic (Codex today, anything else tomorrow).
var agentLaunchScript string

// extraSecretKeys is the parsed name list from GC_NOMAD_EXTRA_SECRET_KEYS
// (comma-separated), read once at init like agentLaunchScript's seam above
// — a deployment-configured bridge letting the supervisor's own process env
// (e.g. via ~/.gc/secrets.env passthrough) deliver a named secret into a
// session's staged environment when gc's own city.toml provider env map has
// no way to interpolate it (cr-u4plc.2). Unset/empty means no extra keys,
// so stage() behaves exactly as before this seam landed — the rollback
// path.
var extraSecretKeys []string

func init() {
	configureAgentLaunchScript()
	configureExtraSecretKeys()
}

func configureAgentLaunchScript() {
	agentLaunchScript = os.Getenv("GC_NOMAD_AGENT_LAUNCH_SCRIPT")
}

func configureExtraSecretKeys() {
	extraSecretKeys = parseExtraSecretKeys(os.Getenv("GC_NOMAD_EXTRA_SECRET_KEYS"))
}

func parseExtraSecretKeys(raw string) []string {
	var keys []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			keys = append(keys, part)
		}
	}
	return keys
}

// buildLaunchCommand is the detached tmux-client command that turns a
// provisioned (tmux-only) box into a launched one (04 §3 provision row +
// R1c-04 launch invariant): it returns immediately, and the agent it starts
// is parented to the tmux server inside the task cgroup, never to the exec
// session that issued it. When agentLaunchScript is configured, the tmux
// pane's initial command execs that script (a real agent bootstrap command
// line); otherwise the session stays the placeholder tmux shell it always
// was (jobspec.go's sessionTask has the same rollback shape).
//
// env's argvSafe-classified entries (envArgvSafe, NRT-P1-06) ride as `-e
// KEY=VALUE` on this argv — safe by construction, since envArgvSafe's whole
// job is excluding anything credential-shaped from ever reaching argv
// (E1a §4.5). The wrapper sources the value-safe env.sh produced by stage
// before invoking tmux, and the pane body re-sources it INSIDE the pane
// (paneStagedEnvPrelude), because tmux gives a pane the server's environment
// and never the client's; keys are sorted so the command is deterministic.
// agentLaunchScript itself never carries credential values — it is
// deployment-owned shell logic (PATH/HOME wiring) that reads secret bytes
// out of the already-staged $NOMAD_SECRETS_DIR files at pane-start time, so
// nothing secret ever rides this argv either.
func buildLaunchCommand(env map[string]string, workDir string) []string {
	return buildLaunchCommandForTmux(env, []string{"new-session", "-d", "-s", tmuxSessionName}, workDir)
}

// buildLaunchCommandForTmux uses the pack-owned staged bootstrap with the
// canonical tmux operation selected by GC. The transport launch command is a
// handshake, not the provider's bootstrap command: preserve whether GC asked
// for a new session or a respawn, while sourcing the staged environment and
// applying the deployment-owned agent launch script in either case.
func buildLaunchCommandForTmux(env map[string]string, tmuxArgs []string, workDir string) []string {
	return buildLaunchCommandForTmuxWithPaneCommand(env, tmuxArgs, workDir, agentLaunchScript)
}

// buildLaunchCommandForTmuxWithPaneCommand uses the canonical tmux operation
// and runs paneCommand after the pack's in-pane staged-environment and cwd
// preludes. The command is supplied by GC for separable transport launches;
// opStart continues to supply the deployment-owned bootstrap through
// buildLaunchCommandForTmux.
func buildLaunchCommandForTmuxWithPaneCommand(env map[string]string, tmuxArgs []string, workDir, paneCommand string) []string {
	env = stripLocalCityStoreEnv(env)
	cmd := []string{"/bin/sh", "-c", launchEnvScript, "gc-runtime-nomad-launch", "tmux"}
	cmd = append(cmd, tmuxArgs...)
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
	if paneCommand != "" {
		// The pane sources the staged environment itself (see
		// paneStagedEnvPrelude): the client-side wrapper cannot reach it.
		// The cwd prelude runs after it and before paneCommand, so deployment
		// wiring or GC's rendered command starts in a directory the alloc
		// actually has (paneCwdPrelude). With no command configured the pane
		// stays the placeholder shell it always was, cwd included - that path is
		// the seam's rollback.
		cmd = append(cmd, "/bin/sh", "-c", paneStagedEnvPrelude+paneCwdPrelude(workDir)+paneCommand)
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
//
// Provision is idempotent over the box it already made: a session whose
// bound child is still live but NOT launched is ADOPTED (staging re-run
// against it) rather than rejected. That is the fnrt-t4l.46 fix — a
// provision that dispatched its child and then never got its launch
// confirmed leaves exactly that state, and a controller that re-drives
// provision every tick must be able to finish the job instead of collecting
// "already exists" forever (61 of them in the DF-03 canary).
//
// A session whose child is live AND launched is still rejected: an agent is
// running in that box, so provisioning over it is a duplicate, not a retry.
// That rejection's stderr message MUST contain "already exists" (04 §6
// wire-contract constant, R1a-02: gc's exec proxy infers ErrSessionExists
// from that exact phrase).
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

	// Adopt-or-dispatch (fnrt-t4l.46): a live child already bound to this
	// session is the box this call was asked to make, so re-drive staging
	// against it instead of dispatching a second one (04 §2.1 rule 4) or
	// rejecting the caller. Re-staging is safe — stage's channels are
	// overwriting writes into fixed paths, so the adopted box converges on
	// the same state a fresh provision would have produced.
	//
	// This subsumes the narrower StagePending resume (fnrt-t4l.18): a
	// provision that died mid-staging is only one of the ways a live,
	// unlaunched box gets left behind. The DF-03 canary hit the other one —
	// staging finished, the launch was never confirmed, and every later
	// provision tick was rejected while the alloc kept running.
	//
	// Adoption is bounded (cr-gdeav.6.1): the box has to be young enough
	// (adoptMaxAge) and its agent confirmed absent, or it is not this call's
	// half-finished box but an orphan, and adoptExisting reaps it so the
	// dispatch below makes a fresh one. See classifyAdoption.
	b, live, err := l.liveChild(ctx, sessionName)
	if err != nil {
		return err
	}
	if live {
		reuse, err := l.adoptExisting(ctx, sessionName, b, "provision")
		if err != nil {
			return err
		}
		if reuse {
			return l.stageTracked(ctx, sessionName, cfg)
		}
		// Reaped: the stale box is deregistered and tombstoned, so the
		// dispatch below is the fresh provision the ruling asked for
		// (never a bypass of liveChild — that gate is what got checked,
		// and it is what said "this box is no longer the one to reuse").
	}

	if err := l.dispatch(ctx, sessionName); err != nil {
		return err
	}
	return l.stageTracked(ctx, sessionName, cfg)
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
// being rejected by dispatch's already-exists check. That resume is bounded by
// the same adopt-or-reap gate provision uses (cr-gdeav.6.1): a box too old to
// be this attempt's, or one whose agent is gone, is reaped instead of resumed,
// and a box whose agent is actually running is refused rather than
// double-launched.
func (l *lifecycle) opStart(ctx context.Context, sessionName string) error {
	return l.opStartWithConfig(ctx, sessionName, stageConfig{})
}

// opStartWithConfig is opStart plus NRT-P1-06 staging: cfg's workspace
// files and secret env vars are materialized (l.stage) after the box is
// dispatched (or adopted through the same bounded adopt-or-reap gate
// provision uses) and before launch, so the agent bootstrap command that
// launch execs finds its workspace and secrets already in place. cfg.Env's argvSafe subset also rides launch's tmux
// argv (buildLaunchCommand) — never the secrets dir.
func (l *lifecycle) opStartWithConfig(ctx context.Context, sessionName string, cfg stageConfig) error {
	unlock, err := l.lockSession(ctx, sessionName)
	if err != nil {
		return err
	}
	defer unlock()

	b, live, err := l.liveChild(ctx, sessionName)
	if err != nil {
		return err
	}
	if live {
		reuse, err := l.adoptExisting(ctx, sessionName, b, "start")
		if err != nil {
			return err
		}
		if reuse {
			if err := l.stageTracked(ctx, sessionName, cfg); err != nil {
				return err
			}
			return l.markLaunched(ctx, sessionName, cfg)
		}
		// Reaped: fall through to a fresh dispatch (same as the provision
		// door above).
	}

	if err := l.dispatch(ctx, sessionName); err != nil {
		return err
	}
	if err := l.stageTracked(ctx, sessionName, cfg); err != nil {
		return err
	}
	return l.markLaunched(ctx, sessionName, cfg)
}

// liveChild reports sessionName's sidecar binding when that binding names a
// child job with at least one non-terminal alloc — the single "this session
// already has a box" test opProvision, opStart, and dispatch all key off, so
// the three cannot drift apart on what counts as an existing session.
//
// When the binding is missing, liveChild first attempts Meta recovery. An
// exactly-one live child is returned as live after the atomic repair. A
// missing binding with no matching child, or only a terminal child, follows
// the normal fresh-dispatch path; the recovery helper itself reports those
// cases, so they are never silently treated as a recoverable binding. A
// failed alloc lookup on a recovered child is an error rather than permission
// to dispatch around an unknown live state. For an existing binding, the
// historical best-effort lookup behavior remains unchanged.
func (l *lifecycle) liveChild(ctx context.Context, sessionName string) (binding, bool, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return binding{}, false, err
	}
	recovered := false
	if !ok || b.ChildJobID == "" {
		// Preserve dispatch's existing nonce-specific crash recovery for an
		// intent record. A completed dispatch with no binding is recovered by
		// session Meta below; an intent with no matching child still follows
		// dispatch's normal fresh-dispatch path.
		if ok && b.ChildJobID == "" && b.Nonce != "" {
			return binding{}, false, nil
		}
		var recoveredBinding binding
		if ok {
			recoveredBinding, err = l.recoverBinding(ctx, sessionName, b)
		} else {
			recoveredBinding, err = l.recoverBinding(ctx, sessionName, nil)
		}
		if err != nil {
			// A session that has never been provisioned has no binding and no
			// matching child. A terminal match is also the established post-stop
			// and post-reap state, so report both cases and preserve the existing
			// fresh-dispatch path. Ambiguity, malformed Meta, and lookup errors
			// remain fail-closed rather than guessing or dispatching.
			if errors.Is(err, errBindingNoMatch) || errors.Is(err, errBindingTerminal) {
				l.logDecision("provision/start %q: Meta binding recovery not used: %v", sessionName, err)
				return binding{}, false, nil
			}
			return binding{}, false, err
		}
		b = &recoveredBinding
		recovered = true
	}
	if b.ChildJobID == "" {
		return binding{}, false, nil
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		if recovered {
			return binding{}, false, fmt.Errorf("checking recovered child %q for session %q: %w", b.ChildJobID, sessionName, err)
		}
		return binding{}, false, nil
	}
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			return *b, true, nil
		}
	}
	if recovered {
		return binding{}, false, fmt.Errorf("recovered child %q for session %q has no non-terminal allocation", b.ChildJobID, sessionName)
	}
	return binding{}, false, nil
}

// adoptPath is the decision the two adopt-or-dispatch doors (provision and
// start) take for a session whose sidecar binding already names a live child.
// cr-gdeav.6.1 added the two rules the doors were missing: an age limit, and
// an agent-absence check before reuse. Before them the only split was
// launched-reject / unlaunched-adopt, so a box left half-provisioned for hours
// was still adopted as if it were this tick's work — the 2026-09-06 canary
// re-staged a child dispatched 14h20m earlier.
type adoptPath uint8

const (
	// adoptReuse re-drives this pack's own half of the job against the box it
	// already made — fnrt-t4l.46's idempotent retry, now conditional on that
	// box being young AND its agent being confirmed absent.
	adoptReuse adoptPath = iota
	// adoptReject answers with the 04 §6 wire contract ("already exists"),
	// which gc's exec proxy maps to ErrSessionExists: an agent is running in
	// that box (or the pack cannot tell either way), so the call is a
	// duplicate, not a retry.
	adoptReject
	// adoptSelfHealLaunched is the crash window the agent-absence rule
	// catches: the launch exec succeeded and confirmLaunched never landed, so
	// the binding claims launched=false while an agent really is running. The
	// pack records the marker it lost, then rejects — adopting would
	// re-stage-and-double-launch into a live agent.
	adoptSelfHealLaunched
	// adoptReap deregisters the box, tombstones the binding beside it, and
	// lets the caller dispatch fresh — executive ruling cr-mnl96 G2 (option
	// A, REAP FIRST): an aged or agent-less provisioned box is an ephemeral
	// runtime allocation, and the estate's answer to it is a record plus a
	// clean dispatch. This is never a bypass of liveChild: liveChild is the
	// gate that just said "this box is not the one to reuse".
	adoptReap
)

// classifyAdoption decides which adoptPath the live child b (found by
// liveChild for sessionName) gets, and returns the reason in words the
// decision log carries. Order matters: age is a cheap cluster-free test and
// is the rule the canary actually violated, so it runs before the in-box
// probe.
func (l *lifecycle) classifyAdoption(ctx context.Context, sessionName string, b binding) (adoptPath, string) {
	age := time.Since(b.CreatedAt).Round(time.Second)
	limit := l.adoptAgeLimit()
	if b.CreatedAt.IsZero() {
		// Nomad's jobs-list response does not carry a creation timestamp, so
		// a binding reconstructed from Meta cannot be sent through the age
		// gate with a fabricated zero/now value. Refuse explicitly; otherwise
		// an arbitrarily old child would look young and be adopted forever.
		return adoptReject, fmt.Sprintf("binding creation age is unknown; refusing adoption because the adoption age limit %s cannot be enforced", limit)
	}
	if time.Since(b.CreatedAt) > limit {
		return adoptReap, fmt.Sprintf("binding age %s exceeds the adoption age limit %s", age, limit)
	}
	alive, known := l.bindingAgentStatus(ctx, b)
	switch {
	case b.Launched && known && alive:
		return adoptReject, fmt.Sprintf("agent alive in the launched box (binding age %s)", age)
	case b.Launched && known && !alive:
		return adoptReap, "binding says launched but its in-box agent is gone: the box is a shell, not a session"
	case b.Launched:
		// Probe unavailable: unknown is not confirmed-absent. Same honesty
		// split opIsRunning applies (04 §6) — reaping on a transport fault
		// would destroy a live session because the API blipped.
		return adoptReject, fmt.Sprintf("agent liveness unknown (probe unavailable, binding age %s): keeping the already-exists contract", age)
	// An unlaunched box whose agent state could not be read is reaped, not
	// reused and not refused. Human ruling cr-gdeav.6.1-probe-absence-unknown:
	// every adopt read that cannot CONFIRM the agent absent is a reap trigger,
	// because the alternative reading of that silence is exactly the canary's —
	// a box that really was half-launched, its agent reset landing after the
	// partial launch was recorded, being re-staged and then launched into a
	// second time. known=false is both unread shapes: an alloc lookup that
	// returned nothing, and an in-box probe exec that never landed
	// (bindingAgentStatus reports unknown, never dead, for either).
	//
	// Not refuse, though refusing is the conservative-looking move. Refusing
	// means writing "already exists" into stderr, and gc's exec provider
	// INFERS runtime.ErrSessionExists from exactly that phrase
	// (pinned edge 8c73625b9 internal/runtime/exec/exec.go:142
	// startCollisionPhrases) — it is the only provider that infers the
	// sentinel from wording rather than returning it. And a session gc believes
	// is there is a session gc will launch into: launchAgent respawns the pane
	// of a box whose tmux session exists (same file, 291-295,
	// `tmux respawn-pane -k`). So the refusal is not a terminal read — it is a
	// claim that a session the pack could not measure is live and owned, and
	// the caller's next launch honours that claim by killing whatever pane is
	// in there and starting a second agent. Reap is also what this ruling
	// already orders for a box that is merely too old; an unreadable box is no
	// safer than an old one.
	//
	// The reap stays BOUNDED, which is what keeps this from being a blind
	// second dispatch: reapChild writes the tombstone, then opStop deregisters
	// and blocks on the terminal read, and adoptExisting returns that error
	// before the caller reaches dispatch. A box whose stop cannot be
	// confirmed therefore mints nothing; the fresh dispatch below happens only
	// over a box the pack has seen go terminal.
	case !known:
		return adoptReap, fmt.Sprintf("agent absence could not be confirmed (in-box probe or alloc lookup unavailable, binding age %s): reaping instead of re-staging a box this pack cannot measure", age)
	case known && alive:
		return adoptSelfHealLaunched, "binding says launched=false but an agent is running: the launch confirm write was lost"
	default:
		// Reached only with the probe's answer in hand: !Launched AND
		// known AND !alive, so this may claim what it says. The reuse the
		// ruling leaves standing is a reuse of a CONFIRMED absence, and the
		// age limit above is the bounded reaper either way.
		return adoptReuse, fmt.Sprintf("binding age %s within the limit %s and its agent is absent: re-driving this pack's own half against the box it already made", age, limit)
	}
}

// adoptAgeLimit resolves the configured adoption age limit: an unset (or
// nonsensically non-positive) value is the shipped default, never "unbounded".
func (l *lifecycle) adoptAgeLimit() time.Duration {
	if l.adoptMaxAge > 0 {
		return l.adoptMaxAge
	}
	return defaultAdoptMaxAge
}

// adoptExisting runs the shared decision for both doors and logs which path
// it took (rule 2's "log which path it took"). It reports reuse=true when the
// caller must finish the job against the existing box and must NOT dispatch;
// reuse=false with nil means the stale box is gone and the caller should
// dispatch a fresh one; a non-nil error ends the call with the box untouched.
func (l *lifecycle) adoptExisting(ctx context.Context, sessionName string, b binding, op string) (bool, error) {
	path, why := l.classifyAdoption(ctx, sessionName, b)
	switch path {
	case adoptReuse:
		l.logDecision("%s %q: path=adopt — %s", op, sessionName, why)
		return true, nil
	case adoptSelfHealLaunched:
		l.logDecision("%s %q: path=self-heal-launched-marker — %s", op, sessionName, why)
		if err := l.confirmLaunched(ctx, sessionName, b.WorkDir); err != nil {
			return false, err
		}
		return false, fmt.Errorf("session %q already exists", sessionName)
	case adoptReap:
		l.logDecision("%s %q: path=reap-then-provision-fresh — %s", op, sessionName, why)
		if err := l.reapChild(ctx, sessionName, b, why); err != nil {
			return false, err
		}
		return false, nil
	default:
		l.logDecision("%s %q: path=reject-already-exists — %s", op, sessionName, why)
		return false, fmt.Errorf("session %q already exists: %s", sessionName, why)
	}
}

// reapChild removes a stale box that the adoption gate refused to reuse: the
// tombstone record is written FIRST (the record is the artifact the ruling
// asks for, and a reap that dies after the deregister must still be citable),
// then opStop carries the stop-path evidence egress, the deregister, and the
// terminal confirmation, and finally removes the binding.
func (l *lifecycle) reapChild(ctx context.Context, sessionName string, b binding, why string) error {
	tomb := tombstone{
		SessionName:  sessionName,
		ChildJobID:   b.ChildJobID,
		AllocID:      l.liveAllocID(ctx, b),
		CreatedAt:    b.CreatedAt,
		Age:          time.Since(b.CreatedAt).Round(time.Second).String(),
		Launched:     b.Launched,
		AgentPID:     b.AgentPID,
		Reason:       why,
		TombstonedAt: time.Now().UTC(),
	}
	if err := l.sidecar.tombstone(tomb); err != nil {
		return fmt.Errorf("tombstoning stale box for session %q: %w", sessionName, err)
	}
	if err := l.opStop(ctx, sessionName); err != nil {
		return fmt.Errorf("reaping stale child %q of session %q (tombstone already written): %w", b.ChildJobID, sessionName, err)
	}
	return nil
}

// bindingAgentStatus runs the in-box liveness probe against b's current live
// alloc. It reports known=false — NOT dead — when the alloc or the probe
// could not be reached, so no caller mistakes a transport fault for a
// confirmed-absent agent (04 §6's honesty split).
func (l *lifecycle) bindingAgentStatus(ctx context.Context, b binding) (alive, known bool) {
	allocID := l.liveAllocID(ctx, b)
	if allocID == "" {
		return false, false
	}
	alive, err := l.probeAgentAlive(ctx, allocID, b.AgentPID)
	if err != nil {
		return false, false
	}
	return alive, true
}

// liveAllocID returns b's non-terminal alloc ID, or "" if there is none or the
// lookup failed. Best-effort by design: it feeds the in-box probe (whose
// caller treats "" as unknown) and the tombstone record (where a missing alloc
// ID weakens a receipt line, it does not make the reap wrong).
func (l *lifecycle) liveAllocID(ctx context.Context, b binding) string {
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return ""
	}
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			return a.ID
		}
	}
	return ""
}

// logDecision writes one line about a decision the pack took that is otherwise
// invisible from outside the box. Nil-safe: see lifecycle.logf.
func (l *lifecycle) logDecision(format string, args ...any) {
	if l.logf == nil {
		return
	}
	l.logf(format, args...)
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
	return l.markLaunched(ctx, sessionName, stageConfig{})
}

// opExec runs command inside sessionName's current alloc over the Nomad
// alloc-exec WebSocket (04 §3 exec row: "op exit = command exit",
// RPP-CONN-001). It works regardless of the launched marker — this is
// exactly the "box exec-able" half of the provision contract
// (RPP-PROVISION-001): a provisioned-but-not-launched box still answers
// exec. The one stateful exception is gc's canonical separable-transport
// tmux launch: after that exact command succeeds, the pack must confirm the
// launch in its private sidecar just as opStart does, or its own is-running
// operation immediately reports the newly launched session missing.
func (l *lifecycle) opExec(ctx context.Context, sessionName string, command []string) (int, []byte, error) {
	transportCommand, transportLaunch := separableTransportLaunch(command)
	if transportLaunch {
		unlock, err := l.lockSession(ctx, sessionName)
		if err != nil {
			return 0, nil, err
		}
		defer unlock()
	}

	if transportLaunch {
		// Preserve GC's canonical launch argv, including its rendered pane
		// command, while adding only the in-box staged-environment and cwd
		// preludes the Nomad boundary requires.
		if err := l.markTransportLaunched(ctx, sessionName, transportCommand); err != nil {
			return 0, nil, fmt.Errorf("launching transport session %q: %w", sessionName, err)
		}
		return 0, nil, nil
	}

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

// isSeparableTransportLaunch recognizes only the two canonical tmux argv
// forms gc's exec transport emits from launchAgent. Ordinary exec remains
// stateless, including unrelated tmux commands.
func isSeparableTransportLaunch(command []string) bool {
	_, ok := separableTransportLaunch(command)
	return ok
}

// separableTransportLaunch returns GC's canonical tmux launch argv after
// unwrapping the exec wire form. The final pane command remains part of the
// returned argv so the in-box launch path can execute GC's rendered command.
func separableTransportLaunch(command []string) ([]string, bool) {
	// RPP v0's exec op carries argv as one shellQuote-produced string on
	// stdin. main preserves that contract by invoking /bin/sh -c, so unwrap
	// only that exact, single-quoted grammar before matching the launch argv.
	if len(command) == 3 && command[0] == "/bin/sh" && command[1] == "-c" {
		parsed, ok := parseExecWireArgv(command[2])
		if !ok {
			return nil, false
		}
		command = parsed
	}
	if len(command) != 6 || command[0] != "tmux" || command[4] != tmuxSessionName || command[5] == "" {
		return nil, false
	}
	if !((command[1] == "new-session" && command[2] == "-d" && command[3] == "-s") ||
		(command[1] == "respawn-pane" && command[2] == "-k" && command[3] == "-t")) {
		return nil, false
	}
	return command, true
}

// parseExecWireArgv decodes the deliberately tiny grammar produced by gc's
// exec shellQuote: every argument is single-quoted, spaces separate arguments,
// and an embedded quote is encoded as:
//
//	'\''
//
// It is not a general shell parser; rejecting anything outside that grammar
// keeps launch recognition narrower than the command the shell itself will
// accept.
func parseExecWireArgv(command string) ([]string, bool) {
	var argv []string
	for command != "" {
		if command[0] != '\'' {
			return nil, false
		}
		command = command[1:]
		var arg strings.Builder
		for {
			end := strings.IndexByte(command, '\'')
			if end < 0 {
				return nil, false
			}
			arg.WriteString(command[:end])
			command = command[end+1:]
			if strings.HasPrefix(command, `\''`) {
				arg.WriteByte('\'')
				command = command[3:]
				continue
			}
			argv = append(argv, arg.String())
			break
		}
		if command == "" {
			return argv, true
		}
		if command[0] != ' ' {
			return nil, false
		}
		command = command[1:]
	}
	return argv, true
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
	if _, live, err := l.liveChild(ctx, sessionName); err != nil {
		return err
	} else if live {
		return fmt.Errorf("session %q already exists", sessionName)
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
	spec := l.parentJobSpec()

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

func (l *lifecycle) parentJobSpec() nomadJob {
	if l.agentImage != "" {
		return parentJobSpecWithAgentImage(l.client.namespace, l.nodePool, l.parentJobID, l.logShipper, l.agentImage, l.placement)
	}
	return parentJobSpecWithArtifactVolume(l.client.namespace, l.nodePool, l.parentJobID, l.logShipper, l.artifactVolumeSource, l.placement)
}

// recoverBinding reconstructs the part of a sidecar binding that Nomad's
// native children-of-parent jobs list can answer. The list is intentionally
// filtered by the child namespace as well as gc_session: the request's
// namespace query is not a substitute for checking each returned row.
//
// Exactly one attributable, non-terminal child is required. Zero matches,
// multiple matches, a terminal match, or a missing nonce are all loud errors;
// the caller must never guess a child or dispatch around a disputed binding.
// CreatedAt remains zero because the jobs-list response does not provide a
// creation timestamp. Adoption handles that unknown age explicitly instead
// of silently bypassing the bounded adoption-age rule.
func (l *lifecycle) recoverBinding(ctx context.Context, sessionName string, prior *binding) (binding, error) {
	children, err := l.client.listChildJobs(ctx, l.parentJobID)
	if err != nil {
		return binding{}, fmt.Errorf("recovering sidecar binding for session %q: %w", sessionName, err)
	}

	matches := make([]childJob, 0, 1)
	for _, child := range children {
		if child.Namespace != l.client.namespace || child.Meta["gc_session"] != sessionName {
			continue
		}
		matches = append(matches, child)
	}
	switch len(matches) {
	case 0:
		return binding{}, fmt.Errorf("%w for session %q in parent %q", errBindingNoMatch, sessionName, l.parentJobID)
	case 1:
		// Continue with the guard checks below.
	default:
		return binding{}, fmt.Errorf("%w for session %q in parent %q: found %d children", errBindingAmbiguous, sessionName, l.parentJobID, len(matches))
	}

	child := matches[0]
	if child.Terminal {
		return binding{}, fmt.Errorf("%w for session %q: child %q", errBindingTerminal, sessionName, child.ID)
	}
	nonce := strings.TrimSpace(child.Meta["gc_nonce"])
	if nonce == "" {
		return binding{}, fmt.Errorf("sidecar binding recovery: child %q for session %q has no gc_nonce", child.ID, sessionName)
	}

	recovered := binding{SessionName: sessionName, ChildJobID: child.ID, Namespace: child.Namespace, Nonce: nonce}
	if prior != nil {
		// A dispatch-intent record may already carry the local residue that
		// Nomad cannot answer. Preserve it while replacing only the fields
		// recovered from Meta. currentAlloc can retain an intent's timestamp
		// and staging state; a nil prior means the sidecar record was
		// completely lost.
		base := *prior
		recovered.CreatedAt = base.CreatedAt
		recovered.EgressComplete = base.EgressComplete
		recovered.EvidenceLost = base.EvidenceLost
		recovered.StagePending = base.StagePending
		recovered.Launched = base.Launched
		recovered.AgentPID = base.AgentPID
		recovered.WorkDir = base.WorkDir
		recovered.PromptedAt = base.PromptedAt
		recovered.PromptStatus = base.PromptStatus
		recovered.PromptPending = base.PromptPending
	}
	if err := l.sidecar.save(recovered); err != nil {
		return binding{}, fmt.Errorf("repairing sidecar binding for session %q: %w", sessionName, err)
	}
	return recovered, nil
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
func (l *lifecycle) launch(ctx context.Context, sessionName string, env map[string]string, workDir string) error {
	return l.launchWithCommand(ctx, sessionName, buildLaunchCommand(env, workDir))
}

// launchWithCommand executes a pack-owned detached tmux launch command into
// sessionName's current alloc. Keeping this separate lets opExec preserve GC's
// new-session versus respawn transport verb without handing GC's raw command
// a way around staged bootstrap.
func (l *lifecycle) launchWithCommand(ctx context.Context, sessionName string, command []string) error {
	return retryAllocExecReady(ctx, func() error {
		allocID, err := l.currentAlloc(ctx, sessionName)
		if err != nil {
			return err
		}
		exitCode, dbgOut, err := l.client.execAlloc(ctx, allocID, execTaskName, command)
		if err != nil {
			return fmt.Errorf("launching session %q: %w", sessionName, err)
		}
		if exitCode != 0 {
			return fmt.Errorf("launching session %q: launch command exited %d DEBUGOUT=%q", sessionName, exitCode, string(dbgOut))
		}
		return nil
	})
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
// launch) and opRelaunch (launch only, no re-dispatch). cfg's argvSafe env subset rides the launch command
// (buildLaunchCommand) and cfg.WorkDir chooses the pane's cwd
// (paneCwdPrelude). opRelaunch passes the zero config, since no fresh env
// or work dir accompanies a relaunch, and falls back to the WorkDir the
// binding recorded. Once the launch is recorded, startupPrime owes the
// pane its first prompt exactly once per session (startup.go) - which is
// the whole point of this call site: it is the only place in the pack
// that runs after every successful launch.
func (l *lifecycle) markLaunched(ctx context.Context, sessionName string, cfg stageConfig) error {
	workDir := cfg.WorkDir
	if workDir == "" {
		// A relaunch - or a start whose caller sent no work_dir - still gets the
		// directory the box was first launched in, instead of "/".
		if b, ok, err := l.sidecar.load(sessionName); err == nil && ok {
			workDir = b.WorkDir
		}
	}
	if err := l.launch(ctx, sessionName, cfg.Env, workDir); err != nil {
		return err
	}
	if err := l.confirmLaunched(ctx, sessionName, workDir); err != nil {
		return err
	}
	return l.startupPrimeDetached(ctx, sessionName, cfg)
}

// markTransportLaunched preserves GC's canonical tmux transport verb and
// rendered pane command while adding the staged, in-box environment prelude.
// The caller already holds sessionName's lock.
//
// Once the launch is confirmed, it hands the pane off to the detached prime
// window exactly as markLaunched does (cr-gdeav.6.12/6.13): this is gc's
// proc.provision path, which stages the startup prompt separately
// (opProvisionWithConfig -> savePromptPending) from the launch it later
// drives through this transport handshake, so the prompt fields ride the
// binding's PromptPending — recorded at stage time — rather than this call's
// own argv, which carries only the transport verb. startupPrimeDetached
// returns as soon as the window is spawned, so this op keeps returning the
// instant launch is confirmed, same as before any priming existed
// (cr-gdeav.6.13: a blocking wait here is what put attempt 6's start past
// gc's async-start budget and looped a fresh dispatch every ~30s).
func (l *lifecycle) markTransportLaunched(ctx context.Context, sessionName string, transport []string) error {
	if len(transport) != 6 {
		return fmt.Errorf("invalid transport launch argv length %d", len(transport))
	}
	workDir := ""
	b, ok, err := l.sidecar.load(sessionName)
	if err == nil && ok {
		workDir = b.WorkDir
	}
	if err := l.launchWithCommand(ctx, sessionName, buildLaunchCommandForTmuxWithPaneCommand(nil, transport[1:5], workDir, transport[5])); err != nil {
		return err
	}
	if err := l.confirmLaunched(ctx, sessionName, workDir); err != nil {
		return err
	}
	cfg := stageConfig{}
	if pending, ok, err := l.sidecar.load(sessionName); err == nil && ok && pending.PromptPending != nil {
		cfg = *pending.PromptPending
	}
	return l.startupPrimeDetached(ctx, sessionName, cfg)
}

// confirmLaunched records a launch that has already succeeded. Its caller
// holds sessionName's lock: opStart/markLaunched hold it end-to-end, while
// opExec acquires it only for gc's canonical transport-launch command.
func (l *lifecycle) confirmLaunched(ctx context.Context, sessionName string, workDir string) error {
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
	if workDir != "" && b.WorkDir != workDir {
		b.WorkDir = workDir
	}
	return l.sidecar.save(*b)
}

// stageTracked runs stage under the sidecar's StagePending marker, so a
// staging failure leaves a binding that records, on its face, that this
// pack's own staging half never finished against the box it dispatched. A
// cfg with nothing to materialize skips the marker entirely: stage is a no-op there, so
// provision/start keep behaving exactly as they did before the marker
// existed when no config rides the call.
func (l *lifecycle) stageTracked(ctx context.Context, sessionName string, cfg stageConfig) error {
	if err := l.savePromptPending(sessionName, cfg); err != nil {
		return err
	}
	if !stagesAnything(cfg) {
		return l.stage(ctx, sessionName, cfg)
	}
	if err := l.setStagePending(sessionName, true); err != nil {
		return err
	}
	if err := l.stage(ctx, sessionName, cfg); err != nil {
		return err
	}
	return l.setStagePending(sessionName, false)
}

// savePromptPending persists cfg's startup-prompt/priming subset onto
// sessionName's binding (cr-gdeav.6.12), so a later transport launch through
// opExec — which runs with no stageConfig of its own — can still deliver the
// prompt gc sent at provision/start time. A cfg that owes no prompt
// (cfg.startupPrompt() is empty) is a complete no-op: provision/start behave
// exactly as before this field existed on every ordinary call.
func (l *lifecycle) savePromptPending(sessionName string, cfg stageConfig) error {
	if text, _ := cfg.startupPrompt(); text == "" {
		return nil
	}
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before its startup prompt could be recorded", sessionName)
	}
	subset := cfg.promptSubset()
	b.PromptPending = &subset
	return l.sidecar.save(*b)
}

// setStagePending records (or clears) the mid-staging marker on
// sessionName's binding, re-reading it first so a concurrent writer's other
// fields are preserved.
func (l *lifecycle) setStagePending(sessionName string, pending bool) error {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q binding vanished before staging could be recorded", sessionName)
	}
	if b.StagePending == pending {
		return nil
	}
	b.StagePending = pending
	return l.sidecar.save(*b)
}

// stagesAnything reports whether cfg — plus the process-env extra secret
// keys stage() merges in (GC_NOMAD_EXTRA_SECRET_KEYS) — has any workspace or
// secret payload to materialize, i.e. whether stage will make an exec call
// that can fail partway.
func stagesAnything(cfg stageConfig) bool {
	if len(cfg.CopyFiles) > 0 {
		return true
	}
	secretEnv, _ := mergeExtraSecretEnv(cfg.Env, extraSecretKeys)
	secretEnv = stripLocalCityStoreEnv(secretEnv)
	secretEnv = mergeLaunchOptionEnv(secretEnv, cfg.Command)
	return len(secretEnv) > 0
}

// stage materializes cfg's workspace files and secret env vars into
// sessionName's current alloc (NRT-P1-06, 04 §5 data contract) over two
// tar-over-exec-stdin calls: cfg.CopyFiles read off the orchestrator and
// archived by copyFilesToStageFiles, extracted under cfg.WorkDir, and cfg.Env's values extracted under
// $NOMAD_SECRETS_DIR both as individual files (legacy/value-specific access)
// and as a sourceable env.sh contract for the launched process. Neither
// channel ever touches the job spec, argv, or the sidecar — the only place
// secret BYTES appear is the alloc-exec WebSocket stream itself (accepted
// residual, 05 §7 R9). A zero-value cfg is a no-op, so start/provision behave
// exactly as before staging landed when a caller sends no config.
func (l *lifecycle) stage(ctx context.Context, sessionName string, cfg stageConfig) error {
	workspaceFiles, err := copyFilesToStageFiles(cfg.CopyFiles)
	if err != nil {
		return fmt.Errorf("staging workspace for %q: %w", sessionName, err)
	}
	if len(workspaceFiles) > 0 {
		data, err := buildTar(workspaceFiles)
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

	secretEnv, extraKeys := mergeExtraSecretEnv(cfg.Env, extraSecretKeys)
	secretEnv = stripLocalCityStoreEnv(secretEnv)
	secretEnv = mergeLaunchOptionEnv(secretEnv, cfg.Command)

	var secretFiles []stageFile
	for k, v := range secretEnv {
		if !extraKeys[k] && envArgvSafe(k) {
			continue
		}
		secretFiles = append(secretFiles, stageFile{Path: k, Content: []byte(v), Mode: 0o600})
	}
	if len(secretEnv) > 0 {
		envScript := buildEnvScript(remoteContextEnvScriptInput(secretEnv))
		// The remote-context bridge (cr-gdeav.6.15) rides the same env.sh:
		// GC_HOME/GC_CITY_CONTEXT are a no-op append when GC_CITY_NAME or
		// GC_CITY_URL is absent from the wire, so this costs nothing for a
		// deployment that has not opted in.
		envScript = append(envScript, buildRemoteContextEnvLines(secretEnv)...)
		secretFiles = append(secretFiles, stageFile{
			Path:    stagedEnvScriptName,
			Content: envScript,
			Mode:    0o600,
		})
		// contexts.toml itself - gc's own $GC_HOME/contexts.toml schema,
		// hand-written since this pack cannot import gc's clientcontext
		// package. Also a no-op append on the same condition.
		if ctxFile := buildRemoteContextFile(secretEnv); ctxFile != nil {
			secretFiles = append(secretFiles, *ctxFile)
		}
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
	return retryAllocExecReady(ctx, func() error {
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
	})
}

// retryAllocExecReady retries only the failures that are expected while a
// freshly dispatched allocation becomes exec-ready: no allocation is visible
// yet, or the exec stream is torn down before the task is ready. Command exit
// failures and other provider errors are returned immediately so a real fault
// is not hidden behind a retry delay.
func retryAllocExecReady(ctx context.Context, operation func() error) error {
	var lastErr error
	for attempt := 0; attempt < allocExecReadyAttempts; attempt++ {
		err := operation()
		if err == nil || !allocReadyRetryable(err) || attempt == allocExecReadyAttempts-1 {
			return err
		}
		lastErr = err

		timer := time.NewTimer(allocExecReadyDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// allocReadyRetryable classifies the transport failures a torn-down exec
// stream can surface. Which one the client sees depends on whether it was
// reading or writing when the peer went away, and on how many hops sit in
// between: reading gives EOF (clean close before any frame),
// ErrUnexpectedEOF (close partway through a frame the codec had committed
// to reading), or ECONNRESET; writing gives EPIPE — the shape fnrt-t4l.18
// hit on the lab, where the staging tar's own stdin frames raced a
// half-closed proxy tunnel ("writing exec stdin-close frame: broken pipe").
// All of them mean the same thing to a caller: the box is not exec-ready
// yet, re-dial and try again. A genuinely dead stream just exhausts the
// bounded retry and surfaces the same error.
func allocReadyRetryable(err error) bool {
	return err != nil && (errors.Is(err, errAllocNotReady) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "broken pipe"))
}

// errSessionNotFound marks a currentAlloc failure caused by the session
// simply not existing (never provisioned, or already stopped) rather than a
// transport/lookup fault — the distinction opNudge and opInterrupt need to
// honor the protocol's best-effort convention (docs/reference/exec-session-
// provider.md "Best-effort interrupt/nudge: Return 0 even if the session
// doesn't exist").
var errSessionNotFound = errors.New("session not found")

// errAllocNotReady marks the narrow post-dispatch window where the sidecar
// binding exists but Nomad has not exposed its allocation yet. It is joined
// with errSessionNotFound below so callers that need the existing
// not-found/best-effort semantics keep seeing that classification.
var errAllocNotReady = errors.New("allocation not ready")

// currentAlloc resolves the non-terminal Nomad alloc ID backing
// sessionName's current child job, sidecar-binding-primary (04 §2.1 rule 1),
// recovering a missing binding from Nomad Meta under the session lock.
func (l *lifecycle) currentAlloc(ctx context.Context, sessionName string) (string, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return "", err
	}
	if !ok || b.ChildJobID == "" {
		// Recovery is a read-modify-write against the same session binding
		// that dispatch uses. Acquire the existing cross-process lock before
		// re-reading so a concurrent fresh dispatch cannot interleave with
		// the Meta lookup and atomic repair. The normal provision/start doors
		// already hold this lock; their liveChild path repairs first, so this
		// branch is for direct resolution verbs such as exec/peek/nudge.
		unlock, err := l.lockSession(ctx, sessionName)
		if err != nil {
			return "", err
		}
		defer unlock()
		b, ok, err = l.sidecar.load(sessionName)
		if err != nil {
			return "", err
		}
		if ok && b.ChildJobID != "" {
			// Another process repaired or dispatched the binding while this
			// caller waited for the lock. Use its authoritative record.
		} else {
			var recovered binding
			if ok {
				recovered, err = l.recoverBinding(ctx, sessionName, b)
			} else {
				recovered, err = l.recoverBinding(ctx, sessionName, nil)
			}
			if err != nil {
				if errors.Is(err, errBindingNoMatch) || errors.Is(err, errBindingTerminal) {
					return "", fmt.Errorf("session %q has no provisioned box: %w", sessionName, errSessionNotFound)
				}
				return "", err
			}
			b = &recovered
		}
	}

	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return "", fmt.Errorf("resolving alloc for session %q: %w", sessionName, err)
	}
	if len(allocs) == 0 {
		return "", fmt.Errorf("session %q has no allocation yet: %w; %w", sessionName, errSessionNotFound, errAllocNotReady)
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

const (
	// logShipperFlushTimeout bounds the wait for the vector process to finish
	// its graceful shutdown after it has been signaled. It is deliberately
	// the same bounded window Nomad gives the shipper after the agent task.
	logShipperFlushTimeout = logShipperFlushWindow
	logShipperPollInterval = 25 * time.Millisecond
)

// logShipperFlushCommand requests a graceful vector shutdown. The wrapper
// owns the vector child and publishes its pid inside the shared allocation;
// signaling that child lets vector flush its buffers while leaving the
// session task alive long enough for this method to observe completion.
const logShipperFlushCommand = `set -eu
pid_file="$NOMAD_ALLOC_DIR/` + logShipperPIDFile + `"
flush_request="$NOMAD_ALLOC_DIR/` + logShipperFlushRequest + `"
if [ ! -s "$pid_file" ]; then
  exit 1
fi
: >"$flush_request"
kill -TERM "$(cat "$pid_file")"
`

const logShipperFlushProbe = `test -f "$NOMAD_ALLOC_DIR/` + logShipperFlushComplete + `"`

// flushLogShipper requests and confirms the optional log-shipper's final
// flush. A failed request, a non-zero request command, or a timeout waiting
// for the wrapper's completion marker is a flush failure; callers record that
// failure as evidence_lost and continue the stop path.
func (l *lifecycle) flushLogShipper(ctx context.Context, allocID string) error {
	if !l.logShipper.enabled() {
		return nil
	}
	exitCode, out, err := l.client.execAlloc(ctx, allocID, logShipperTaskName, []string{"/bin/sh", "-c", logShipperFlushCommand})
	if err != nil {
		return fmt.Errorf("flushing log shipper: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("flushing log shipper: request exited %d: %s", exitCode, out)
	}

	flushCtx, cancel := context.WithTimeout(ctx, logShipperFlushTimeout)
	defer cancel()
	for {
		exitCode, _, err := l.client.execAlloc(flushCtx, allocID, execTaskName, []string{"/bin/sh", "-c", logShipperFlushProbe})
		if err != nil {
			return fmt.Errorf("flushing log shipper: waiting for completion: %w", err)
		}
		if exitCode == 0 {
			return nil
		}
		select {
		case <-flushCtx.Done():
			return fmt.Errorf("flushing log shipper: completion timeout: %w", flushCtx.Err())
		case <-time.After(logShipperPollInterval):
		}
	}
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

	// Best-effort: end the agent's tmux session before the task itself is
	// killed below (NRT-P2-06.1). The task's own exit is independent of
	// this — jobspec.go's sessionSupervisorScript traps SIGTERM and exits 0
	// on its own once deregister signals it — but tmux's server process is
	// a detached daemon outside the task's process tree once launch starts
	// it (buildLaunchCommand), so nothing else ever reaps it. Any failure
	// here (alloc already terminal, transport fault) is ignored — the
	// deregister call below is what actually confirms terminal either way.
	_ = l.runDrivingVerb(ctx, sessionName, "stop-kill-session", []string{"tmux", "kill-session", "-t", tmuxSessionName})

	// Ask the optional log shipper to flush before egress makes the
	// allocation's files unreachable. A failed flush is evidence loss, but
	// it must not prevent the local egress attempt or the deregister below.
	needEgress := !b.EgressComplete && !b.EvidenceLost
	if l.logShipper.enabled() && !b.EvidenceLost {
		allocID, err := l.currentAlloc(ctx, sessionName)
		if err != nil {
			b.EvidenceLost = true
		} else if err := l.flushLogShipper(ctx, allocID); err != nil {
			b.EvidenceLost = true
		}
	}

	// Egress transcript/evidence before deregister makes them unreachable.
	// A failing egress gets egressMaxAttempts bounded retries (R2b-04); if
	// it still hasn't succeeded, stop PROCEEDS rather than wedging behind
	// evidence collection — it just marks the tombstone evidence_lost
	// instead. Either outcome is receipted in the sidecar (still resident)
	// before deregister, so a stop that then crashes before deregister
	// retries idempotently: it neither re-attempts a marker that already
	// gave up nor re-copies files it already egressed (NRT-P1-07).
	if needEgress {
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
	}
	if b.EgressComplete || b.EvidenceLost {
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
	// Best-effort: pick up the detached prime window's marker files if this
	// session is still owed one (cr-gdeav.6.13). A no-op exec-free check
	// once PromptedAt is set or no prompt was ever staged, so this costs
	// nothing on the common path and never affects the liveness answer.
	l.syncPromptedMarker(ctx, sessionName)
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
