// Package reconcilersim is the L4 drill harness (NRT-P2-06a, 08 §1.1
// fixture): "the L4 harness = pack CLI + scripted reconciler-sim driver,
// standing in for GC until L5". It drives the already-shipped
// gc-runtime-nomad binary as a subprocess for every lifecycle op — exactly
// how the real GC execs it — and separately reads the Nomad API directly
// (this file) for the observations no RPP op exposes: raw allocation
// status, job/allocation counts, and index-ordered timestamps. Those two
// legs together let a scripted drill classify agent-death vs box-death,
// check the observation-honesty split, age staleness, count replacement
// allocs, and check egress-before-deregister ordering (see classify.go).
//
// Offline-testable against fakenomad (driver_test.go); in L4 mode this
// package's client talks TLS directly to the real T1 lab cluster using the
// NRT-P2-02 mTLS baseline (client cert/key + CA, GC_NOMAD_TLS_* env,
// config.go) — a capability deliberately NOT added to the production
// ../runtime/client.go (NRT-P1-04/NRT-P2-05: that binary's HTTP client
// stays zero-TLS-config by design). The pack CLI subprocess itself still
// has no TLS support, so L4 mode fronts it with a local mTLS-terminating
// reverse proxy (tlsproxy.go) instead of reaching into the pack.
package reconcilersim

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultTimeout = 10 * time.Second

// Client is a read-only Nomad API client scoped to exactly what
// classification needs: allocation status for a job, and the children-of-
// parent job list. It never registers, dispatches, deregisters, or execs —
// every mutating action a drill needs goes through the pack CLI (Driver),
// never around it, so the harness observes the same wire contract the pack
// itself is bound by.
type Client struct {
	addr      *url.URL
	token     string
	namespace string
	http      *http.Client
}

// TLSConfig names the three NRT-P2-02 mTLS materials (CA cert, client cert,
// client key — all file paths) a real T1 lab connection needs. The zero
// value means "no TLS" (fakenomad's plain-HTTP L1/L2 fixture).
type TLSConfig struct {
	CACertPath string
	CertPath   string
	KeyPath    string
}

func (t TLSConfig) empty() bool {
	return t.CACertPath == "" && t.CertPath == "" && t.KeyPath == ""
}

// NewClient builds a Client. namespace defaults to "default" like the
// production pack (client.go's nsQuery convention). A non-empty tlsCfg
// requires all three fields set (mTLS is all-or-nothing against the P2-02
// baseline: verify_https_client=true rejects a client cert without a key,
// and the CA is required to verify the lab server's own leaf cert).
func NewClient(addr, token, namespace string, tlsCfg TLSConfig) (*Client, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("nomad API address is required")
	}
	parsed, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parsing nomad API address: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("nomad API address must be an absolute URL, got %q", addr)
	}
	if namespace == "" {
		namespace = "default"
	}

	httpClient := &http.Client{Timeout: defaultTimeout}
	if !tlsCfg.empty() {
		transport, err := tlsTransport(tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("building mTLS transport: %w", err)
		}
		httpClient.Transport = transport
	}

	return &Client{addr: parsed, token: token, namespace: namespace, http: httpClient}, nil
}

// tlsTransport builds an http.Transport presenting the client cert/key and
// trusting only the given CA — the NRT-P2-02 baseline's mTLS shape
// (verify_server_hostname + verify_https_client both true).
func tlsTransport(cfg TLSConfig) (*http.Transport, error) {
	if cfg.CACertPath == "" || cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, fmt.Errorf("TLS config requires CA cert, client cert, and client key (got ca=%q cert=%q key=%q)", cfg.CACertPath, cfg.CertPath, cfg.KeyPath)
	}
	caPEM, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA cert at %s contained no usable certificates", cfg.CACertPath)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
		},
	}, nil
}

// URL returns the base address this client observes — used by Driver to
// derive the pack CLI subprocess's GC_NOMAD_ADDR (directly if there's no
// TLS, or the local proxy's URL if there is).
func (c *Client) URL() string { return c.addr.String() }

func (c *Client) nsQuery() url.Values {
	v := url.Values{}
	if c.namespace != "" && c.namespace != "default" {
		v.Set("namespace", c.namespace)
	}
	return v
}

// AllocRecord is the subset of a Nomad allocation classification needs:
// status pair plus the response index it was observed at, so a caller can
// order two reads without wall-clock timing (matches ../runtime/client.go's
// index-based consistency model, e2a-blocking-queries).
type AllocRecord struct {
	ID            string
	ClientStatus  string
	DesiredStatus string
	ModifyIndex   uint64
}

// ChildJob is one child-of-parent job, decoded the same way
// ../runtime/client.go's listChildJobs does (04 §2.1 rule 2/3: enumerate
// children, don't trust the sidecar as the existence source).
type ChildJob struct {
	ID       string
	Status   string
	Terminal bool
	Meta     map[string]string
}

// ListAllocsForJob reads every allocation ever placed for jobID, most
// recent last (Nomad's own ordering). A drill uses the full slice (not just
// the latest) to count replacement allocs (classify.go's
// CountReplacementAllocs) — the "zero replacement allocs" pass criterion on
// the box-kill/client-agent-kill/drain drills needs to see every alloc that
// ever existed, not just the current one.
func (c *Client) ListAllocsForJob(ctx context.Context, jobID string) ([]AllocRecord, error) {
	var out []AllocRecord
	if err := c.get(ctx, []string{"v1", "job", jobID, "allocations"}, c.nsQuery(), &out); err != nil {
		return nil, fmt.Errorf("listing allocations for job %s: %w", jobID, err)
	}
	return out, nil
}

// ListChildJobs reads every job whose ParentID is parentJobID — the same
// "children of parent" enumeration ../runtime/client.go's list-running op
// uses, exposed here read-only so a drill can independently confirm what
// the pack CLI itself would answer.
func (c *Client) ListChildJobs(ctx context.Context, parentJobID string) ([]ChildJob, error) {
	q := c.nsQuery()
	q.Set("meta", "true")
	var raw []struct {
		ID       string
		ParentID string
		Status   string
		Meta     map[string]string
	}
	if err := c.get(ctx, []string{"v1", "jobs"}, q, &raw); err != nil {
		return nil, fmt.Errorf("listing nomad jobs: %w", err)
	}
	var children []ChildJob
	for _, j := range raw {
		if j.ParentID != parentJobID {
			continue
		}
		children = append(children, ChildJob{ID: j.ID, Status: j.Status, Terminal: j.Status != "running", Meta: j.Meta})
	}
	return children, nil
}

// LatestAlloc returns the allocation with the highest ModifyIndex for
// jobID — the "current" alloc a drill classifies a kill's aftermath
// against (ClassifyDeath in classify.go compares two LatestAlloc calls
// straddling the kill). Returns an error if jobID has no allocations yet.
func (c *Client) LatestAlloc(ctx context.Context, jobID string) (AllocRecord, error) {
	allocs, err := c.ListAllocsForJob(ctx, jobID)
	if err != nil {
		return AllocRecord{}, err
	}
	if len(allocs) == 0 {
		return AllocRecord{}, fmt.Errorf("job %s has no allocations", jobID)
	}
	latest := allocs[0]
	for _, a := range allocs[1:] {
		if a.ModifyIndex > latest.ModifyIndex {
			latest = a
		}
	}
	return latest, nil
}

func (c *Client) get(ctx context.Context, parts []string, query url.Values, out any) error {
	u := *c.addr
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.Join(parts, "/")
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("X-Nomad-Token", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("nomad: %s not found", u.Path)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("nomad: %s %s: unexpected status %s", req.Method, u.Path, resp.Status)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(out)
}
