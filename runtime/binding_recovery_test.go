package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSidecarBindingRecoveryFromNomadMeta(t *testing.T) {
	t.Run("repairs after a fresh lifecycle and refuses unknown age", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-sidecar-recovery"

		if err := l.opProvision(ctx, session); err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		before, ok, err := l.sidecar.load(session)
		if err != nil || !ok || before.ChildJobID == "" || before.Nonce == "" {
			t.Fatalf("initial binding = (%+v, %v, %v), want a bound child and nonce", before, ok, err)
		}

		sidecarDir := l.sidecar.dir
		if err := os.RemoveAll(sidecarDir); err != nil {
			t.Fatalf("remove sidecar directory: %v", err)
		}
		sc, err := newSidecar(filepath.Join(sidecarDir))
		if err != nil {
			t.Fatalf("recreate sidecar: %v", err)
		}
		fresh := &lifecycle{client: l.client, sidecar: sc, parentJobID: l.parentJobID}

		// The recovered record has no creation timestamp because the jobs list
		// does not provide one. A provision retry must refuse explicitly rather
		// than silently bypassing the adoption age guard or dispatching another
		// child into an unknown-age box.
		if err := fresh.opProvision(ctx, session); err == nil ||
			!strings.Contains(err.Error(), "age") {
			t.Fatalf("provision after sidecar loss = %v, want an explicit unknown-age refusal", err)
		}

		repaired, ok, err := fresh.sidecar.load(session)
		if err != nil || !ok {
			t.Fatalf("repaired binding reload = (%+v, %v, %v), want a binding", repaired, ok, err)
		}
		if repaired.SessionName != session || repaired.ChildJobID != before.ChildJobID ||
			repaired.Namespace != before.Namespace || repaired.Nonce != before.Nonce {
			t.Fatalf("repaired binding = %+v, want session/child/namespace/nonce from Nomad Meta and the existing child", repaired)
		}
		if !repaired.CreatedAt.IsZero() {
			t.Fatalf("repaired CreatedAt = %v, want zero because Nomad jobs-list has no creation timestamp", repaired.CreatedAt)
		}
		if n := countDispatchCalls(srv.Trace(), l.parentJobID); n != 1 {
			t.Fatalf("dispatch calls after recovery refusal = %d, want 1", n)
		}
	})

	t.Run("zero match is loud", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		before := countDispatchCalls(srv.Trace(), l.parentJobID)
		_, err := l.recoverBinding(context.Background(), "sess-no-meta-match", nil)
		if err == nil || !errors.Is(err, errBindingNoMatch) {
			t.Fatalf("recoverBinding with no match = %v, want errBindingNoMatch", err)
		}
		if after := countDispatchCalls(srv.Trace(), l.parentJobID); after != before {
			t.Fatalf("zero-match recovery dispatched %d jobs, want none", after-before)
		}
	})

	t.Run("current allocation resolution repairs a missing binding", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-current-alloc-recovery"

		if err := l.opProvision(ctx, session); err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		before, ok, err := l.sidecar.load(session)
		if err != nil || !ok {
			t.Fatalf("initial binding = (%+v, %v, %v), want a binding", before, ok, err)
		}
		allocs, _, err := l.client.listAllocsForJob(ctx, before.ChildJobID, 0, 0)
		if err != nil || len(allocs) != 1 {
			t.Fatalf("initial allocations = (%v, %v), want one allocation", allocs, err)
		}

		sidecarDir := l.sidecar.dir
		if err := os.RemoveAll(sidecarDir); err != nil {
			t.Fatalf("remove sidecar directory: %v", err)
		}
		sc, err := newSidecar(sidecarDir)
		if err != nil {
			t.Fatalf("recreate sidecar: %v", err)
		}
		fresh := &lifecycle{client: l.client, sidecar: sc, parentJobID: l.parentJobID}
		gotAlloc, err := fresh.currentAlloc(ctx, session)
		if err != nil || gotAlloc != allocs[0].ID {
			t.Fatalf("currentAlloc after sidecar loss = (%q, %v), want %q", gotAlloc, err, allocs[0].ID)
		}
		repaired, ok, err := fresh.sidecar.load(session)
		if err != nil || !ok || repaired.ChildJobID != before.ChildJobID || repaired.Nonce != before.Nonce {
			t.Fatalf("repaired currentAlloc binding = (%+v, %v, %v), want the original child and nonce", repaired, ok, err)
		}
		if n := countDispatchCalls(srv.Trace(), l.parentJobID); n != 1 {
			t.Fatalf("dispatch calls after currentAlloc recovery = %d, want 1", n)
		}
	})

	t.Run("multiple matches are refused", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		if err := l.ensureParentRegistered(ctx); err != nil {
			t.Fatalf("register parent: %v", err)
		}
		for _, nonce := range []string{"nonce-one", "nonce-two"} {
			if _, err := l.client.dispatchChild(ctx, l.parentJobID, "sess-ambiguous", nonce); err != nil {
				t.Fatalf("dispatch candidate %q: %v", nonce, err)
			}
		}
		before := countDispatchCalls(srv.Trace(), l.parentJobID)
		_, err := l.recoverBinding(ctx, "sess-ambiguous", nil)
		if err == nil || !errors.Is(err, errBindingAmbiguous) {
			t.Fatalf("recoverBinding with two matches = %v, want errBindingAmbiguous", err)
		}
		if after := countDispatchCalls(srv.Trace(), l.parentJobID); after != before {
			t.Fatalf("ambiguous recovery dispatched %d jobs, want none", after-before)
		}
	})

	t.Run("terminal match is reported", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		if err := l.ensureParentRegistered(ctx); err != nil {
			t.Fatalf("register parent: %v", err)
		}
		childID, err := l.client.dispatchChild(ctx, l.parentJobID, "sess-terminal-match", "nonce-terminal")
		if err != nil {
			t.Fatalf("dispatch terminal candidate: %v", err)
		}
		allocs, _, err := l.client.listAllocsForJob(ctx, childID, 0, 0)
		if err != nil || len(allocs) != 1 {
			t.Fatalf("candidate allocations = (%v, %v), want one allocation", allocs, err)
		}
		if !srv.SetAllocStatus(allocs[0].ID, "complete") {
			t.Fatalf("set candidate terminal: allocation %q not found", allocs[0].ID)
		}
		before := countDispatchCalls(srv.Trace(), l.parentJobID)
		_, err = l.recoverBinding(ctx, "sess-terminal-match", nil)
		if err == nil || !errors.Is(err, errBindingTerminal) {
			t.Fatalf("recoverBinding with terminal match = %v, want errBindingTerminal", err)
		}
		if after := countDispatchCalls(srv.Trace(), l.parentJobID); after != before {
			t.Fatalf("terminal recovery dispatched %d jobs, want none", after-before)
		}
	})

}
