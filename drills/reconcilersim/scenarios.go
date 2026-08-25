package reconcilersim

import (
	"context"
	"fmt"
)

// LifecycleRoundtripSteps is a scripted smoke scenario: start a session
// through the pack CLI, confirm the observer client independently sees a
// running, unreplaced allocation, then stop it and confirm is-running
// flips false. No fault is injected — it exists to prove the harness's own
// plumbing (pack CLI subprocess, optional mTLS front proxy, observer
// client) actually reaches the target end to end, offline against
// fakenomad or live against the L4 lab. It is also the worked example the
// 08 §1.1 per-drill beads (fnrt-o37.3 .. fnrt-o37.14) are expected to
// follow when scripting their own scenario: a short []Step slice built the
// same way, run through RunScript.
func LifecycleRoundtripSteps(session, parentJob string) []Step {
	// The dispatched child job ID is real Nomad's own generated ID (e.g.
	// "<parent>/dispatch-<ts>-<rand>") — never derivable from the session
	// name (e2a-job-id-charset-gap, ../runtime/client.go's childJob
	// comment) — so this looks it up by its gc_session Meta key, exactly
	// how the pack's own list-running op resolves identity.
	childJobIDFor := func(ctx context.Context, d *Driver) (string, error) {
		children, err := d.Observer.ListChildJobs(ctx, parentJob)
		if err != nil {
			return "", err
		}
		for _, c := range children {
			if c.Meta["gc_session"] == session {
				return c.ID, nil
			}
		}
		return "", fmt.Errorf("no child job found with gc_session=%s under parent %s", session, parentJob)
	}

	return []Step{
		{
			Name: "start",
			Do: func(ctx context.Context, d *Driver) error {
				res, err := d.RunOp(ctx, "start", nil, session)
				if err != nil {
					return err
				}
				if res.ExitCode != 0 {
					return fmt.Errorf("start exited %d: %s", res.ExitCode, string(res.Stderr))
				}
				return nil
			},
		},
		{
			Name: "observer sees exactly one unreplaced running alloc",
			Do: func(ctx context.Context, d *Driver) error {
				childJobID, err := childJobIDFor(ctx, d)
				if err != nil {
					return err
				}
				allocs, err := d.Observer.ListAllocsForJob(ctx, childJobID)
				if err != nil {
					return err
				}
				if n := CountReplacementAllocs(allocs); n != 0 {
					return fmt.Errorf("observer saw %d replacement alloc(s), want 0", n)
				}
				latest, err := d.Observer.LatestAlloc(ctx, childJobID)
				if err != nil {
					return err
				}
				if latest.ClientStatus != "running" && latest.ClientStatus != "pending" {
					return fmt.Errorf("latest alloc ClientStatus = %q, want running/pending", latest.ClientStatus)
				}
				return nil
			},
		},
		{
			Name: "is-running via pack CLI",
			Do: func(ctx context.Context, d *Driver) error {
				running, err := d.IsRunning(ctx, session)
				if err != nil {
					return err
				}
				if !running {
					return fmt.Errorf("is-running = false, want true")
				}
				return nil
			},
		},
		{
			Name: "stop",
			Do: func(ctx context.Context, d *Driver) error {
				res, err := d.RunOp(ctx, "stop", nil, session)
				if err != nil {
					return err
				}
				if res.ExitCode != 0 {
					return fmt.Errorf("stop exited %d: %s", res.ExitCode, string(res.Stderr))
				}
				return nil
			},
		},
		{
			Name: "is-running false after stop",
			Do: func(ctx context.Context, d *Driver) error {
				running, err := d.IsRunning(ctx, session)
				if err != nil {
					return err
				}
				if running {
					return fmt.Errorf("is-running = true after stop, want false")
				}
				return nil
			},
		},
	}
}
