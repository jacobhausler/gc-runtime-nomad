package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gc-runtime-nomad/fakenomad"
)

// reconcilerStep is one scripted action in a reconciler-sim drill: an
// optional fault to inject against the fake Nomad server immediately
// before the step runs, the lifecycle call to make, and an assertion on
// the resulting error.
type reconcilerStep struct {
	name   string
	inject func(*fakenomad.Server)
	do     func(context.Context, *lifecycle) error
	assert func(*testing.T, error)
}

// runReconcilerDrill drives l through steps in order against srv: inject,
// then do, then assert, for each step in turn. It is this pack's scripted
// reconciler-sim harness (NRT-P1-09) — a reusable "inject fault X, run op
// Y, expect Z" shape so the L4 drills that replay these scenarios
// end-to-end (owner assigned per R3b-14) can script sequences the same way
// this L2 suite does, without re-deriving per-step wiring.
func runReconcilerDrill(t *testing.T, l *lifecycle, srv *fakenomad.Server, steps []reconcilerStep) {
	t.Helper()
	ctx := context.Background()
	for _, step := range steps {
		if step.inject != nil {
			step.inject(srv)
		}
		err := step.do(ctx, l)
		if step.assert != nil {
			step.assert(t, err)
		}
	}
}

// TestReconcilerSimOutageThenRecoveryDrill scripts the shape an L4 drill
// exercises live: a start attempt during an outage fails, the outage
// clears, and a subsequent start on the same session succeeds and is
// confirmed running.
func TestReconcilerSimOutageThenRecoveryDrill(t *testing.T) {
	l, srv := newTestLifecycle(t)
	const session = "sess-drill"

	if err := l.client.registerJob(context.Background(), parentJobSpec("default", l.parentJobID)); err != nil {
		t.Fatalf("registerJob: %v", err)
	}

	runReconcilerDrill(t, l, srv, []reconcilerStep{
		{
			name: "start during outage fails",
			inject: func(s *fakenomad.Server) {
				s.FailNext("POST", "/v1/job/"+l.parentJobID+"/dispatch", 500, `{"error":"injected outage"}`)
			},
			do: func(ctx context.Context, l *lifecycle) error { return l.opStart(ctx, session) },
			assert: func(t *testing.T, err error) {
				if err == nil {
					t.Fatalf("start during outage = nil error, want failure")
				}
			},
		},
		{
			name: "recovered start succeeds",
			do:   func(ctx context.Context, l *lifecycle) error { return l.opStart(ctx, session) },
			assert: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("start after recovery: %v", err)
				}
			},
		},
		{
			name: "is-running confirms",
			do: func(ctx context.Context, l *lifecycle) error {
				running, err := l.opIsRunning(ctx, session)
				if err == nil && !running {
					return fmt.Errorf("is-running after recovered start = false, want true")
				}
				return err
			},
			assert: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("is-running: %v", err)
				}
			},
		},
	})
}
