package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDispatchChildCarriesStableIdempotencyToken(t *testing.T) {
	var gotMethod, gotToken, gotNamespace string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotToken = r.URL.Query().Get("idempotency_token")
		gotNamespace = r.URL.Query().Get("namespace")
		w.Header().Set("X-Nomad-Index", "7")
		_ = json.NewEncoder(w).Encode(dispatchResult{DispatchedJobID: "parent/dispatch-1"})
	}))
	t.Cleanup(server.Close)

	c, err := newClient(server.URL, "", "sessions")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if _, err := c.dispatchChild(context.Background(), "parent", "worker", "nonce-1"); err != nil {
		t.Fatalf("dispatchChild: %v", err)
	}
	if gotNamespace != "sessions" {
		t.Fatalf("namespace query = %q, want %q", gotNamespace, "sessions")
	}
	if gotToken != "worker:nonce-1" {
		t.Fatalf("idempotency token = %q, want stable session/nonce token", gotToken)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("dispatch method = %q, want native Nomad PUT", gotMethod)
	}
}

func TestListAllocsUsesNativeBlockingQuery(t *testing.T) {
	var gotIndex, gotWait, gotNamespace string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIndex = r.URL.Query().Get("index")
		gotWait = r.URL.Query().Get("wait")
		gotNamespace = r.URL.Query().Get("namespace")
		w.Header().Set("X-Nomad-Index", "42")
		_, _ = w.Write([]byte(`[{"ID":"alloc-1","ClientStatus":"running","DesiredStatus":"run"}]`))
	}))
	t.Cleanup(server.Close)

	c, err := newClient(server.URL, "", "sessions")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	allocs, index, err := c.listAllocsForJob(context.Background(), "parent", 41, 2*time.Second)
	if err != nil {
		t.Fatalf("listAllocsForJob: %v", err)
	}
	if len(allocs) != 1 || allocs[0].ID != "alloc-1" {
		t.Fatalf("allocations = %#v, want alloc-1", allocs)
	}
	if index != 42 {
		t.Fatalf("query index = %d, want 42", index)
	}
	if gotIndex != "41" || gotWait != "2000ms" || gotNamespace != "sessions" {
		t.Fatalf("query = index %q wait %q namespace %q, want 41/2000ms/sessions", gotIndex, gotWait, gotNamespace)
	}
}
