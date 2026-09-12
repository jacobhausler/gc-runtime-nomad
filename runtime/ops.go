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
	// becoming usable. That window is now only the websocket handshake:
	// whether the task is up at all is settled BEFORE the first exec by
	// awaitExecAllocReady, not by dialling a task that has not started.
	// This stays deliberately short, not a general retry for provider or
	// command failures.
	allocExecReadyAttempts = 20
	allocExecReadyDelay    = 250 * time.Millisecond

	// allocTaskRunningTimeout and allocTaskRunningPoll bound the wait for
	// execTaskName's task to reach Nomad state "running" before the pack execs
	// into its allocation. currentAlloc answers as soon as an alloc is
	// non-terminal, and non-terminal includes a task still in setup: the
	// 2026-09-10 disposable-city proof watched one go Received 16:16:08 ->
	// Task Setup 16:16:09 -> killed 16:16:15 without ever starting, so the
	// first staging exec hit "reading exec frame: EOF", the provider errored
	// after 5.2 s, and gc reaped and re-dispatched 26 boxes in five minutes.
	// 180 s is the budget that bead names: generous for task setup on this
	// estate's nodes, bounded, and cancelled with the caller's context.
	// allocPlacementTimeout bounds how long provision waits for Nomad to place
	// an allocation before calling the dispatch a failure. Placement is fast
	// when it can happen at all; when it cannot — no eligible node, an image
	// the node has no way to pull — waiting longer does not change the answer,
	// it only delays a report the operator needs. Short on purpose.
	allocPlacementTimeout = 30 * time.Second
	allocPlacementPoll    = 1 * time.Second

	allocTaskRunningTimeout = 180 * time.Second
	allocTaskRunningPoll    = 500 * time.Millisecond
)

// allocStateRunning is the Nomad task state that means the task process is up
// and can take an exec. Anything else -- "pending", "starting", "blocked",
// "dead" -- means an exec stream the box will tear down under us.
const allocStateRunning = "running"

var (
	// errBindingNoMatch is returned when Nomad's children-of-parent read has
	// no child attributable to the requested session. Callers that are
	// looking for a current allocation translate this into the existing
	// session-not-found contract; the recovery helper keeps it distinct so a
	// caller cannot mistake an empty read for a recovered binding.
	errBindingNoMatch = errors.New("sidecar binding recovery: no matching child")
	// errBindingAmbiguous prevents a recovery from guessing when more than one
	// live child carries the same session Meta. The safe answer is a loud
	// refusal; terminal residue is excluded from that count by recoverBinding,
	// because a child whose alloc already finished cannot compete for the
	// binding and would otherwise make every retry of a live session ambiguous.
	errBindingAmbiguous = errors.New("sidecar binding recovery: matching children are ambiguous")
	// errBindingTerminal prevents a terminal Meta-matching child from being
	// silently skipped. A terminal match is evidence of a stale or disputed
	// binding, not permission to dispatch another child.
	errBindingTerminal = errors.New("sidecar binding recovery: matching child is terminal")
)

// lifecycle implements the RPP ops this pack scopes so far: the four
// dispatch/deregister/blocking-read lifecycle ops (start, stop, is-running,
// list-running), the provision/launch split and warm relaunch
// (provision, exec, relaunch), and workspace/secret staging
// (stage) — using the sidecar for the session_name ->
// child-job-ID binding ("sidecar binding is primary") and its
// launched marker.
type lifecycle struct {
	client      *client
	sidecar     *sidecar
	parentJobID string

	// nodePool is the Nomad node pool the parent job registers into (empty
	// keeps Nomad's own "default" pool). Unlike namespace it carries no
	// per-request query param (nsQuery has no node-pool analog on
	// any route this pack calls) — it only ever rides the parent job body
	// parentJobSpec builds, so it lives here rather than on client.
	nodePool string

	// egressDir is the local sink directory for the stop-path transcript/
	// evidence egress. Empty disables egress entirely — a
	// deployment that never sets GC_NOMAD_EGRESS_DIR gets the pre-egress
	// stop behavior unchanged. Sinks beyond a local directory (remote
	// upload, etc.) are out of scope for this bead.
	egressDir string

	// forbidRegistration, when true, makes ensureParentRegistered fail
	// closed instead of ever attempting to register the parent job — for a
	// deployment whose runtime token deliberately lacks the submit-job
	// capability (the lab ACL model) and wants a clear
	// config-time guarantee that dispatch never even probes that capability
	// boundary, trading a start failure with a precise cause for the
	// default behavior of attempting register and surfacing its 403.
	forbidRegistration bool

	// logShipper configures the optional log-shipper task added to the
	// session task group's jobspec. Zero value (Sink=="")
	// disables it — parentJobSpec's default single-task group is
	// unaffected. It only ever rides the parent job body parentJobSpec
	// builds, same as nodePool above.
	logShipper logShipperConfig

	// agentImage names the prebaked Docker image that owns the agent toolchain.
	// It is required — the Docker task is the only session shape — and
	// newLifecycle fails closed when GC_NOMAD_AGENT_IMAGE is unset,
	// because the legacy exec task it used to select has been deleted.
	agentImage string

	// diagnostics is the durable half of logf — the file under the sidecar
	// dir that survives gc discarding this process's stderr.
	diagnostics diagnosticsSink

	// placement is the deployment-configured DNS servers and node constraints
	// that ride the parent job body parentJobSpec builds, read
	// from GC_NOMAD_DNS_SERVERS / GC_NOMAD_NODE_CONSTRAINT by newLifecycle.
	// Zero value renders the jobspec as it was before placement config
	// existed — the same
	// optional-stanza shape as logShipper above.
	placement placementConfig

	// adoptMaxAge bounds how old a live-but-unlaunched box may be before
	// provision/start refuse to ADOPT it and reap it instead.
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
// concurrent same-name Start must go through. It has to be
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
// The agent-capture trio is written inside the allocation by
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
// so after deregistration the three channels are simply gone (forensics on
// a lost box found all of them empty). A lane whose capture block has not
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
// name: every carrier verb and the launch command target
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
// (measured on tmux 3.7b): an agent launched that way had no
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
// directory. Nothing cd'd the pane before, so
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

// extraSecretKeys is the parsed name list from GC_NOMAD_EXTRA_SECRET_KEYS
// (comma-separated), read once at init
// — a deployment-configured bridge letting the supervisor's own process env
// (e.g. via ~/.gc/secrets.env passthrough) deliver a named secret into a
// session's staged environment when gc's own city.toml provider env map has
// no way to interpolate it. Unset/empty means no extra keys,
// so stage() behaves exactly as before this seam landed — the rollback
// path.
var extraSecretKeys []string

func init() {
	configureExtraSecretKeys()
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

// defaultTmuxLaunchArgs is the canonical detached tmux operation the pack uses
// to start a provisioned box. It is the same operation gc itself emits on the
// separable transport path (separableTransportLaunch), which is why both launch
// surfaces can compose through one builder.
var defaultTmuxLaunchArgs = []string{"new-session", "-d", "-s", tmuxSessionName}

// buildLaunchCommandForPane composes the detached tmux launch for an explicit
// pane body. It is the single composition point of the two launch surfaces:
// the pack contributes the value-safe env wrapper, the argvSafe `-e` pairs, and
// the in-pane staged-env plus cwd preludes, and the CALLER owns the pane body:
// gc's rendered provider command, on the docker-driver (prebaked image) task
// that is the only session shape.
func buildLaunchCommandForPane(env map[string]string, workDir, paneCommand string) []string {
	return buildLaunchCommandForTmuxWithPaneCommand(env, defaultTmuxLaunchArgs, workDir, paneCommand)
}

// buildLaunchCommandForTmuxWithPaneCommand uses the canonical tmux operation
// and runs paneCommand after the pack's in-pane staged-environment and cwd
// preludes. paneCommand is always gc's OWN rendered launch command: the
// deployment bootstrap seam (GC_NOMAD_AGENT_LAUNCH_SCRIPT) is deleted
// outright, so the docker task's pane has exactly one body, and an empty
// rendered command fails the launch closed at launch() rather than falling
// back to a placeholder shell.
//
// env's argvSafe-classified entries (envArgvSafe) ride as `-e
// KEY=VALUE` on this argv — safe by construction, since envArgvSafe's whole
// job is excluding anything credential-shaped from ever reaching argv. The
// wrapper sources the value-safe env.sh produced by stage
// before invoking tmux, and the pane body re-sources it INSIDE the pane
// (paneStagedEnvPrelude), because tmux gives a pane the server's environment
// and never the client's; keys are sorted so the command is deterministic.
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
	// The pane sources the staged environment itself (see
	// paneStagedEnvPrelude): the client-side wrapper cannot reach it. The cwd
	// prelude runs after it and before paneCommand, so gc's rendered command
	// starts in a directory the alloc actually has (paneCwdPrelude). There is no
	// placeholder-pane shape left to fall back to: launch() has
	// already failed closed on an empty rendered command.
	cmd = append(cmd, "/bin/sh", "-c", paneStagedEnvPrelude+paneCwdPrelude(workDir)+paneCommand)
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
// though the box already answers exec.
//
// Provision is idempotent over the box it already made: a session whose
// bound child is still live but NOT launched is ADOPTED (staging re-run
// against it) rather than rejected. That is the idempotent-retry fix — a
// provision that dispatched its child and then never got its launch
// confirmed leaves exactly that state, and a controller that re-drives
// provision every tick must be able to finish the job instead of collecting
// "already exists" forever (61 of them in the DF-03 canary).
//
// A session whose child is live AND launched is still rejected: an agent is
// running in that box, so provisioning over it is a duplicate, not a retry.
// That rejection's stderr message MUST contain "already exists" (a
// wire-contract constant: gc's exec proxy infers ErrSessionExists
// from that exact phrase).
func (l *lifecycle) opProvision(ctx context.Context, sessionName string) error {
	return l.opProvisionWithConfig(ctx, sessionName, stageConfig{})
}

// opProvisionWithConfig is opProvision plus staging: once the box
// is dispatched (and therefore exec-able), cfg's
// workspace files and secret env vars are materialized into it before this
// returns — staging does not wait for launch, since a provisioned-but-not-
// launched box already answers exec and the agent bootstrap command started
// at launch time expects its workspace and secrets to already be in place.
// warnDeclaredSecrets reports declared-but-absent secret keys once per session
// op, independent of whether anything is staged.
//
// It sits here rather than on the staging path because staging is CONDITIONAL:
// stage() runs only when there is something to materialize, and a session
// whose only declared keys are all missing has nothing — so the warning was
// silent in exactly the case it exists for. Found by asking what this fix
// would look like if it had silently failed.
func (l *lifecycle) warnDeclaredSecrets(sessionName string, cfg stageConfig) {
	merged, _, absent := mergeExtraSecretEnv(cfg.Env, extraSecretKeys)
	// l.durableWarn(), not os.Stderr: both of these describe a box that comes
	// up healthy and cannot work, which is the state least reconstructable
	// after the fact and the one gc's exec transport discards.
	w := l.durableWarn()
	warnAbsentSecretKeys(w, sessionName, absent)
	warnIncompleteCityTarget(w, sessionName, merged)
}

// warnIncompleteCityTarget names a remote city target that is half-configured.
//
// buildRemoteContextFile stages a remote context only when BOTH GC_CITY_NAME
// and GC_CITY_URL are present, and returns nil otherwise — correctly, because
// neither half alone describes a city. But nil is also what a deployment that
// never opted in returns, so the two were indistinguishable and the
// half-configured case was dropped in silence: the box comes up, the agent
// runs, and every gc call inside it that needs the city fails with nothing
// anywhere explaining that the pack decided not to stage a context.
//
// That is the "unreachable city" symptom with a configuration cause, and the
// stager is the layer that knows: it is holding both halves and can see that
// exactly one arrived. Names the missing KEY, never the value of the present
// one.
func warnIncompleteCityTarget(w io.Writer, sessionName string, env map[string]string) {
	name := strings.TrimSpace(env["GC_CITY_NAME"])
	url := strings.TrimSpace(env["GC_CITY_URL"])
	switch {
	case name != "" && url != "":
		return // complete
	case name == "" && url == "":
		return // never opted in; the common case
	}
	missing, present := "GC_CITY_NAME", "GC_CITY_URL"
	if url == "" {
		missing, present = "GC_CITY_URL", "GC_CITY_NAME"
	}
	fmt.Fprintf(w, "gc-runtime-nomad: warning: session %q sets %s but not %s, so NO remote city "+
		"context was staged into the box. Both halves are required — neither alone describes a "+
		"city. The box will come up and look healthy, and every in-box gc call that needs the "+
		"city will fail as though the city were unreachable.\n",
		sessionName, present, missing) //nolint:errcheck
}

func (l *lifecycle) opProvisionWithConfig(ctx context.Context, sessionName string, cfg stageConfig) error {
	l.warnDeclaredSecrets(sessionName, cfg)
	unlock, err := l.lockSession(ctx, sessionName)
	if err != nil {
		return err
	}
	defer unlock()

	// Adopt-or-dispatch: a live child already bound to this
	// session is the box this call was asked to make, so re-drive staging
	// against it instead of dispatching a second one or
	// rejecting the caller. Re-staging is safe — stage's channels are
	// overwriting writes into fixed paths, so the adopted box converges on
	// the same state a fresh provision would have produced.
	//
	// This subsumes the narrower StagePending resume: a
	// provision that died mid-staging is only one of the ways a live,
	// unlaunched box gets left behind. The DF-03 canary hit the other one —
	// staging finished, the launch was never confirmed, and every later
	// provision tick was rejected while the alloc kept running.
	//
	// Adoption is bounded: the box has to be young enough
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

	if err := l.dispatch(ctx, sessionName, sessionAgentImage(cfg)); err != nil {
		return err
	}
	// A dispatch that Nomad ACCEPTS is not a box. Before this check, provision
	// returned 0 with empty stdout and empty stderr for an image no node has:
	// the child was dispatched, nothing was ever placed for it, and the caller
	// was told everything was fine. Measured T6, 2026-09-11 — provision with
	// gc-agent:T6-does-not-exist exited 0 and silent, and the child reported
	// "Placement Failure ... No allocations placed".
	//
	// A verb whose success means "I sent a request" is the wrong contract for
	// one the caller uses to mean "the box exists". Confirm an allocation
	// exists and is not already terminal, and say the image when it does not —
	// "the image is not on this node" is the sentence an operator needs, three
	// layers earlier than the exec-frame EOF they get today.
	//
	// FAIL CLOSED on an unreadable allocation list: a read error is reported as
	// a provision failure, not swallowed. Exiting nonzero because we could not
	// tell is still louder than exiting 0 while wrong.
	if err := l.confirmDispatchPlaced(ctx, sessionName); err != nil {
		return err
	}
	return l.stageTracked(ctx, sessionName, cfg)
}

// confirmDispatchPlaced turns "Nomad accepted the dispatch" into "a box exists".
//
// It waits briefly for an allocation to appear, because placement is not
// instantaneous, and reports the driver's OWN failure text when the allocation
// died instead of starting — a denied image pull says so in its events, and
// that sentence is worth more than every error derived from it downstream.
func (l *lifecycle) confirmDispatchPlaced(ctx context.Context, sessionName string) error {
	// Read the BINDING, not liveChild. liveChild filters for a child that is
	// already live, and the whole point here is the child that is NOT — a
	// dispatch that placed nothing comes back empty from liveChild and the
	// caller then gets "no child job is bound", which is true but useless: it
	// names our bookkeeping instead of the image that could not run. dispatch
	// commits ChildJobID to the binding before returning, so the binding has
	// what this check needs.
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return fmt.Errorf("provision %q: confirming the dispatch was placed: %w", sessionName, err)
	}
	if !ok || b.ChildJobID == "" {
		return fmt.Errorf("provision %q: dispatch accepted but no child job was recorded for this session", sessionName)
	}
	deadline := time.Now().Add(allocPlacementTimeout)
	var lastStatus string
	for {
		allocs, _, lerr := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
		if lerr != nil {
			return fmt.Errorf("provision %q: reading allocations of %s: %w", sessionName, b.ChildJobID, lerr)
		}
		for _, a := range allocs {
			switch strings.ToLower(strings.TrimSpace(a.ClientStatus)) {
			case "complete", "failed", "lost":
				lastStatus = a.ClientStatus
				continue
			default:
				return nil
			}
		}
		if time.Now().After(deadline) {
			if lastStatus != "" {
				return fmt.Errorf("provision %q: child %s placed an allocation that went %s without starting — if the agent image (%s) is not present on the node, that is why; read `nomad alloc status` for the driver's own error",
					sessionName, b.ChildJobID, lastStatus, l.agentImage)
			}
			// Name the constraints rather than referring to them. NOTHING
			// was placed, so there is no allocation to read a driver error
			// from, and "the node constraints cannot be satisfied" sends an
			// operator to go find out which. The pack built the jobspec; it
			// knows exactly which rules it asked for.
			// Ask the SCHEDULER before blaming the constraints. The
			// constraint story is right for a filtered placement and wrong
			// for an empty node pool, and the two are indistinguishable from
			// here without reading the evaluation.
			pf := l.client.jobPlacementFailure(ctx, b.ChildJobID)
			if sched := describePlacementFailure(pf); pf.emptyNodePool() && sched != "" {
				return fmt.Errorf("provision %q: child %s was dispatched but NO allocation was placed within %s.%s",
					sessionName, b.ChildJobID, allocPlacementTimeout, sched)
			}
			return fmt.Errorf("provision %q: child %s was dispatched but NO allocation was placed within %s — "+
				"no client satisfied the job's constraints. This job requires %s. Compare them against "+
				"`nomad node status -verbose <node>`; an image that is merely missing would have PLACED "+
				"and then failed, so a placement that never happens is a constraint or capacity fault, "+
				"not an image one.%s",
				sessionName, b.ChildJobID, allocPlacementTimeout,
				describeConstraints(l.parentJobSpec().Constraints), describePlacementFailure(pf))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(allocPlacementPoll):
		}
	}
}

// opStart is provision + launch: dispatch a fresh child,
// then launch the agent into it over exec, and only then mark the binding
// launched. Held under sessionName's single-flight lock end-to-end
// so two concurrent Start calls for the same session cannot both
// observe an absent/terminal binding and each dispatch a child.
//
// Before dispatching, it checks for a binding left by a prior attempt that
// crashed after the dispatch call committed but before the launched marker
// was recorded (the "after binding, before
// ConfirmStarted" crash point): that binding IS the positive attribution
// (no cluster lookup needed, since this pack already wrote it), so the
// retry resumes straight to launch instead of dispatching a second child or
// being rejected by dispatch's already-exists check. That resume is bounded by
// the same adopt-or-reap gate provision uses: a box too old to
// be this attempt's, or one whose agent is gone, is reaped instead of resumed,
// and a box whose agent is actually running is refused rather than
// double-launched.
func (l *lifecycle) opStart(ctx context.Context, sessionName string) error {
	return l.opStartWithConfig(ctx, sessionName, stageConfig{})
}

// opStartWithConfig is opStart plus staging: cfg's workspace
// files and secret env vars are materialized (l.stage) after the box is
// dispatched (or adopted through the same bounded adopt-or-reap gate
// provision uses) and before launch, so the agent bootstrap command that
// launch execs finds its workspace and secrets already in place. cfg.Env's argvSafe subset also rides launch's tmux
// argv (buildLaunchCommandForPane) — never the secrets dir.
func (l *lifecycle) opStartWithConfig(ctx context.Context, sessionName string, cfg stageConfig) error {
	l.warnDeclaredSecrets(sessionName, cfg)
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

	if err := l.dispatch(ctx, sessionName, sessionAgentImage(cfg)); err != nil {
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
// Two rules were added to the doors: an age limit, and an agent-absence
// check before reuse. Before them the only split was
// launched-reject / unlaunched-adopt, so a box left half-provisioned for hours
// was still adopted as if it were this tick's work — the 2026-09-06 canary
// re-staged a child dispatched 14h20m earlier.
type adoptPath uint8

const (
	// adoptReuse re-drives this pack's own half of the job against the box it
	// already made — the idempotent retry, now conditional on that
	// box being young AND its agent being confirmed absent.
	adoptReuse adoptPath = iota
	// adoptReject answers with the wire contract ("already exists"),
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
	// lets the caller dispatch fresh — the REAP FIRST ruling: an aged or
	// agent-less provisioned box is an ephemeral
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
		// split opIsRunning applies — reaping on a transport fault
		// would destroy a live session because the API blipped.
		return adoptReject, fmt.Sprintf("agent liveness unknown (probe unavailable, binding age %s): keeping the already-exists contract", age)
	// An unlaunched box whose agent state could not be read is reaped, not
	// reused and not refused. The probe-absence-unknown ruling:
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
		// The self-heal re-records a launch that already happened, so it
		// re-states what the binding already holds — the recorded provider
		// command included, so healing the marker cannot drop the command a
		// later relaunch needs.
		if err := l.confirmLaunched(ctx, sessionName, b.WorkDir, b.ProviderCommand); err != nil {
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
// confirmed-absent agent (the honesty split).
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
// dispatch — the warm-box mechanism for launch-only fingerprint drift:
// a tmux-respawn analog via exec kill+relaunch, no re-dispatch.
// It requires an existing provisioned box with a live alloc; a session with
// nothing running has nothing to relaunch into.
func (l *lifecycle) opRelaunch(ctx context.Context, sessionName string) error {
	if _, err := l.currentAlloc(ctx, sessionName); err != nil {
		return fmt.Errorf("relaunching session %q: %w", sessionName, err)
	}
	// No fresh env rides a relaunch (it re-execs into the SAME alloc
	// with no fresh dispatch) — the box's workspace/secrets were already
	// staged at start/provision time and are still there.
	return l.markLaunched(ctx, sessionName, stageConfig{})
}

// opExec runs command inside sessionName's current alloc over the Nomad
// alloc-exec WebSocket ("op exit = command exit"). It works regardless of
// the launched marker — this is exactly the "box exec-able" half of the
// provision contract: a
// provisioned-but-not-launched box still answers
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
	// stdout keeps the plain execAlloc semantics (peek's pane capture must not
	// start seeing box diagnostics spliced in); stderr is collected only to
	// record why a nonzero command failed, because gc discards the pack's own
	// output whenever the op exits nonzero.
	exitCode, stdout, boxErr, err := l.client.execAllocObserved(ctx, allocID, execTaskName, command, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("exec on session %q: %w", sessionName, err)
	}
	if exitCode != 0 {
		l.logDecision("exec on session %q exited %d: stdout=%s stderr=%s",
			sessionName, exitCode, execExcerpt(stdout), execExcerpt(boxErr))
	}
	return exitCode, stdout, nil
}

// execExcerpt renders box output for an error line: trimmed, length-bounded so
// a chatty failing command cannot blow up the log line or the caller's error
// string, and quoted so the boundary is visible.
func execExcerpt(b []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return strconv.Quote(s[:limit] + "...")
	}
	return strconv.Quote(s)
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
// tmux commands over exec rather than a bespoke protocol. text
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
// sessionAgentImage reads a per-session agent image out of the start payload's
// environment.
//
// This is the seam that lets a lane onboard by NAMING its runtime rather than
// by standing up a second deployment: a provider that wants a different agent
// toolchain sets GC_NOMAD_AGENT_IMAGE in the session env, and that session's
// box runs it while every other lane keeps the deployment default. Empty means
// "use the default", which the parent job already carries in its own Meta.
func sessionAgentImage(cfg stageConfig) string {
	return strings.TrimSpace(cfg.Env[envAgentImage])
}

// dispatch mints a child job for sessionName. image names a per-session agent
// image; empty uses the deployment default the parent already carries.
func (l *lifecycle) dispatch(ctx context.Context, sessionName, image string) error {
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

	// Positive-attribution adoption: a prior attempt may
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

	// Dispatch-intent record BEFORE the call: Nomad mints
	// the child ID in the dispatch response, so the binding cannot exist
	// before the call — this placeholder is what a crash between dispatch
	// and the binding write would otherwise lose.
	intent := binding{SessionName: sessionName, Namespace: l.client.namespace, Nonce: nonce, CreatedAt: time.Now().UTC()}
	if err := l.sidecar.save(intent); err != nil {
		return err
	}

	childID, err := l.client.dispatchChildAt(ctx, l.parentJobID, sessionName, nonce, intent.CreatedAt, image)
	if err != nil {
		return err
	}

	final := intent
	final.ChildJobID = childID
	return l.sidecar.save(final)
}

// ensureParentRegistered makes sure the parent job dispatch's children come
// from exists, in the current namespace, and matches the jobspec this build
// is configured for — the fix for the lab ACL model: the
// runtime token dispatch normally runs under may hold read-job and
// dispatch-job but deliberately NOT submit-job, since registering the
// parent is a one-time management-token setup step, not a per-Start action.
// Previously dispatch called registerJob unconditionally on every Start,
// which a narrowed dispatch-only token could never satisfy.
//
// It looks the parent up first via a GET (dispatch-scoped tokens can always
// read-job) and only calls register when the parent is missing or has
// drifted from this build's config on either signal getJob reports: NodePool
// (the one parentJobSpec field beyond namespace that varies at runtime)
// or the jobspecHashMetaKey fingerprint — added
// because a NodePool-only comparison missed a registered parent whose TASK
// had gone stale (e.g. a supervisor-script swap): the parent still
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

	// A pool change is not a task-spec change, and conflating them cost a live
	// outage. Re-registering an existing parent to pick up a new task spec is
	// routine — that is what a pack upgrade does. Moving the parent to a
	// different NODE POOL repoints where every future session is placed, and
	// this function used to do it silently whenever the calling process's
	// environment happened to differ from the registered job.
	//
	// The unintentional shape is specific and worth refusing outright: the
	// registered parent names a pool, and this process computed NO pool,
	// because GC_NOMAD_NODE_POOL is simply absent from its environment. That
	// is a forgotten variable, not a decision. Observed 2026-09-12 — a
	// hand-run `start` for diagnosis, without that variable, moved the parent
	// from "prod-services" into "default", which held zero nodes; every
	// dispatch then pended forever, and nothing said why until the supervisor
	// restored it on its next pass.
	//
	// Widening a pool is still allowed when it is EXPRESSED: set
	// GC_NOMAD_NODE_POOL to the pool you mean, including to move it back.
	if err := parentPoolMoveRefusal(ok, nodePool, l.nodePool, l.parentJobID); err != nil {
		return err
	}
	if w := parentPoolMoveWarning(ok, nodePool, l.nodePool, l.parentJobID); w != "" {
		// Never quietly, and durably, because the consequence outlives the
		// command that caused it.
		l.logDecision("%s", w)
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
				return fmt.Errorf("parent job %q is stale — re-register with a management token: the registered parent no longer matches this build's jobspec (node pool and/or task spec drifted) and the runtime token lacks the submit-job capability to fix it in place (the lab ACL model): %w", l.parentJobID, err)
			}
			return fmt.Errorf("registering parent job %q: token lacks the submit-job capability required to register jobs in namespace %q — register it once with a management token, or set GC_NOMAD_FORBID_REGISTER and do so out-of-band: %w", l.parentJobID, l.client.namespace, err)
		}
		return fmt.Errorf("registering parent job %q: %w", l.parentJobID, err)
	}
	return nil
}

func (l *lifecycle) parentJobSpec() nomadJob {
	// One shape: the Docker task the configured image describes. There is no
	// legacy exec variant left to choose between.
	return parentJobSpec(l.client.namespace, l.nodePool, l.parentJobID, l.logShipper, l.agentImage,
		l.placement, l.gcArtifact())
}

// gcArtifact resolves the per-box gc download from the gc that is ACTUALLY
// driving this pack, which is the whole point: nobody configures the URL, so
// nobody can forget to update it on a pin roll.
//
// Resolved fresh on each call rather than cached at construction, because the
// pack is exec'd per operation and the pin can roll between two of them. A
// value captured once would be exactly the stale copy this design exists to
// abolish.
func (l *lifecycle) gcArtifact() gcArtifactConfig {
	tmpl := strings.TrimSpace(os.Getenv(envGCArtifactTemplate))
	if tmpl == "" {
		return gcArtifactConfig{} // off: the image's own gc stands
	}
	return resolveGCArtifact(tmpl, hostGCCommit(context.Background()))
}

// recoverBinding reconstructs the part of a sidecar binding that Nomad's
// native children-of-parent jobs list can answer. The list is intentionally
// filtered by the child namespace as well as gc_session: the request's
// namespace query is not a substitute for checking each returned row.
//
// Exactly one attributable, non-terminal child is required. Terminal children
// are neither candidates nor, on their own, evidence of a dispute: they are
// residue from an attempt that already ended. Counting them made one live box
// plus one dead child read as a disputed binding on every retry, which is how
// the D3 canary lost every runtime-backed seat on 2026-09-10
// ("matching children are ambiguous ... found 2 children"). Zero non-terminal
// matches, multiple non-terminal matches, or a missing nonce are all loud
// errors; the caller must never guess a child or dispatch around a disputed
// binding. With nothing live left to recover, a terminal match keeps its own
// loud error: there is nothing to adopt, and refusing is what prevents a
// dispatch around a stale or disputed binding.
// CreatedAt is recovered from the provider-stamped gc_created_at Meta value.
// A child from before that metadata existed, or one with malformed metadata,
// remains undated and the adoption gate refuses it rather than fabricating a
// young timestamp and bypassing the bounded age rule.
func (l *lifecycle) recoverBinding(ctx context.Context, sessionName string, prior *binding) (binding, error) {
	children, err := l.client.listChildJobs(ctx, l.parentJobID)
	if err != nil {
		return binding{}, fmt.Errorf("recovering sidecar binding for session %q: %w", sessionName, err)
	}

	matches := make([]childJob, 0, 1)
	terminalMatches := make([]childJob, 0, 1)
	for _, child := range children {
		if child.Namespace != l.client.namespace || child.Meta["gc_session"] != sessionName {
			continue
		}
		if child.Terminal {
			terminalMatches = append(terminalMatches, child)
			continue
		}
		matches = append(matches, child)
	}
	switch len(matches) {
	case 0:
		if len(terminalMatches) > 0 {
			return binding{}, fmt.Errorf("%w for session %q: child %q", errBindingTerminal, sessionName, terminalMatches[0].ID)
		}
		return binding{}, fmt.Errorf("%w for session %q in parent %q", errBindingNoMatch, sessionName, l.parentJobID)
	case 1:
		// Continue with the guard checks below.
	default:
		return binding{}, fmt.Errorf("%w for session %q in parent %q: found %d non-terminal children", errBindingAmbiguous, sessionName, l.parentJobID, len(matches))
	}

	child := matches[0]
	nonce := strings.TrimSpace(child.Meta["gc_nonce"])
	if nonce == "" {
		return binding{}, fmt.Errorf("sidecar binding recovery: child %q for session %q has no gc_nonce", child.ID, sessionName)
	}

	recovered := binding{
		SessionName: sessionName,
		ChildJobID:  child.ID,
		Namespace:   child.Namespace,
		Nonce:       nonce,
		CreatedAt:   parseCreatedAtMeta(child.Meta),
	}
	if prior != nil {
		// A dispatch-intent record may already carry the local residue that
		// Nomad cannot answer. Preserve it while replacing only the fields
		// recovered from Meta. currentAlloc can retain an intent's timestamp
		// and staging state; a nil prior means the sidecar record was
		// completely lost.
		base := *prior
		if !base.CreatedAt.IsZero() {
			recovered.CreatedAt = base.CreatedAt
		}
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

// parseCreatedAtMeta parses the provider-owned dispatch timestamp that
// survives a sidecar loss in Nomad's child-job metadata. Invalid or absent
// values deliberately return the zero time: classifyAdoption then refuses the
// child, preserving the fail-closed behavior for genuinely unknown age.
func parseCreatedAtMeta(meta map[string]string) time.Time {
	raw := strings.TrimSpace(meta[createdAtMetaKey])
	if raw == "" {
		return time.Time{}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return createdAt.UTC()
}

// findOrphanByNonce looks up the parent job's dispatched children ("list
// the city parent's children with meta=true") for one whose
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

// launch execs the composed tmux-client launch into sessionName's current
// alloc — the detached tmux-client call that starts the agent (the launch
// invariant).
//
// providerCommand is gc's rendered launch command. The Docker task is the only
// session shape and it owns the agent toolchain, so the pane
// runs what gc rendered instead of a deployment bootstrap the image neither
// needs nor can reach — the session shape declares no artifact volume
// (jobspec.go sessionTask), so a host-volume bootstrap would name a path that
// is not mounted. GC_NOMAD_AGENT_IMAGE is required and a run without it fails
// closed in newLifecycle; gc's rendered command is the only pane body: the
// deployment bootstrap seam is deleted, so there is no alternative body left
// for the pack to compose.
//
// An image-mode launch with no rendered command fails closed here rather than
// execing a pane that starts nothing yet still gets a launched record — the
// agent-less-box wedge this weld exists to close.
func (l *lifecycle) launch(ctx context.Context, sessionName string, env map[string]string, workDir, providerCommand string) error {
	if strings.TrimSpace(providerCommand) == "" {
		return fmt.Errorf("launching session %q: %s is set but gc sent no rendered launch command, and the prebaked image leaves the pack no bootstrap to substitute",
			sessionName, envAgentImage)
	}
	// The same class reaches the pane by a second door: tmux `new-session -e`
	// seeds the SERVER environment, which every pane inherits, so an
	// argv-safe PATH would hide the image toolchain even from a clean env.sh.
	// The canonical transport path hands no env to the builder at all, so
	// this is the only place it can cross.
	return l.launchWithCommand(ctx, sessionName, buildLaunchCommandForPane(boxFilesystemEnvInput(env), workDir, providerCommand))
}

// launchWithCommand executes a pack-owned detached tmux launch command into
// sessionName's current alloc. Keeping this separate lets opExec preserve GC's
// new-session versus respawn transport verb without handing GC's raw command
// a way around staged bootstrap.
func (l *lifecycle) launchWithCommand(ctx context.Context, sessionName string, command []string) error {
	allocID, err := l.awaitExecAllocReady(ctx, sessionName)
	if err != nil {
		return err
	}
	return retryAllocExecReady(ctx, func() error {
		// The launch is the one exec whose failure has to say WHY. gc's exec
		// transport turns any nonzero op exit into "tmux exited N" and drops
		// the pack's own stdout/stderr on the floor
		// (internal/runtime/exec/exec.go Provider.Exec/launchAgent), so the
		// box's diagnostics are the only reason that survives this boundary —
		// and they arrive on the exec stream's stderr frames.
		exitCode, out, boxErr, err := l.client.execAllocObserved(ctx, allocID, execTaskName, command, nil)
		if err != nil {
			return fmt.Errorf("launching session %q: %w", sessionName, err)
		}
		if exitCode != 0 {
			return fmt.Errorf("launching session %q: launch command exited %d: stdout=%s stderr=%s",
				sessionName, exitCode, execExcerpt(out), execExcerpt(boxErr))
		}
		return nil
	})
}

// capturePanePID reads back the pid tmux assigned the pane launch just
// created inside sessionName's current alloc — the in-box liveness probe
// (opIsRunning) later kill -0's this exact pid rather than trusting the
// tmux session's mere existence, which alone cannot tell "agent died,
// nothing reaped the empty session" apart from "agent alive" (the in-box
// agent-kill case). A lookup failure here is non-fatal to launch itself: it
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

// paneLaunchSettle bounds how long verifyPaneRunsAgent waits for the pane to
// stop being a bare shell. An agent process appears within a second or two of
// the launch exec; this window is generous so a slow image never trips it.
var (
	// Short on purpose. This window is only ever spent to decide whether to
	// WARN (warnIfPaneRunsNoAgent), so paying for it is pure latency on every
	// single launch — and in a harness whose panes legitimately hold a shell it
	// is paid in full every time. Measured 2026-09-12: at 20s it pushed the pack
	// suite past a 12-minute timeout that it had been finishing inside before,
	// which is a real cost for an advisory signal. An agent that is going to
	// appear appears within a second or two.
	paneLaunchSettle = 3 * time.Second
	paneLaunchPoll   = 500 * time.Millisecond
)

// bareShellCommands are the pane commands that mean "nothing was started here".
// tmux reports the pane's foreground command, so a pane whose launch command
// died back to its parent shell reads as one of these.
var bareShellCommands = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ash": true, "ksh": true,
}

// bareShellRefusalHint is the operator-facing half of the bare-shell refusal,
// kept separate so a guard can assert it stays actionable.
func bareShellRefusalHint() string {
	return "the launch command started nothing in this box. A provider shaped for the orchestrator " +
		"(argv flags, hook support, argv priming) does not survive here; the box needs gc's rendered launch " +
		"command. Left unchecked this box reports healthy forever and claims no work"
}

// paneRunsAgent classifies what tmux reports for the pane. This is the judgement
// the pack got wrong: a bare shell is not an agent, but it IS a live process, so
// every pid-based check downstream called it healthy.
//
// An empty reading is NOT an answer and must never be treated as a failure —
// the caller keeps waiting and, if it never resolves, stays silent rather than
// inventing a cause.
func paneRunsAgent(paneCommand string) bool {
	c := strings.TrimSpace(paneCommand)
	if c == "" {
		return false
	}
	return !bareShellCommands[c]
}

// verifyPaneRunsAgent refuses a launch whose pane is only running a shell.
//
// See confirmLaunched for why this lives at launch and not in the liveness
// probe. The error names the session, what the pane is actually running, and
// the command that was supposed to start an agent, because the whole failure
// this replaces was silent: the operator's only symptom was a lane that claimed
// nothing.
// warnIfPaneRunsNoAgent reports — loudly, on stderr, where the supervisor log
// will carry it — that a launch left the pane holding a bare shell.
//
// IT DOES NOT FAIL THE LAUNCH, and that is a correction to my first version of
// this check (67d6360), which did. Measured 2026-09-12: a legitimate relaunch
// whose pane body wraps the command in a shell leaves the SHELL as the pane's
// foreground process while the agent runs beneath it, so a refusal there breaks
// a healthy lane. TestFaultReplacementAlloc caught exactly that, and I could not
// rule out the same shape behind a production wrapper.
//
// A check that can refuse a healthy launch is worse than the dark lane it
// replaces: it converts a silent failure into a broken one. The diagnosis was
// always the valuable half — an empty box went six minutes with a
// P0 routed to it and NOTHING in any log — so the warning stays and the refusal
// goes.
func (l *lifecycle) warnIfPaneRunsNoAgent(ctx context.Context, sessionName string) {
	if err := l.verifyPaneRunsAgent(ctx, sessionName); err != nil {
		fmt.Fprintf(os.Stderr, "gc-runtime-nomad: WARNING: %v\n", err) //nolint:errcheck
	}
}

func (l *lifecycle) verifyPaneRunsAgent(ctx context.Context, sessionName string) error {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		// Cannot read the alloc: this is a transport fault, not evidence the
		// agent is missing. Do not fail a launch on an unreadable box.
		return nil
	}
	deadline := time.Now().Add(paneLaunchSettle)
	last := ""
	for {
		// An agent PROCESS is the reliable signal; the pane's foreground command
		// is not. A pane body that wraps the command in a shell keeps the SHELL
		// in front while the agent runs beneath it, which made this warn on every
		// healthy launch in the harness — and a warning that cries wolf every
		// time teaches an operator to ignore the one that matters.
		runs, unmeasurable := l.boxRunsAnAgent(ctx, allocID)
		if unmeasurable {
			return fmt.Errorf("launching session %q: cannot tell whether an agent is running — "+
				"the process probe exited nonzero inside the box, which means the image has no "+
				"usable `ps`. This runtime requires it (COMPATIBILITY.md, \"The agent image\"): "+
				"without it an empty box is indistinguishable from a working one, for every "+
				"session this deployment ever runs", sessionName)
		}
		if runs {
			return nil
		}
		cmd, err := l.paneCurrentCommand(ctx, allocID)
		if err == nil {
			last = cmd
			if paneRunsAgent(cmd) {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(paneLaunchPoll):
		}
	}
	if last == "" {
		// No reading at all: stay silent rather than invent a failure.
		return nil
	}
	return fmt.Errorf("launching session %q: the pane is running %q and no agent after %s — %s",
		sessionName, last, paneLaunchSettle, bareShellRefusalHint())
}

// warnIfBoxGCCannotClaim probes the `gc` INSIDE the box, once, at launch.
//
// The startup gate checks the gc on the supervisor's PATH. That is the gc
// driving the pack, and it is not the gc that claims: a seat claims with the
// binary baked into the agent image. A deployment can therefore pass the gate
// and still produce healthy boxes whose seats never claim — the exact dark
// lane the gate exists to prevent, moved one layer in. Found by an operator
// following this pack's own skill against a staged fault.
//
// A warning rather than a refusal: the box already exists by the time this can
// run, and tearing down a healthy allocation over a probe is a worse failure
// than reporting it. Unknown says nothing at all.
func (l *lifecycle) warnIfBoxGCCannotClaim(ctx context.Context, sessionName string) {
	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil {
		return
	}
	_, out, err := l.client.execAlloc(ctx, allocID, execTaskName, inBoxProbeCommand())
	if err != nil {
		// A probe that could not RUN is not a probe that passed. Returning
		// silently here made "the in-box gc cannot claim" and "I never asked"
		// the same observation, which is the dark lane one layer in.
		l.logDecision("in-box gc capability probe did not run for session %q: %v "+
			"(so whether the gc INSIDE the box can claim is unverified; a compliant host "+
			"gc with a base-lineage gc in the image gives green boxes and dark seats)",
			sessionName, err)
		return
	}
	if w := inBoxWorkerSurfaceWarning(string(out)); w != "" {
		l.logDecision("%s (session %q)", w, sessionName)
	}

	// Capability is not identity. A stale-but-capable gc passes the check
	// above and still answers an older contract, which is the failure that
	// actually happens here — see the stale-image comment in gc_capability.go.
	l.warnIfBoxGCIsStale(ctx, sessionName, allocID)
}

// warnIfBoxGCIsStale compares the gc inside the box against the gc driving
// this pack, by COMMIT.
//
// Every non-answer is silent on purpose. "Could not tell" must never render as
// "they differ": a false staleness report on the deepest dependency in the
// stack would send an operator to rebuild an image that was already correct,
// and this warning is only worth having if it is never cried wolf.
func (l *lifecycle) warnIfBoxGCIsStale(ctx context.Context, sessionName, allocID string) {
	host := hostGCCommit(ctx)
	if host == gcCommitUnknownText {
		return
	}
	_, out, err := l.client.execAlloc(ctx, allocID, execTaskName, inBoxVersionCommand())
	if err != nil {
		return
	}
	if w := staleBoxGCWarning(host, gcCommitFromVersionJSON(string(out))); w != "" {
		l.logDecision("%s (session %q)", w, sessionName)
	}
}

// boxRunsAnAgent reports whether the box holds any process that is not part of
// the empty-box furniture.
//
// This is the check actually used by hand to diagnose one such box: a box with
// nothing in it showed exactly `sleep` (the task's own idle process), a tmux
// server and a shell, and NOTHING else. A box with a live agent showed codex
// beside them. That distinction survives a wrapped pane, which the pane's
// foreground command does not.
//
// Unreadable is NOT an answer: a failed probe reports runs=true (do not warn),
// so an unreachable box never produces a false alarm.
//
// The two ways it can fail are not the same failure and are reported apart.
// A transport fault (err != nil) says nothing about the box. But an exec that
// REACHED the box and came back nonzero means the probe command itself failed
// in there — overwhelmingly, an image with no `ps`. That is an image-contract
// violation (COMPATIBILITY.md lists `ps` as required), and it matters more
// than it looks: without `ps` this probe can never answer for the life of the
// deployment, so every box silently forfeits its empty-box diagnosis. Naming
// it is the layer that knows doing its job; an outside review found this path
// asserting health on a probe that could not run.
func (l *lifecycle) boxRunsAnAgent(ctx context.Context, allocID string) (runs, unmeasurable bool) {
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName,
		[]string{"/bin/sh", "-c", "ps -eo comm 2>/dev/null || ps -o comm 2>/dev/null"})
	return classifyProcessProbe(err, exitCode, out)
}

// classifyProcessProbe is boxRunsAnAgent's decision, separated from its
// transport so it can be tested at all.
//
// It cannot be reached through this pack's offline harness: fakenomad runs a
// box's commands as local subprocesses on the developer's machine, where `ps`
// always exists, so the missing-`ps` case is unreachable there by
// construction. That is worth stating rather than working around — it is a
// concrete instance of the harness being blind to a real-box property, and
// the reason this logic is a pure function.
func classifyProcessProbe(err error, exitCode int, out []byte) (runs, unmeasurable bool) {
	if err != nil {
		return true, false
	}
	if exitCode != 0 {
		return true, true
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == "COMMAND" {
			continue
		}
		name = filepath.Base(name)
		switch {
		case name == "sleep", name == "ps", name == "COMMAND":
			continue
		case strings.HasPrefix(name, "tmux"):
			continue
		case bareShellCommands[name]:
			continue
		}
		return true, false
	}
	return false, false
}

// paneCurrentCommand reads the foreground command tmux reports for the pane.
func (l *lifecycle) paneCurrentCommand(ctx context.Context, allocID string) (string, error) {
	exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName,
		[]string{"tmux", "list-panes", "-t", tmuxSessionName, "-F", "#{pane_current_command}"})
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("tmux list-panes exited %d: %s", exitCode, execExcerpt(out))
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]), nil
}

// markLaunched launches sessionName's agent and, only once that succeeds,
// records the launched marker (and the pane pid the probe will later
// kill -0) in the sidecar binding — shared by opStart (provision then
// launch) and opRelaunch (launch only, no re-dispatch). cfg's argvSafe env subset rides the launch command
// (buildLaunchCommandForPane) and cfg.WorkDir chooses the pane's cwd
// (paneCwdPrelude). opRelaunch passes the zero config, since no fresh env
// or work dir accompanies a relaunch, and falls back to the WorkDir the
// binding recorded. Once the launch is recorded, startupPrime owes the
// pane its first prompt exactly once per session (startup.go) - which is
// the whole point of this call site: it is the only place in the pack
// that runs after every successful launch.
func (l *lifecycle) markLaunched(ctx context.Context, sessionName string, cfg stageConfig) error {
	workDir := cfg.WorkDir
	providerCommand := strings.TrimSpace(cfg.Command)
	if workDir == "" || providerCommand == "" {
		// A relaunch - or a start whose caller sent no work_dir or command -
		// still gets the directory and the rendered command the box was first
		// launched with, instead of "/" and an empty pane body.
		if b, ok, err := l.sidecar.load(sessionName); err == nil && ok {
			if workDir == "" {
				workDir = b.WorkDir
			}
			if providerCommand == "" {
				providerCommand = strings.TrimSpace(b.ProviderCommand)
			}
		}
	}
	if err := l.launch(ctx, sessionName, cfg.Env, workDir, providerCommand); err != nil {
		return err
	}
	if err := l.confirmLaunched(ctx, sessionName, workDir, providerCommand); err != nil {
		return err
	}
	return l.startupPrimeDetached(ctx, sessionName, cfg)
}

// markTransportLaunched preserves GC's canonical tmux transport verb and
// rendered pane command while adding the staged, in-box environment prelude.
// The caller already holds sessionName's lock.
//
// Once the launch is confirmed, it hands the pane off to the detached prime
// window exactly as markLaunched does: this is gc's
// proc.provision path, which stages the startup prompt separately
// (opProvisionWithConfig -> savePromptPending) from the launch it later
// drives through this transport handshake, so the prompt fields ride the
// binding's PromptPending — recorded at stage time — rather than this call's
// own argv, which carries only the transport verb. startupPrimeDetached
// returns as soon as the window is spawned, so this op keeps returning the
// instant launch is confirmed, same as before any priming existed
// (a blocking wait here is what put attempt 6's start past gc's
// async-start budget and looped a fresh dispatch every ~30s).
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
	if err := l.confirmLaunched(ctx, sessionName, workDir, transport[5]); err != nil {
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
func (l *lifecycle) confirmLaunched(ctx context.Context, sessionName string, workDir, providerCommand string) error {
	// THE PANE MUST BE RUNNING AN AGENT, not merely holding a live pid.
	//
	// Measured 2026-09-12: a lane whose provider was not pre-shaped
	// for a box got a running alloc, a running tmux server, and a pane holding a
	// bare `sh` — no agent at all. Nothing failed. `launch` had exited 0 (tmux
	// started fine; the command inside the pane is what did not survive),
	// capturePanePID returned the shell's pid, and probeAgentAlive's
	// `tmux has-session && kill -0 <pid>` then answered ALIVE forever, because a
	// shell is a perfectly live process. The lane served nothing for six minutes
	// with a P0 routed to it and not one line in any log.
	//
	// That is this programme's recurring defect exactly — a dark lane that looks
	// alive — so the check belongs here, at launch, in the layer that knows what
	// it just started.
	//
	// It is deliberately NOT added to probeAgentAlive. A healthy agent shells out
	// constantly for tool calls, so pane_current_command reads "bash"/"sh" during
	// normal work; asserting on it continuously would reap working seats. At
	// launch nothing has shelled out yet, which is the one moment the reading is
	// unambiguous.
	l.warnIfPaneRunsNoAgent(ctx, sessionName)
	l.warnIfBoxGCCannotClaim(ctx, sessionName)
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
	if providerCommand != "" && b.ProviderCommand != providerCommand {
		b.ProviderCommand = providerCommand
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
// sessionName's binding, so a later transport launch through
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
	// No warning here: this is a pure predicate that decides whether stage()
	// has work to do, and it can be consulted more than once. The declared-but-
	// absent report belongs on the staging path itself, which runs once.
	secretEnv, _, _ := mergeExtraSecretEnv(cfg.Env, extraSecretKeys)
	secretEnv = stripLocalCityStoreEnv(secretEnv)
	secretEnv = mergeLaunchOptionEnv(secretEnv, cfg.Command)
	return len(secretEnv) > 0
}

// stage materializes cfg's workspace files and secret env vars into
// sessionName's current alloc (the data contract) over two
// tar-over-exec-stdin calls: cfg.CopyFiles read off the orchestrator and
// archived by copyFilesToStageFiles, extracted under cfg.WorkDir, and cfg.Env's values extracted under
// $NOMAD_SECRETS_DIR both as individual files (legacy/value-specific access)
// and as a sourceable env.sh contract for the launched process. Neither
// channel ever touches the job spec, argv, or the sidecar — the only place
// secret BYTES appear is the alloc-exec WebSocket stream itself (accepted
// residual). A zero-value cfg is a no-op, so start/provision behave
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
			// T6: "reading exec frame: EOF" is what a task that never started
			// looks like from in here. Nomad knows why; ask it before reporting.
			if detail := l.provisionFailureDetail(ctx, sessionName); detail != "" {
				return fmt.Errorf("staging workspace for %q: %s: %w", sessionName, detail, err)
			}
			return fmt.Errorf("staging workspace for %q: %w", sessionName, err)
		}
	}

	// The declared-but-absent report does NOT live here. stage() runs only when
	// stagesAnything() says there is something to materialize, and a session
	// whose ONLY declared secret keys are all absent stages nothing — measured:
	// stagesAnything is false for an empty cfg with one absent declaration. So
	// attaching the warning here silenced it in precisely the case that needs
	// it. It is reported once per session op instead, in warnDeclaredSecrets.
	secretEnv, extraKeys, _ := mergeExtraSecretEnv(cfg.Env, extraSecretKeys)
	secretEnv = stripLocalCityStoreEnv(secretEnv)
	secretEnv = mergeLaunchOptionEnv(secretEnv, cfg.Command)

	// Runtime secrets stay FILE-delivered into Nomad's own secrets mount. The
	// pack declares no `template {}` / Vault / Consul / identity block anywhere
	// (jobspec.go sessionTask), so secret bytes reach a box only over
	// the alloc-exec stream below — never through the job spec, argv, or a
	// Nomad-interpolated template. The docker-driver shape resolves that
	// mount's path from the box before writing anything that names it:
	// contexts.toml is literal TOML with no $NOMAD_SECRETS_DIR expansion, and a
	// guessed /secrets would point gc's mTLS files at a directory that may not
	// exist in a container. The empty-image branch below is only a
	// discriminator now: GC_NOMAD_AGENT_IMAGE is required and a run without it
	// already failed closed in newLifecycle, and the launch-script seam that
	// shape used to reach has been deleted.
	secretsDir := ""
	if l.agentImage != "" && len(secretEnv) > 0 {
		resolved, err := l.boxSecretsDir(ctx, sessionName)
		if err != nil {
			return fmt.Errorf("staging secrets for %q: %w", sessionName, err)
		}
		secretsDir = resolved
	}

	var secretFiles []stageFile
	for k, v := range secretEnv {
		if !extraKeys[k] && envArgvSafe(k) {
			continue
		}
		secretFiles = append(secretFiles, stageFile{Path: k, Content: []byte(v), Mode: 0o600})
	}
	if len(secretEnv) > 0 {
		// The image shape has no deployment bootstrap to hand its agent home to
		// (the launch-script seam is deleted outright), so the pack
		// stages the Codex home itself. A box staged without an image keeps its
		// bytes unchanged.
		codexHome := codexHomeOwned(secretsDir, secretEnv)
		envInput := remoteContextEnvScriptInput(secretEnv)
		// An image-mode box owns its own filesystem. PATH, HOME, TMPDIR, the XDG
		// dirs and anything else naming this host are sourced by the pane before
		// its command, so they hid the image's toolchain and the pane died with
		// its tmux server. secretsDir is only resolved in the image
		// shape, so it is that shape's discriminator here too; the legacy shape
		// still reaches buildEnvScript with its bytes unchanged.
		if secretsDir != "" {
			envInput = boxFilesystemEnvInput(envInput)
		}
		if codexHome {
			envInput = codexHomeEnvScriptInput(envInput)
		}
		envScript := buildEnvScript(envInput)
		// The remote-context bridge rides the same env.sh:
		// GC_HOME/GC_CITY_CONTEXT are a no-op append when GC_CITY_NAME or
		// GC_CITY_URL is absent from the wire, so this costs nothing for a
		// deployment that has not opted in.
		envScript = append(envScript, buildRemoteContextEnvLines(secretEnv)...)
		if codexHome {
			// Appended last on purpose: an export written after every staged
			// provider value is the one that wins when the pane sources env.sh,
			// and this is the override of the provider env the image-mode Codex
			// home asks for. The recipe lives in codex_home.go, not here.
			envScript = append(envScript, buildCodexHomeEnvLines()...)
		}
		secretFiles = append(secretFiles, stageFile{
			Path:    stagedEnvScriptName,
			Content: envScript,
			Mode:    0o600,
		})
		// contexts.toml itself - gc's own $GC_HOME/contexts.toml schema,
		// hand-written since this pack cannot import gc's clientcontext
		// package. Also a no-op append on the same condition.
		if ctxFile := buildRemoteContextFile(secretEnv, secretsDir); ctxFile != nil {
			secretFiles = append(secretFiles, *ctxFile)
		}
		if codexHome {
			homeFiles, err := buildCodexHomeFiles(secretEnv)
			if err != nil {
				return fmt.Errorf("staging secrets for %q: %w", sessionName, err)
			}
			secretFiles = append(secretFiles, homeFiles...)
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
			// Same reasoning as the workspace leg above (T6): a task that never
			// started reads as a truncated exec frame from in here.
			if detail := l.provisionFailureDetail(ctx, sessionName); detail != "" {
				return fmt.Errorf("staging secrets for %q: %s: %w", sessionName, detail, err)
			}
			return fmt.Errorf("staging secrets for %q: %w", sessionName, err)
		}
	}
	return nil
}

// boxSecretsDir asks sessionName's box where Nomad actually mounted its secrets
// tmpfs instead of assuming the exec driver's /secrets also holds inside a
// container. The docker-driver shape calls it only when there are secrets to
// deliver, and it fails closed on an empty answer: a box with no secrets mount
// has nowhere for the staged runtime secret files to go, and staging them to a
// path the box cannot see would hand the agent a credential-less session with
// nothing in the record to say so.
//
// The probe prints only a mount POINT — the same path the pack already writes
// into its own comments and job specs — never a secret value.
func (l *lifecycle) boxSecretsDir(ctx context.Context, sessionName string) (string, error) {
	allocID, err := l.awaitExecAllocReady(ctx, sessionName)
	if err != nil {
		return "", err
	}
	var resolved string
	if err := retryAllocExecReady(ctx, func() error {
		exitCode, out, err := l.client.execAlloc(ctx, allocID, execTaskName, []string{
			"/bin/sh", "-c", `printf %s "$NOMAD_SECRETS_DIR"`, "gc-runtime-nomad-secrets-dir",
		})
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("secrets-dir probe exited %d: %s", exitCode, execExcerpt(out))
		}
		resolved = strings.TrimSpace(string(out))
		return nil
	}); err != nil {
		return "", err
	}
	return validatedSecretsDir(sessionName, resolved)
}

// validatedSecretsDir judges what a box reported for its own secrets mount: a
// single non-empty path, or nothing this pack may write secret bytes to. It is
// split out of the probe so the fail-closed rule is checkable without a box
// that lies about its mount.
func validatedSecretsDir(sessionName, reported string) (string, error) {
	reported = strings.TrimSpace(reported)
	if reported == "" || strings.ContainsAny(reported, "\n\r") {
		return "", fmt.Errorf("session %q reports no usable Nomad secrets mount (NOMAD_SECRETS_DIR=%q): refusing to stage runtime secrets to a path the box cannot see", sessionName, reported)
	}
	return reported, nil
}

// execStage runs command inside sessionName's current alloc with stdin
// attached (execAllocStdin) and turns a nonzero exit into an error — the
// shared tail stage's two tar-extraction calls need. Deliberately does not
// echo stdin (the tar bytes) into any error message: a failing tar's own
// stderr/stdout (out) is safe to surface, but stdin here may carry secret
// content the error path must never leak.
func (l *lifecycle) execStage(ctx context.Context, sessionName string, command []string, stdin []byte) error {
	allocID, err := l.awaitExecAllocReady(ctx, sessionName)
	if err != nil {
		return err
	}
	return retryAllocExecReady(ctx, func() error {
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
//
// It is the handshake retry, not the readiness gate: both of its callers
// resolve the allocation through awaitExecAllocReady first, because
// a task that has not started is a state to wait for, not a transport failure
// to punch at for five seconds.
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
// to reading), or ECONNRESET; writing gives EPIPE — the shape seen on the
// lab, where the staging tar's own stdin frames raced a
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
// sessionName's current child job, sidecar-binding-primary,
// recovering a missing binding from Nomad Meta under the session lock.
func (l *lifecycle) currentAlloc(ctx context.Context, sessionName string) (string, error) {
	rec, err := l.currentAllocRecord(ctx, sessionName)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// currentAllocRecord is currentAlloc with the allocation's own lifecycle
// record attached: the exec-readiness wait needs its TaskStates, and issuing
// a second read for them would let the binding move between the two. Same
// binding rules, same error classifications as currentAlloc.
func (l *lifecycle) currentAllocRecord(ctx context.Context, sessionName string) (allocRecord, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return allocRecord{}, err
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
			return allocRecord{}, err
		}
		defer unlock()
		b, ok, err = l.sidecar.load(sessionName)
		if err != nil {
			return allocRecord{}, err
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
					return allocRecord{}, fmt.Errorf("session %q has no provisioned box: %w", sessionName, errSessionNotFound)
				}
				return allocRecord{}, err
			}
			b = &recovered
		}
	}

	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return allocRecord{}, fmt.Errorf("resolving alloc for session %q: %w", sessionName, err)
	}
	if len(allocs) == 0 {
		return allocRecord{}, fmt.Errorf("session %q has no allocation yet: %w; %w", sessionName, errSessionNotFound, errAllocNotReady)
	}
	for _, a := range allocs {
		if !isTerminalStatus(a.ClientStatus) {
			return a, nil
		}
	}
	return allocRecord{}, fmt.Errorf("session %q has no non-terminal alloc: %w", sessionName, errSessionNotFound)
}

// awaitExecAllocReady resolves sessionName's allocation and returns its ID
// only once execTaskName's task inside it is running -- the readiness contract
// the exec seam was missing. An alloc in "Task Setup" is
// non-terminal, so currentAlloc returned it gladly and the first staging exec
// died on "reading exec frame: EOF"; the pack then looked like a provider
// failure and gc reaped and re-dispatched the session.
//
// currentAlloc's classifications are preserved on purpose: a binding that
// names no box, or an alloc that has already gone terminal, fails now exactly
// as it did before. Only "the alloc is not visible yet" and "it is visible but
// its task has not started" are states worth waiting in; waiting on a real
// fault just delays it by the whole budget.
//
// A read that carries NO task states at all is answered as ready. That is the
// API declining to answer (a stub read; the pack's own fake for a job whose
// tasks it does not model), not an answer that the task is down, and treating
// it as one would turn every existing caller into a 180 s stall.
func (l *lifecycle) awaitExecAllocReady(ctx context.Context, sessionName string) (string, error) {
	return l.awaitExecAllocReadyWithin(ctx, sessionName, allocTaskRunningTimeout, allocTaskRunningPoll)
}

// awaitExecAllocReadyWithin is the poll itself with its budget as parameters,
// so the wait can be tested at second scale instead of 180 s.
func (l *lifecycle) awaitExecAllocReadyWithin(ctx context.Context, sessionName string, timeout, interval time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var observed string
	for {
		rec, err := l.currentAllocRecord(ctx, sessionName)
		switch {
		case err == nil:
			state, hasState := rec.TaskStates[execTaskName]
			if !hasState {
				return rec.ID, nil
			}
			if state.State == allocStateRunning {
				return rec.ID, nil
			}
			observed = describeTaskState(rec.ID, state)
		case errors.Is(err, errAllocNotReady):
			// The alloc is not visible yet -- the window the short exec retry
			// used to stand in for on its own. Keep polling inside the budget.
			observed = err.Error()
		default:
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("session %q: task %q was not running within %s (last seen: %s)",
				sessionName, execTaskName, timeout, observed)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

// describeTaskState renders one observation of the task for a timeout reason,
// carrying the allocation's own event trail when the API supplied one.
func describeTaskState(allocID string, state allocTaskState) string {
	if state.LastEvent != "" {
		return fmt.Sprintf("alloc %s task state %q, last event %s", allocID, state.State, state.LastEvent)
	}
	return fmt.Sprintf("alloc %s task state %q", allocID, state.State)
}

// egressMaxAttempts bounds the stop-path transcript/evidence egress retries
// before opStop gives up and falls back to an evidence_lost marker:
// evidence-best-effort beats a wedged fleet.
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
// configured), deregisters its child job without purge, confirms
// terminal via a blocking read, and tombstones the sidecar binding. Stop on
// a session with no binding (never started, or already stopped) is a
// success no-op — Stop stays idempotent.
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
	// killed below. The task's own exit is independent of
	// this — the Docker task-main exits on Nomad's own stop — but tmux's server
	// process is
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
	// A failing egress gets egressMaxAttempts bounded retries; if
	// it still hasn't succeeded, stop PROCEEDS rather than wedging behind
	// evidence collection — it just marks the tombstone evidence_lost
	// instead. Either outcome is receipted in the sidecar (still resident)
	// before deregister, so a stop that then crashes before deregister
	// retries idempotently: it neither re-attempts a marker that already
	// gave up nor re-copies files it already egressed.
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
		// Confirm terminal via blocking read (the stop-row ordering
		// invariant). fakenomad drives allocs terminal synchronously inside
		// deregister, so this resolves immediately rather than genuinely
		// blocking — it still exercises the blocking-read code path a real
		// cluster would need.
		_, _, _ = l.client.listAllocsForJob(ctx, b.ChildJobID, idx-1, blockingWait)
	}
	return l.sidecar.remove(sessionName)
}

// isForbiddenErr reports a 403 from EITHER path the client uses.
//
// The pack reaches Nomad two ways: its own raw transport, which mints
// errJobForbidden, and the vendored SDK, whose 403 arrives as a plain error
// reading "Unexpected response code: 403". Checking only the sentinel missed
// the SDK path — measured against the live cluster, where `is-running` with no
// token warned that its answer was unknown but never said the cause was
// durable, because the 403 had come through the SDK. The text match is
// deliberate: the SDK exposes no typed status error to assert on, and a
// missed durable case is exactly the permanently-green-fleet this warning
// exists to prevent.
func isForbiddenErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errJobForbidden) {
		return true
	}
	return strings.Contains(err.Error(), "response code: 403")
}

// warnUnknownLiveness says out loud that a `true` was UNKNOWN rather than
// observed.
//
// is-running deliberately answers last-known-good on a transport fault: an
// unreachable API is not evidence a session died, and fabricating a `false`
// would have the controller reap live work. That is the right answer and it
// stays. What was wrong is that it was SILENT, with no counter, no marker and
// no decay — so a revoked or expired token, which fails every call the same
// way forever, produced a permanently green fleet. Every session reports
// running, exit 0, nothing on stderr, indefinitely.
//
// An auth failure gets its own sentence because it is the durable case. A
// dial error may be a blip; a 403 will answer identically on every call until
// somebody changes a credential, so a fleet that looks healthy behind one is
// not briefly stale, it is permanently wrong.
func warnUnknownLiveness(w io.Writer, sessionName, what string, err error) {
	durable := ""
	if isForbiddenErr(err) {
		durable = " This is an AUTHORISATION failure, not a blip: it will answer the same way on " +
			"every call until a credential changes, so every session will keep reporting healthy " +
			"until then."
	}
	fmt.Fprintf(w, "gc-runtime-nomad: warning: session %q reports running because its state is "+
		"UNKNOWN, not because it was observed alive — %s failed: %v.%s\n",
		sessionName, what, err, durable) //nolint:errcheck
}

// probeAgentAlive execs an in-box liveness check into allocID: the tmux
// session launch created (tmuxSessionName) must still exist, AND — when
// pid was captured at launch (capturePanePID) — that exact process must
// still answer kill -0. A box whose alloc stays non-terminal but whose
// agent died inside it (the tmux session and/or its pane process gone) is
// otherwise indistinguishable from a healthy session to an alloc-status-
// only check (the in-box agent-kill case). A transport error execing the
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
// answers false here even though it already answers exec (the launched
// marker is what distinguishes
// "provisioned, agent never launched" from "launched, agent died"). Once
// launched, alive=true requires BOTH the alloc to be non-terminal AND
// probeAgentAlive to confirm the agent process/tmux session is still there
// — alive=false with the box otherwise healthy means the agent died inside
// a surviving box (the in-box agent-kill case), which the alloc's
// ClientStatus alone can never see. The per-op honesty split still
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
		warnUnknownLiveness(l.durableWarn(), sessionName, "listing allocations", err)
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
		warnUnknownLiveness(l.durableWarn(), sessionName, "probing in-box liveness", err)
		return true, nil
	}
	// Best-effort: pick up the detached prime window's marker files if this
	// session is still owed one. A no-op exec-free check
	// once PromptedAt is set or no prompt was ever staged, so this costs
	// nothing on the common path and never affects the liveness answer.
	l.syncPromptedMarker(ctx, sessionName)
	return alive, nil
}

// opListRunning enumerates every launched, non-terminal session:
// the children-of-parent jobs list (meta=true) is the source
// of existence — never the sidecar, which can drift stale — filtered to
// non-terminal children, decoded via each child's `gc_session` Meta key
// (never ID-string parsing), and then to prefix, if
// one was given (ListRunning(prefix)). The sidecar is
// still consulted for exactly one thing the children list cannot answer:
// the launched marker — a provisioned-but-not-
// launched box is not "running", same gate as opIsRunning. On ANY lookup
// error it returns an error rather than a partial list — matching the
// "list-based reap arms defer on ANY error" contract.
// childHasLiveAlloc reports whether a dispatched child still has an allocation
// that could be running a session.
//
// "Live" is deliberately generous: anything that is not a terminal client
// status counts, because a box in Task Setup or pending placement is a session
// on its way up, not a corpse. Only a child whose allocations are ALL terminal
// — or one that has no allocations at all after Nomad has had the chance to
// place it — is dead.
//
// An error is returned rather than a guess. The caller keeps reporting the
// child as running when it cannot tell, because a read failure is not evidence
// of death.
// deadChildReason asks Nomad why a child job's box is no longer live, reusing
// the same task-event reading that names an absent image on the provision path.
// Empty means Nomad offered no reason — report nothing rather than invent one.
func (l *lifecycle) deadChildReason(ctx context.Context, childJobID string) string {
	allocs, _, err := l.client.listAllocsForJob(ctx, childJobID, 0, 0)
	if err != nil || len(allocs) == 0 {
		return ""
	}
	return allocFailureDiagnosis(l.agentImage, allocs, execTaskName)
}

// allocFailureDiagnosis turns an allocation's OWN task state into a sentence that
// names the cause of a failed provision.
//
// T6, measured live 2026-09-11: with GC_NOMAD_AGENT_IMAGE set to a tag that
// exists nowhere, Nomad said exactly what was wrong —
//
//	Driver Failure  Failed to pull `gc-agent:...`: Error response from daemon:
//	pull access denied for gc-agent, repository does not exist
//
// — while the city's own log said only:
//
//	provision worker-local-1-pool: staging workspace for "worker-local-1-pool":
//	reading exec frame: EOF
//
// No image, no node, no pull: an exec frame that ended early, three layers from
// the cause. The operator has to leave the city and read Nomad to learn that the
// image is absent. This function closes that gap by reading the answer Nomad
// already has.
//
// It is deliberately NOT an image special case. The exec frame ends early for
// every reason a task can fail to start — pull denied, OOM, a constraint, a bad
// entrypoint — and each of those arrives here with its own driver text. The
// image gets a named phrasing because it is the one this pack can state
// authoritatively (it owns agentImage); everything else is reported verbatim,
// which is still infinitely better than EOF.
//
// Empty return means "no answer", never a guess: a caller that cannot read the
// allocation keeps its original error rather than inventing a cause.
func allocFailureDiagnosis(agentImage string, allocs []allocRecord, taskName string) string {
	if len(allocs) == 0 {
		// Nothing placed at all is the PLACEMENT shape, which
		// confirmDispatchPlaced already reports with its own message.
		return ""
	}
	// Prefer an allocation that actually failed; fall back to the newest read.
	pick := allocs[len(allocs)-1]
	for _, a := range allocs {
		switch strings.ToLower(strings.TrimSpace(a.ClientStatus)) {
		case "failed", "lost":
			pick = a
		}
	}
	if pick.TaskStates == nil {
		// A read that carried no task states is a stub read, not an answer.
		return ""
	}
	ts, ok := pick.TaskStates[taskName]
	if !ok {
		return ""
	}
	// The FAILURE event, not the newest one. Nomad appends terminal bookkeeping
	// ("Policy allows no restarts") after the real cause, so LastEvent on a
	// missing image reports the restart policy and never names the image.
	event := strings.TrimSpace(ts.FailureEvent)
	if event == "" {
		event = strings.TrimSpace(ts.LastEvent)
	}
	if event == "" {
		return ""
	}
	if isImagePullFailure(event) {
		if img := strings.TrimSpace(agentImage); img != "" {
			if imageIsRegistryless(img) {
				// Not a transient pull fault and not fixable in a registry:
				// this reference can never be pulled by anything, so "not on
				// this node" understates it. It will fail on every node that
				// does not already hold a copy, forever.
				return fmt.Sprintf("the image %q is not on this node AND NAMES NO REGISTRY, so no "+
					"node can ever pull it — a bare name resolves against Docker Hub, which will not "+
					"have a privately built agent image. This allocation did not lose a race with a "+
					"pull; it landed on a node that was never given the image. Push it to a registry "+
					"your clients can reach, or pre-load it on every eligible node: %s", img, event)
			}
			return fmt.Sprintf("the image %q is not on this node: %s", img, event)
		}
		return fmt.Sprintf("the agent image is not on this node: %s", event)
	}
	if isOOMFailure(event) {
		// Its own case because its REMEDY is its own. A pull fault is fixed in
		// a registry, a constraint fault on a client; an OOM is fixed by
		// raising the task's memory or shrinking what runs in it, and the
		// generic "never started" wording below buries that under a sentence
		// about starting.
		return fmt.Sprintf("the %q task was OOM-killed — the box ran out of the memory its "+
			"resources block allows, which is a sizing fault, not a start fault: %s", taskName, event)
	}
	state := strings.TrimSpace(ts.State)
	if state == "" {
		state = "unknown"
	}
	return fmt.Sprintf("the %q task never started (state %q): %s", taskName, state, event)
}

// describeConstraints renders a job's placement rules as the operator would
// have to read them off a node, so a "nothing placed" error can name what was
// required instead of referring to it.
func describeConstraints(cs []nomadConstraint) string {
	if len(cs) == 0 {
		return "no constraints at all (so this is capacity, not placement)"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, strings.TrimSpace(c.LTarget+" "+c.Operand+" "+c.RTarget))
	}
	return strings.Join(parts, ", ")
}

// isOOMFailure reports whether a driver event describes a task the kernel
// killed for memory. Matched on the driver's vocabulary rather than one exact
// string, the same way isImagePullFailure is: the docker driver, the exec
// driver and the kernel each word this differently and all three mean the box
// ran out of memory.
func isOOMFailure(event string) bool {
	e := strings.ToLower(event)
	switch {
	case strings.Contains(e, "oom killed"),
		strings.Contains(e, "oom-killed"),
		strings.Contains(e, "oomkilled"),
		strings.Contains(e, "out of memory"):
		return true
	}
	return false
}

// isImagePullFailure reports whether a driver event describes an image the node
// does not have. Matched on the driver's own vocabulary rather than an exact
// string, because the daemon's wording differs between "not found", a denied
// pull on a private repo, and a registry that cannot be reached — all of which
// mean the same thing to an operator: the image is not on this node.
func isImagePullFailure(event string) bool {
	e := strings.ToLower(event)
	switch {
	case strings.Contains(e, "failed to pull"),
		strings.Contains(e, "pull access denied"),
		strings.Contains(e, "repository does not exist"),
		strings.Contains(e, "manifest unknown"),
		strings.Contains(e, "no such image"),
		strings.Contains(e, "image not found"):
		return true
	}
	return false
}

// provisionFailureDetail reads the live allocation behind sessionName and
// returns allocFailureDiagnosis's sentence for it, or "" when it cannot get an
// answer. Every read failure is silent on purpose: this runs on an error path,
// and a diagnosis that fails must never replace or mask the original error.
func (l *lifecycle) provisionFailureDetail(ctx context.Context, sessionName string) string {
	if l == nil || l.sidecar == nil || l.client == nil {
		return ""
	}
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return fmt.Sprintf("could not read the binding to diagnose this: %v", err)
	}
	if !ok || strings.TrimSpace(b.ChildJobID) == "" {
		// No child bound yet: there is genuinely nothing to ask Nomad about,
		// and saying so beats inventing a cause.
		return ""
	}
	allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
	if err != nil {
		return fmt.Sprintf("could not read the allocations of child %q to diagnose this: %v", b.ChildJobID, err)
	}
	if d := allocFailureDiagnosis(l.agentImage, allocs, execTaskName); d != "" {
		return d
	}
	// Nomad had nothing to say about the task. Report THAT rather than going
	// quiet, because a silent diagnosis is what left the exec-frame EOF standing
	// alone in the first place (T6).
	return fmt.Sprintf("child %q has %d allocation(s) and none reports why task %q is not running",
		b.ChildJobID, len(allocs), execTaskName)
}

func (l *lifecycle) childHasLiveAlloc(ctx context.Context, childJobID string) (bool, error) {
	allocs, _, err := l.client.listAllocsForJob(ctx, childJobID, 0, 0)
	if err != nil {
		return false, err
	}
	if len(allocs) == 0 {
		// No allocation at all. This is the placement-failure shape (T6): the
		// child was dispatched and nothing was ever placed for it.
		return false, nil
	}
	for _, a := range allocs {
		switch strings.ToLower(strings.TrimSpace(a.ClientStatus)) {
		case "complete", "failed", "lost":
			continue
		default:
			return true, nil
		}
	}
	return false, nil
}

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
		// A non-terminal child JOB is not a running SESSION. Killing an
		// allocation does not make its parent dispatch terminal: the job still
		// exists and Nomad has simply lost the alloc. So this loop reported a
		// seat whose box was dead as running, and every caller that asks this
		// verb "is the seat alive" got a confident yes about a corpse.
		//
		// Measured T5, 2026-09-11: alloc SIGKILLed, Nomad reporting
		// ClientStatus=failed and Task "agent" dead twenty-five seconds later,
		// and `gc-runtime-nomad list-running` still naming the seat five
		// minutes on. gc's orphan-release probe consults this verb and fails
		// closed on "still present", so the claim was never released and the
		// bead stayed assigned to a dead seat.
		//
		// FAIL CLOSED on an unreadable allocation list: if the read errors we
		// keep reporting the child as running, exactly as before. A transient
		// Nomad blip must never look like a fleet of dead seats — that would
		// release live work everywhere at once, which is far worse than holding
		// one claim too long.
		if live, err := l.childHasLiveAlloc(ctx, c.ID); err == nil && !live {
			// SAY WHY THE SEAT VANISHED. Dropping it silently is correct for the
			// caller — the seat is gone and must not be reported alive — but it
			// leaves an operator with a lane that quietly shrank and no cause
			// anywhere. Nomad already knows: the allocation's task events carry
			// OOM kills, constraint evictions, a failed pull, a bad entrypoint.
			// Asking costs one read on a path that runs only when a box has
			// actually died: every failure names its cause at the layer that
			// knows.
			if why := l.deadChildReason(ctx, c.ID); why != "" {
				fmt.Fprintf(os.Stderr, "gc-runtime-nomad: seat %q is gone: %s\n", name, why) //nolint:errcheck
			}
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// parentPoolMoveRefusal refuses the one parent-pool change that is never
// intentional: the registered parent names a pool and the calling process
// computed NONE, because GC_NOMAD_NODE_POOL is absent from its environment.
//
// A pool change is not a task-spec change, and conflating them cost a live
// outage. Re-registering to pick up a new task spec is routine — that is what a
// pack upgrade does. Moving the parent to a different NODE POOL repoints where
// every future session is placed, and this used to happen silently whenever the
// calling process's environment differed from the registered job.
//
// Observed 2026-09-12: a hand-run `start`, for diagnosis, without that variable
// set, moved the parent from "prod-services" into Nomad's "default" pool, which
// held zero nodes. Every dispatch then pended forever and nothing on the
// session said why, until the supervisor restored the parent on its next pass.
//
// Widening or moving a pool is still allowed when it is EXPRESSED. An absent
// variable is a forgotten one, not a decision.
func parentPoolMoveRefusal(exists bool, registeredPool, computedPool, parentJobID string) error {
	if !exists || registeredPool == "" || computedPool != "" {
		return nil
	}
	return fmt.Errorf("refusing to re-register parent job %q: it is registered in node pool %q "+
		"and this process computed no pool at all, because %s is unset in ITS environment. "+
		"Re-registering would move the parent to Nomad's \"default\" pool and repoint where every "+
		"future session is placed — if that pool holds no nodes, every dispatch pends forever with "+
		"no error on the session itself. This is refused rather than warned because an absent "+
		"variable is a forgotten one, not a decision: a hand-run of this binary for diagnosis must "+
		"not silently move the fleet. Set %s=%s to proceed with the pool it already has, or to "+
		"another pool to move it deliberately.",
		parentJobID, registeredPool, envNodePool, envNodePool, registeredPool)
}

// parentPoolMoveWarning is the deliberate case: both pools expressed and
// different. That is a decision, so it proceeds — but it is announced, because
// a pool with no eligible nodes leaves every dispatch pending with no error on
// the session.
func parentPoolMoveWarning(exists bool, registeredPool, computedPool, parentJobID string) string {
	if !exists || registeredPool == "" || computedPool == "" || registeredPool == computedPool {
		return ""
	}
	return fmt.Sprintf("re-registering parent %q MOVES its node pool %q -> %q; every future session "+
		"dispatch will be placed in %q. A pool with no eligible nodes leaves dispatches pending with "+
		"no error on the session itself.", parentJobID, registeredPool, computedPool, computedPool)
}

// namedProcessAlive answers the RPP `process-alive` op: are any of these
// process names running inside the session's box?
//
// THE HONESTY SPLIT IS THE WHOLE DESIGN, and the caller's contract makes it
// explicit: gc treats empty output and "true" alike as alive, and only an
// EXPLICIT "false" as a dead agent, precisely so an unimplemented or
// unmeasurable runtime cannot make it reap a live session. So this op may
// answer false only on a definitive observation — a `ps` that ran, succeeded,
// and did not list any of the names. Every other outcome is alive:
//
//	transport error       we did not observe anything
//	`ps` exited nonzero   no `ps` in the image; unmeasurable, not dead
//	no binding / no alloc a different fault with its own message
//
// Answering "dead" from any of those would convert a diagnostic blind spot into
// a reaped session, which is the most expensive mistake this pack could make.
func (l *lifecycle) namedProcessAlive(ctx context.Context, sessionName string, names []string) (alive bool, observed bool, err error) {
	wanted := make([]string, 0, len(names))
	for _, n := range names {
		if t := strings.TrimSpace(n); t != "" {
			wanted = append(wanted, filepath.Base(t))
		}
	}
	if len(wanted) == 0 {
		// The interface contract: no names asked about means nothing to
		// disprove.
		return true, false, nil
	}

	allocID, err := l.currentAlloc(ctx, sessionName)
	if err != nil || allocID == "" {
		return true, false, err
	}

	exitCode, out, execErr := l.client.execAlloc(ctx, allocID, execTaskName,
		[]string{"/bin/sh", "-c", "ps -eo comm 2>/dev/null || ps -o comm 2>/dev/null"})
	if execErr != nil || exitCode != 0 {
		// Unmeasurable, not dead. See the honesty split above.
		return true, false, nil
	}
	return processListHasAny(out, wanted), true, nil
}

// processListHasAny reports whether a `ps -eo comm` listing contains any of the
// wanted names, compared on basename so a listing that prints a full path
// matches a name given bare (and the reverse).
func processListHasAny(psOut []byte, wanted []string) bool {
	for _, line := range strings.Split(string(psOut), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == "COMMAND" {
			continue
		}
		name = filepath.Base(name)
		for _, w := range wanted {
			if name == w {
				return true
			}
		}
	}
	return false
}
