package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
)

func TestSidecarBindingRecoveryFromNomadMeta(t *testing.T) {
	t.Run("repairs after a fresh lifecycle and preserves the dispatch age", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-sidecar-recovery"

		fakeBin := t.TempDir()
		if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(`#!/bin/sh
case "$1" in
has-session) exit 1 ;;
*) exit 1 ;;
esac
`), 0o700); err != nil {
			t.Fatalf("write fake tmux: %v", err)
		}
		t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

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

		if err := fresh.opProvision(ctx, session); err != nil {
			t.Fatalf("provision after sidecar loss = %v, want the dated box adopted", err)
		}

		repaired, ok, err := fresh.sidecar.load(session)
		if err != nil || !ok {
			t.Fatalf("repaired binding reload = (%+v, %v, %v), want a binding", repaired, ok, err)
		}
		if repaired.SessionName != session || repaired.ChildJobID != before.ChildJobID ||
			repaired.Namespace != before.Namespace || repaired.Nonce != before.Nonce {
			t.Fatalf("repaired binding = %+v, want session/child/namespace/nonce from Nomad Meta and the existing child", repaired)
		}
		if repaired.CreatedAt.IsZero() || !repaired.CreatedAt.Equal(before.CreatedAt) {
			t.Fatalf("repaired CreatedAt = %v, want the original dispatch time %v", repaired.CreatedAt, before.CreatedAt)
		}
		if n := countDispatchCalls(srv.Trace(), l.parentJobID); n != 1 {
			t.Fatalf("dispatch calls after recovery = %d, want 1", n)
		}
	})

	t.Run("refuses a genuinely undated recovered child", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		const session = "sess-undated-recovery"

		if err := l.ensureParentRegistered(ctx); err != nil {
			t.Fatalf("register parent: %v", err)
		}
		options := (&nomadapi.WriteOptions{Namespace: l.client.namespace}).WithContext(ctx)
		if _, _, err := l.client.nomad.Jobs().Dispatch(
			l.parentJobID,
			map[string]string{"gc_session": session, "gc_nonce": "nonce-undated"},
			nil,
			"",
			options,
		); err != nil {
			t.Fatalf("dispatch undated child: %v", err)
		}

		sc, err := newSidecar(filepath.Join(t.TempDir(), "sidecar"))
		if err != nil {
			t.Fatalf("new sidecar: %v", err)
		}
		fresh := &lifecycle{client: l.client, sidecar: sc, parentJobID: l.parentJobID}
		if err := fresh.opProvision(ctx, session); err == nil || !strings.Contains(err.Error(), "age") {
			t.Fatalf("provision for undated child = %v, want explicit unknown-age refusal", err)
		}
		repaired, ok, err := fresh.sidecar.load(session)
		if err != nil || !ok || !repaired.CreatedAt.IsZero() {
			t.Fatalf("undated repaired binding = (%+v, %v, %v), want a zero timestamp preserved for refusal", repaired, ok, err)
		}
		if n := countDispatchCalls(srv.Trace(), l.parentJobID); n != 1 {
			t.Fatalf("dispatch calls after undated recovery = %d, want 1", n)
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
			if _, err := l.client.dispatchChild(ctx, l.parentJobID, "sess-ambiguous", nonce, ""); err != nil {
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
		childID, err := l.client.dispatchChild(ctx, l.parentJobID, "sess-terminal-match", "nonce-terminal", "")
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

	t.Run("terminal residue does not mask the live child", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		if err := l.ensureParentRegistered(ctx); err != nil {
			t.Fatalf("register parent: %v", err)
		}
		const session = "sess-terminal-residue"
		staleID, err := l.client.dispatchChild(ctx, l.parentJobID, session, "nonce-stale", "")
		if err != nil {
			t.Fatalf("dispatch stale candidate: %v", err)
		}
		liveID, err := l.client.dispatchChild(ctx, l.parentJobID, session, "nonce-live", "")
		if err != nil {
			t.Fatalf("dispatch live candidate: %v", err)
		}
		allocs, _, err := l.client.listAllocsForJob(ctx, staleID, 0, 0)
		if err != nil || len(allocs) != 1 {
			t.Fatalf("stale candidate allocations = (%v, %v), want one allocation", allocs, err)
		}
		if !srv.SetAllocStatus(allocs[0].ID, "complete") {
			t.Fatalf("set stale candidate terminal: allocation %q not found", allocs[0].ID)
		}

		// D3 canary signature (cr-x6nr04, 2026-09-10): the session's earlier
		// box was stopped or reaped and its terminal child is still in the
		// parent's jobs list while a live child exists. Terminal residue is
		// not a candidate, so it must not be counted; counting it read one
		// live box plus one dead child as a disputed binding and refused the
		// start on every retry.
		before := countDispatchCalls(srv.Trace(), l.parentJobID)
		got, err := l.recoverBinding(ctx, session, nil)
		if err != nil {
			t.Fatalf("recoverBinding with terminal residue = %v, want the live child %q recovered", err, liveID)
		}
		if got.ChildJobID != liveID || got.Nonce != "nonce-live" || got.SessionName != session {
			t.Fatalf("recovered binding = %+v, want child %q with nonce-live for session %q", got, liveID, session)
		}
		if after := countDispatchCalls(srv.Trace(), l.parentJobID); after != before {
			t.Fatalf("recovery from terminal residue dispatched %d jobs, want none", after-before)
		}
	})

	t.Run("terminal residue never outnumbers a live child into ambiguity", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		if err := l.ensureParentRegistered(ctx); err != nil {
			t.Fatalf("register parent: %v", err)
		}
		const session = "sess-two-terminal-one-live"
		for _, nonce := range []string{"nonce-dead-a", "nonce-dead-b"} {
			childID, err := l.client.dispatchChild(ctx, l.parentJobID, session, nonce, "")
			if err != nil {
				t.Fatalf("dispatch terminal candidate %q: %v", nonce, err)
			}
			allocs, _, err := l.client.listAllocsForJob(ctx, childID, 0, 0)
			if err != nil || len(allocs) != 1 {
				t.Fatalf("terminal candidate %q allocations = (%v, %v), want one allocation", childID, allocs, err)
			}
			if !srv.SetAllocStatus(allocs[0].ID, "complete") {
				t.Fatalf("set terminal candidate %q: allocation %q not found", childID, allocs[0].ID)
			}
		}
		liveID, err := l.client.dispatchChild(ctx, l.parentJobID, session, "nonce-live", "")
		if err != nil {
			t.Fatalf("dispatch live candidate: %v", err)
		}

		got, err := l.recoverBinding(ctx, session, nil)
		if err != nil {
			t.Fatalf("recoverBinding with two terminal children and one live = %v, want the live child %q recovered", err, liveID)
		}
		if got.ChildJobID != liveID || got.Nonce != "nonce-live" {
			t.Fatalf("recovered binding = %+v, want child %q with nonce-live", got, liveID)
		}
	})

	t.Run("only terminal residue is still a loud terminal refusal", func(t *testing.T) {
		l, srv := newTestLifecycle(t)
		ctx := context.Background()
		if err := l.ensureParentRegistered(ctx); err != nil {
			t.Fatalf("register parent: %v", err)
		}
		const session = "sess-terminal-only-pair"
		for _, nonce := range []string{"nonce-dead-c", "nonce-dead-d"} {
			childID, err := l.client.dispatchChild(ctx, l.parentJobID, session, nonce, "")
			if err != nil {
				t.Fatalf("dispatch terminal candidate %q: %v", nonce, err)
			}
			allocs, _, err := l.client.listAllocsForJob(ctx, childID, 0, 0)
			if err != nil || len(allocs) != 1 {
				t.Fatalf("terminal candidate %q allocations = (%v, %v), want one allocation", childID, allocs, err)
			}
			if !srv.SetAllocStatus(allocs[0].ID, "complete") {
				t.Fatalf("set terminal candidate %q: allocation %q not found", childID, allocs[0].ID)
			}
		}

		// With nothing live left to recover there is nothing to adopt, and a
		// terminal match is the shipped evidence of a stale or disputed
		// binding. Ignoring residue for the count must not turn this into a
		// silent no-match.
		before := countDispatchCalls(srv.Trace(), l.parentJobID)
		_, err := l.recoverBinding(ctx, session, nil)
		if err == nil || !errors.Is(err, errBindingTerminal) {
			t.Fatalf("recoverBinding with two terminal matches = %v, want errBindingTerminal", err)
		}
		if after := countDispatchCalls(srv.Trace(), l.parentJobID); after != before {
			t.Fatalf("terminal-only recovery dispatched %d jobs, want none", after-before)
		}
	})
}
