package reconcilersim

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Driver execs the pack CLI binary for every lifecycle op — the same way
// the real GC controller invokes gc-runtime-nomad (main.go's "gc execs the
// binary directly" calling convention) — and separately holds an Observer
// for the direct Nomad reads classify.go needs. This split is deliberate:
// a scripted drill only ever mutates state through the same RPP surface GC
// itself is bound by, and only ever asserts through independent reads, so
// the harness never gets to "pass" a drill by taking a shortcut a real
// reconciler couldn't take.
type Driver struct {
	binPath  string
	env      []string
	Observer *Client
	// proxy is non-nil when L4 mode fronts the pack CLI subprocess with a
	// local mTLS-terminating proxy (see New); Close tears it down.
	proxy *TLSProxy
}

// New builds a Driver from cfg (typically ConfigFromEnv()). When cfg.TLS is
// set, it starts a local TLSProxy in front of cfg.Addr and points the pack
// CLI subprocess's GC_NOMAD_ADDR at the proxy instead — the pack binary
// itself never learns about TLS. The Observer always talks TLS directly
// (Client.NewClient), never through the proxy, since it has native TLS
// support this package's own code owns.
func New(cfg Config) (*Driver, error) {
	observer, err := NewClient(cfg.Addr, cfg.Token, cfg.Namespace, cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("building observer client: %w", err)
	}

	cliAddr := cfg.Addr
	var proxy *TLSProxy
	if !cfg.TLS.empty() {
		proxy, err = StartTLSProxy(cfg.Addr, cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("starting mTLS front proxy for the pack CLI: %w", err)
		}
		cliAddr = proxy.URL()
	}

	// Base the subprocess env on this process's own environment (so
	// operator-set vars the harness doesn't know about, e.g. PATH, pass
	// through untouched), stripping any of the four vars this Driver
	// controls so the appended, authoritative copies below can't collide
	// with a stale inherited one.
	env := filterEnv(os.Environ(), EnvAddr, EnvToken, EnvNamespace, EnvParentJob)
	env = append(env,
		EnvAddr+"="+cliAddr,
		EnvToken+"="+cfg.Token,
		EnvNamespace+"="+cfg.Namespace,
		EnvParentJob+"="+cfg.ParentJob,
	)

	return &Driver{binPath: cfg.RuntimeBin, env: env, Observer: observer, proxy: proxy}, nil
}

// filterEnv returns env with any entry whose key is in drop removed.
func filterEnv(env []string, drop ...string) []string {
	blocked := make(map[string]bool, len(drop))
	for _, k := range drop {
		blocked[k] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if blocked[key] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// WithEnv returns a copy of d whose pack CLI subprocess env additionally
// carries the given KEY=VALUE entries (e.g. GC_NOMAD_SIDECAR_DIR,
// GC_NOMAD_EGRESS_DIR — per-drill directories a scripted scenario owns and
// that Config intentionally doesn't cover, since they're test/drill-run
// scoped rather than cluster-target config).
func (d *Driver) WithEnv(extra ...string) *Driver {
	nd := *d
	nd.env = append(append([]string{}, d.env...), extra...)
	return &nd
}

// Close tears down the mTLS front proxy, if one was started. A Driver with
// no TLS config is a no-op Close.
func (d *Driver) Close() error {
	if d.proxy == nil {
		return nil
	}
	return d.proxy.Close()
}

// OpResult is one pack CLI subprocess invocation's outcome.
type OpResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// RunOp execs the pack CLI as `<bin> <op> [args...]`, feeding stdin and
// returning its captured stdout/stderr/exit code — never treating a
// non-zero exit as a Go error, since the pack's own 0/1/2 convention
// (main.go) is data a scripted step's Assert needs to see, not a Go-level
// failure this call should hide. Only a failure to start the subprocess at
// all (binary missing, etc.) is a Go error.
func (d *Driver) RunOp(ctx context.Context, op string, stdin []byte, args ...string) (OpResult, error) {
	fullArgs := append([]string{op}, args...)
	cmd := exec.CommandContext(ctx, d.binPath, fullArgs...)
	cmd.Env = d.env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			return OpResult{}, fmt.Errorf("starting %s %s: %w", d.binPath, strings.Join(fullArgs, " "), err)
		}
		exitCode = exitErr.ExitCode()
	}
	return OpResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode}, nil
}

// IsRunning is a typed convenience over RunOp("is-running", ...) — the
// classification helpers (classify.go) want a bool + error, not raw
// stdout bytes to parse inline in every scripted step.
func (d *Driver) IsRunning(ctx context.Context, session string) (bool, error) {
	res, err := d.RunOp(ctx, "is-running", nil, session)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("is-running %s: exit %d: %s", session, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return strconv.ParseBool(strings.TrimSpace(string(res.Stdout)))
}

// Step is one scripted action in a reconciler-sim drill — generalizes
// runtime/reconcilersim_test.go's in-process reconcilerStep shape into a
// standalone, non-*testing.T runner so the same scripted-drill pattern
// works both as an L1/L2 go test (driver_test.go, against fakenomad) and
// as this package's own CLI (cmd/reconcilersim-drill) against a real L4
// lab target. Inject is the caller's fault/kill action — offline it's a
// fakenomad call (srv.SetAllocStatus, srv.FailNext, ...); at L4 it's
// whatever lab-side action the specific drill bead scripts (an SSH kill,
// an nftables partition, ...) — this package intentionally has no opinion
// on how a real box gets killed, only on how the aftermath is classified.
type Step struct {
	Name   string
	Inject func(ctx context.Context, d *Driver) error
	Do     func(ctx context.Context, d *Driver) error
	// Assert reports a failure message (empty = pass) after Do returns.
	// It receives Do's error so it can distinguish "failed as expected"
	// from "failed unexpectedly".
	Assert func(doErr error) (failMsg string)
}

// StepResult is one Step's recorded outcome.
type StepResult struct {
	Name    string
	Elapsed time.Duration
	Passed  bool
	FailMsg string
	DoErr   error
}

// Report accumulates StepResults across a whole scripted drill.
type Report struct {
	Steps []StepResult
}

// AllPassed reports whether every step in the report passed.
func (r Report) AllPassed() bool {
	for _, s := range r.Steps {
		if !s.Passed {
			return false
		}
	}
	return true
}

// RunScript drives d through steps in order — inject, then do, then assert,
// same as runtime/reconcilersim_test.go's runReconcilerDrill — accumulating
// a Report instead of calling t.Fatalf, so the identical script can run
// under go test (a driver_test.go wraps this and fails the test on
// !report.AllPassed()) or under the standalone CLI (main.go prints the
// report and sets the exit code). A step whose Inject or Do returns an
// error still runs its Assert (many steps expect Do to fail — e.g. "start
// during outage fails" — Assert is what judges whether that error was the
// right one), but a step whose Inject fails skips Do and Assert entirely,
// since an unscripted inject failure means the drill's own setup is broken,
// not that anything about the target was observed.
func RunScript(ctx context.Context, d *Driver, steps []Step) Report {
	var report Report
	for _, step := range steps {
		start := time.Now()
		result := StepResult{Name: step.Name}

		if step.Inject != nil {
			if err := step.Inject(ctx, d); err != nil {
				result.Passed = false
				result.FailMsg = fmt.Sprintf("inject failed: %v", err)
				result.Elapsed = time.Since(start)
				report.Steps = append(report.Steps, result)
				continue
			}
		}

		var doErr error
		if step.Do != nil {
			doErr = step.Do(ctx, d)
		}
		result.DoErr = doErr

		if step.Assert != nil {
			if msg := step.Assert(doErr); msg != "" {
				result.FailMsg = msg
			} else {
				result.Passed = true
			}
		} else {
			result.Passed = doErr == nil
			if doErr != nil {
				result.FailMsg = doErr.Error()
			}
		}

		result.Elapsed = time.Since(start)
		report.Steps = append(report.Steps, result)
	}
	return report
}
