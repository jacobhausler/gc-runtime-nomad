package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
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
	if _, err := c.dispatchChild(context.Background(), "parent", "worker", "nonce-1", ""); err != nil {
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

func TestNewClientUsesGCNomadMTLSPaths(t *testing.T) {
	testNewClientUsesMTLSPaths(t, "GC_NOMAD_")
}

func TestNewClientUsesNomadMTLSPaths(t *testing.T) {
	testNewClientUsesMTLSPaths(t, "NOMAD_")
}

func testNewClientUsesMTLSPaths(t *testing.T, envPrefix string) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Nomad-Index", "42")
		_, _ = w.Write([]byte(`[{"ID":"alloc-1","ClientStatus":"running","DesiredStatus":"run"}]`))
	}))
	server.StartTLS()
	server.TLS.ClientAuth = tls.RequireAnyClientCert
	t.Cleanup(server.Close)

	paths := t.TempDir()
	caPath := filepath.Join(paths, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write server CA: %v", err)
	}
	certPath, keyPath := writeTestClientCertificate(t, paths)

	for _, name := range []string{"NOMAD_CACERT", "NOMAD_CLIENT_CERT", "NOMAD_CLIENT_KEY", "NOMAD_TLS_SERVER_NAME", "GC_NOMAD_CACERT", "GC_NOMAD_CLIENT_CERT", "GC_NOMAD_CLIENT_KEY", "GC_NOMAD_TLS_SERVER_NAME"} {
		t.Setenv(name, "")
	}
	t.Setenv(envPrefix+"CACERT", caPath)
	t.Setenv(envPrefix+"CLIENT_CERT", certPath)
	t.Setenv(envPrefix+"CLIENT_KEY", keyPath)

	c, err := newClient(server.URL, "", "sessions")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	allocs, index, err := c.listAllocsForJob(context.Background(), "parent", 41, 2*time.Second)
	if err != nil {
		t.Fatalf("listAllocsForJob over mTLS: %v", err)
	}
	if len(allocs) != 1 || allocs[0].ID != "alloc-1" {
		t.Fatalf("allocations = %#v, want alloc-1", allocs)
	}
	if index != 42 {
		t.Fatalf("query index = %d, want 42", index)
	}
}

func TestApplyGCNomadTLSConfigPrefersPackAliases(t *testing.T) {
	t.Setenv("NOMAD_CACERT", "nomad-ca.pem")
	t.Setenv("NOMAD_CLIENT_CERT", "nomad-client.pem")
	t.Setenv("NOMAD_CLIENT_KEY", "nomad-client-key.pem")
	t.Setenv("NOMAD_TLS_SERVER_NAME", "nomad.example")
	t.Setenv("GC_NOMAD_CACERT", "gc-ca.pem")
	t.Setenv("GC_NOMAD_CLIENT_CERT", "gc-client.pem")
	t.Setenv("GC_NOMAD_CLIENT_KEY", "gc-client-key.pem")
	t.Setenv("GC_NOMAD_TLS_SERVER_NAME", "gc.example")

	config := &nomadapi.TLSConfig{
		CACert:        os.Getenv("NOMAD_CACERT"),
		ClientCert:    os.Getenv("NOMAD_CLIENT_CERT"),
		ClientKey:     os.Getenv("NOMAD_CLIENT_KEY"),
		TLSServerName: os.Getenv("NOMAD_TLS_SERVER_NAME"),
	}
	applyGCNomadTLSConfig(config)

	if config.CACert != "gc-ca.pem" || config.ClientCert != "gc-client.pem" || config.ClientKey != "gc-client-key.pem" || config.TLSServerName != "gc.example" {
		t.Fatalf("TLS config = %+v, want GC_NOMAD_* aliases to override NOMAD_* values", config)
	}
}

func writeTestClientCertificate(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          newSerialNumber(t),
		Subject:               pkix.Name{CommonName: "test-client"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	certPath = filepath.Join(dir, "client.pem")
	keyPath = filepath.Join(dir, "client-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write client certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	return certPath, keyPath
}

func newSerialNumber(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate certificate serial: %v", err)
	}
	return serial
}
