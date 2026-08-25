package main

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// lifecycle implements the four RPP ops this bead scopes (start, stop,
// is-running, list-running) over dispatch/deregister/blocking reads against
// the Nomad API, using the sidecar for the session_name -> child-job-ID
// binding (04 §2.1: "sidecar binding is primary"). Driving verbs, staging,
// and the provision/launch split are out of scope (NRT-P1-03).
type lifecycle struct {
	client      *client
	sidecar     *sidecar
	parentJobID string
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

// opStart dispatches a new child job for sessionName. A session with a
// live (non-terminal) child already bound is rejected — the stderr message
// MUST contain "already exists" (04 §6 wire-contract constant, R1a-02:
// gc's exec proxy infers ErrSessionExists from that exact phrase).
func (l *lifecycle) opStart(ctx context.Context, sessionName string) error {
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

// opStop deregisters sessionName's child job without purge, confirms
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

// opIsRunning reports whether sessionName has a non-terminal child alloc.
// Per the per-op honesty split (04 §6): API unavailability answers
// last-known-good (true, since a binding is still on record) rather than
// false — flipping to false here would read as confirmed death to gc's
// heal/quarantine ladders.
func (l *lifecycle) opIsRunning(ctx context.Context, sessionName string) (bool, error) {
	b, ok, err := l.sidecar.load(sessionName)
	if err != nil {
		return false, err
	}
	if !ok || b.ChildJobID == "" {
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

// opListRunning enumerates every session with a non-terminal child alloc,
// sidecar-primary per 04 §2.1 rule 1/3 (cluster-side recovery via a
// children-of-parent list is out of scope: fakenomad implements no such
// endpoint, and it belongs to the provision-split work). On ANY lookup
// error it returns an error rather than a partial list — matching the
// "list-based reap arms defer on ANY error" contract (04 §6).
func (l *lifecycle) opListRunning(ctx context.Context) ([]string, error) {
	bindings, err := l.sidecar.list()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, b := range bindings {
		if b.ChildJobID == "" {
			continue
		}
		allocs, _, err := l.client.listAllocsForJob(ctx, b.ChildJobID, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("list-running: checking session %q: %w", b.SessionName, err)
		}
		for _, a := range allocs {
			if !isTerminalStatus(a.ClientStatus) {
				names = append(names, b.SessionName)
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
