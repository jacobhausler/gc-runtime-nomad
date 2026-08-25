package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 10 * time.Second
	blockingWait    = 5 * time.Second
	execTimeout     = 30 * time.Second
	maxResponseByte = 1 << 20
)

// errJobGone marks a 404 from the Nomad API against a job route — the
// confirmed-absence signal (04 §2.1 rule 7): a DEFAULT-consistency 404 on a
// direct job GET, or (for deregister/dispatch) the job never having existed.
var errJobGone = errors.New("nomad: job gone")

// errAllocFileGone marks a 404 from the client fs "cat" endpoint — either
// the allocation or the requested path is gone/never existed. The
// stop-path egress treats this as "nothing to copy" rather than a failure:
// not every allocation produces every file.
var errAllocFileGone = errors.New("nomad: alloc file gone")

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

// readAllocFile reads one file out of allocID's filesystem via Nomad's
// client fs "cat" endpoint (GET /v1/client/fs/cat/:allocID?path=...) — the
// stop-path egress read that must complete before a session's job is
// deregistered, since deregister is what makes the allocation's files
// unreachable. Returns errAllocFileGone on a 404 (alloc or path absent).
func (c *client) readAllocFile(ctx context.Context, allocID, path string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	q := url.Values{"path": []string{path}}
	target := c.urlFor([]string{"v1", "client", "fs", "cat", allocID}, q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building nomad fs-cat request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("X-Nomad-Token", c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nomad fs-cat request: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("reading nomad fs-cat response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing nomad fs-cat response: %w", closeErr)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errAllocFileGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nomad %s: status %d: %s", target.String(), resp.StatusCode, statusText(resp.StatusCode, data))
	}
	return data, nil
}

func blockingTimeout(wait time.Duration) time.Duration {
	if wait <= 0 {
		return defaultTimeout
	}
	return wait + defaultTimeout
}

// execFrame decodes the two alloc-exec WebSocket frame shapes this client
// reads: a stdout data chunk and the terminal exited/exit_code frame
// (e2a-exec-protocol).
type execFrame struct {
	Stdout *struct {
		Data string `json:"data"`
	} `json:"stdout,omitempty"`
	Exited bool `json:"exited,omitempty"`
	Result struct {
		ExitCode int `json:"exit_code"`
	} `json:"result,omitempty"`
}

// execAlloc runs command inside task of allocID over the Nomad alloc-exec
// WebSocket (GET /v1/client/allocation/:alloc_id/exec, e2a-exec-protocol)
// and returns the remote command's exit code plus its collected stdout. No
// stdin is sent — every caller (launch/relaunch's tmux command, driving-verb
// probes) needs none — so the stdin channel is closed immediately, which is
// enough to make both fakenomad and real Nomad run the command and report
// its result.
func (c *client) execAlloc(ctx context.Context, allocID, task string, command []string) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := c.dialExecWS(ctx, allocID, task, command)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(execTimeout))

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	closeFrame, err := json.Marshal(map[string]any{"stdin": map[string]any{"close": true}})
	if err != nil {
		return 0, nil, fmt.Errorf("encoding exec stdin-close frame: %w", err)
	}
	if err := wsWriteMaskedFrame(conn, wsOpText, closeFrame); err != nil {
		return 0, nil, fmt.Errorf("writing exec stdin-close frame: %w", err)
	}

	var stdout bytes.Buffer
	for {
		opcode, payload, err := wsReadFrame(conn)
		if err != nil {
			return 0, nil, fmt.Errorf("reading exec frame: %w", err)
		}
		if opcode != wsOpText {
			continue
		}
		var msg execFrame
		if err := json.Unmarshal(payload, &msg); err != nil {
			return 0, nil, fmt.Errorf("decoding exec frame: %w", err)
		}
		if msg.Stdout != nil && msg.Stdout.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(msg.Stdout.Data)
			if err != nil {
				return 0, nil, fmt.Errorf("decoding exec stdout: %w", err)
			}
			stdout.Write(decoded)
		}
		if msg.Exited {
			return msg.Result.ExitCode, stdout.Bytes(), nil
		}
	}
}

// wsConn is the subset of net.Conn the exec frame codec needs, satisfied by
// the raw connection returned from dialExecWS after its handshake.
type wsConn interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// bufferedConn wraps a net.Conn whose handshake was read through a
// *bufio.Reader, so any bytes the reader buffered past the handshake
// headers (frame data arriving in the same TCP segment) are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// dialExecWS dials allocID's exec endpoint and performs the client side of
// the RFC 6455 handshake, returning a connection positioned to read/write
// exec frames.
func (c *client) dialExecWS(ctx context.Context, allocID, task string, command []string) (wsConn, error) {
	cmdJSON, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("encoding exec command: %w", err)
	}
	q := url.Values{}
	q.Set("command", string(cmdJSON))
	q.Set("task", task)
	q.Set("tty", "false")

	host := c.addr.Host
	var d net.Dialer
	var conn net.Conn
	if c.addr.Scheme == "https" {
		conn, err = (&tls.Dialer{NetDialer: &d}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("dialing nomad exec websocket: %w", err)
	}

	keyBuf := make([]byte, 16)
	if _, err := rand.Read(keyBuf); err != nil {
		conn.Close()
		return nil, fmt.Errorf("generating websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBuf)

	base := strings.TrimRight(c.addr.Path, "/")
	path := base + "/v1/client/allocation/" + allocID + "/exec?" + q.Encode()

	var req bytes.Buffer
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	if c.token != "" {
		fmt.Fprintf(&req, "X-Nomad-Token: %s\r\n", c.token)
	}
	req.WriteString("\r\n")

	if _, err := conn.Write(req.Bytes()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing websocket handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading websocket handshake status: %w", err)
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		return nil, fmt.Errorf("nomad exec websocket handshake failed: %s", strings.TrimSpace(statusLine))
	}

	var acceptKey string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading websocket handshake headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Accept") {
			acceptKey = strings.TrimSpace(v)
		}
	}
	if acceptKey != wsAcceptKey(key) {
		conn.Close()
		return nil, fmt.Errorf("nomad exec websocket handshake: Sec-WebSocket-Accept mismatch")
	}

	return &bufferedConn{Conn: conn, r: br}, nil
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
