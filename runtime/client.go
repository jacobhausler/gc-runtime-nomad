package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 10 * time.Second
	blockingWait    = 5 * time.Second
	maxResponseByte = 1 << 20
)

// errJobGone marks a 404 from the Nomad API against a job route — the
// confirmed-absence signal (04 §2.1 rule 7): a DEFAULT-consistency 404 on a
// direct job GET, or (for deregister/dispatch) the job never having existed.
var errJobGone = errors.New("nomad: job gone")

// client speaks the subset of the Nomad HTTP API the lifecycle ops need:
// job register (parent), dispatch, deregister, and blocking-capable job/
// allocation reads. Stdlib-only, matching the pack's zero-gascity-imports
// contract (mirrors runtime-cloudflare/runtime/client.go).
type client struct {
	addr      *url.URL
	token     string
	namespace string
	http      *http.Client
}

func newClient(addr, token, namespace string) (*client, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("nomad API address is required (set %s)", envAddr)
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
	return &client{
		addr:      parsed,
		token:     token,
		namespace: namespace,
		http:      &http.Client{Timeout: defaultTimeout},
	}, nil
}

// registerJob upserts a job spec. Nomad's job register is idempotent, so
// callers can call this on every start without a check-then-act race.
func (c *client) registerJob(ctx context.Context, job nomadJob) error {
	return c.do(ctx, defaultTimeout, http.MethodPost, []string{"v1", "jobs"}, map[string]any{"Job": job}, nil)
}

type dispatchResult struct {
	DispatchedJobID string
}

// dispatchChild dispatches a child of parentID, carrying only non-secret
// attribution in Meta (04 §2.1: gc_session + a per-dispatch nonce — never a
// capability).
func (c *client) dispatchChild(ctx context.Context, parentID, sessionName, nonce string) (string, error) {
	body := map[string]any{
		"Meta": map[string]string{"gc_session": sessionName, "gc_nonce": nonce},
	}
	var out dispatchResult
	if err := c.do(ctx, defaultTimeout, http.MethodPost, []string{"v1", "job", parentID, "dispatch"}, body, &out); err != nil {
		return "", err
	}
	if out.DispatchedJobID == "" {
		return "", fmt.Errorf("nomad dispatch: empty DispatchedJobID in response")
	}
	return out.DispatchedJobID, nil
}

// allocRecord is the subset of a Nomad allocation the lifecycle ops need.
type allocRecord struct {
	ID            string
	ClientStatus  string
	DesiredStatus string
}

// listAllocsForJob reads the allocations placed for jobID. If wait > 0 and
// sinceIndex > 0, it performs a blocking read (index/wait query params) that
// resolves as soon as the server's index advances past sinceIndex, or wait
// elapses — the "confirm terminal" mechanism 04 §4/§6 call for around stop
// and duplicate-start detection.
func (c *client) listAllocsForJob(ctx context.Context, jobID string, sinceIndex uint64, wait time.Duration) ([]allocRecord, uint64, error) {
	parts := []string{"v1", "job", jobID, "allocations"}
	q := url.Values{}
	if sinceIndex > 0 && wait > 0 {
		q.Set("index", strconv.FormatUint(sinceIndex, 10))
		q.Set("wait", wait.String())
	}
	var out []allocRecord
	idx, err := c.doIndexed(ctx, blockingTimeout(wait), http.MethodGet, parts, q, nil, &out)
	if err != nil {
		return nil, 0, err
	}
	return out, idx, nil
}

// deregisterJob deregisters jobID and returns the response index (0 if the
// job was already gone). purge=false matches the stop-without-purge
// ordering invariant (04 §3 stop row, R1c-05); a 404 (already gone) is
// folded to success so stop stays idempotent.
func (c *client) deregisterJob(ctx context.Context, jobID string, purge bool) (uint64, error) {
	parts := []string{"v1", "job", jobID}
	q := url.Values{}
	if purge {
		q.Set("purge", "true")
	}
	var out struct {
		JobModifyIndex uint64
	}
	_, err := c.doIndexed(ctx, defaultTimeout, http.MethodDelete, parts, q, nil, &out)
	if errors.Is(err, errJobGone) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return out.JobModifyIndex, nil
}

func blockingTimeout(wait time.Duration) time.Duration {
	if wait <= 0 {
		return defaultTimeout
	}
	return wait + defaultTimeout
}

func (c *client) do(ctx context.Context, timeout time.Duration, method string, parts []string, body, out any) error {
	_, err := c.doIndexed(ctx, timeout, method, parts, nil, body, out)
	return err
}

// doIndexed performs one Nomad API request and returns the X-Nomad-Index
// response header (0 if absent).
func (c *client) doIndexed(ctx context.Context, timeout time.Duration, method string, parts []string, query url.Values, body, out any) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshaling nomad request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	target := c.urlFor(parts, query)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return 0, fmt.Errorf("building nomad request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("X-Nomad-Token", c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("nomad request: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return 0, fmt.Errorf("reading nomad response: %w", readErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("closing nomad response: %w", closeErr)
	}

	var idx uint64
	if raw := resp.Header.Get("X-Nomad-Index"); raw != "" {
		idx, _ = strconv.ParseUint(raw, 10, 64)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return idx, statusError(resp.StatusCode, target.String(), data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return idx, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return idx, fmt.Errorf("decoding nomad response: %w", err)
	}
	return idx, nil
}

// urlFor joins parts onto the API base path. It assigns to u.Path (the
// decoded form) rather than pre-escaping each part: dispatched child job
// IDs legitimately contain a literal "/" (e2a-child-job-naming, mirrored in
// fakenomad's own routing comment), and url.URL.String() escapes u.Path
// correctly on its own — escaping parts individually first would encode
// that literal "/" to "%2F" and then double-escape it when the URL is
// serialized, corrupting the path the server routes on.
func (c *client) urlFor(parts []string, query url.Values) *url.URL {
	u := *c.addr
	base := strings.TrimRight(u.Path, "/")
	u.Path = base + "/" + strings.Join(parts, "/")
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	return &u
}

func statusError(status int, target string, data []byte) error {
	msg := statusText(status, data)
	if status == http.StatusNotFound {
		return fmt.Errorf("%w: %s: %s", errJobGone, target, msg)
	}
	return fmt.Errorf("nomad %s: status %d: %s", target, status, msg)
}

func statusText(status int, data []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	if text := strings.TrimSpace(string(data)); text != "" {
		return text
	}
	return http.StatusText(status)
}
