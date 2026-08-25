package fakenomad

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// httpJSON performs one request and decodes a JSON response, returning the
// status code, the X-Nomad-Index header (0 if absent), and the decoded body.
func httpJSON(t *testing.T, method, url string, body any, out any) (status int, index uint64) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if raw := resp.Header.Get("X-Nomad-Index"); raw != "" {
		index, _ = strconv.ParseUint(raw, 10, 64)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp.StatusCode, index
}

// TestEndpointFamilies drives every endpoint family the provider uses:
// jobs (register), dispatch, job-read, allocations, and system. Each case
// registers a fresh job so cases stay independent.
func TestEndpointFamilies(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	cases := []struct {
		name string
		run  func(t *testing.T, jobID string)
	}{
		{
			name: "jobs: register",
			run: func(t *testing.T, jobID string) {
				var out map[string]any
				status, idx := httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs",
					map[string]any{"Job": map[string]any{"ID": jobID}}, &out)
				if status != http.StatusOK {
					t.Fatalf("register: status = %d, want 200", status)
				}
				if idx == 0 {
					if v, ok := out["Index"].(float64); !ok || v == 0 {
						t.Fatalf("register: expected a nonzero index, got %v", out["Index"])
					}
				}
			},
		},
		{
			name: "dispatch",
			run: func(t *testing.T, jobID string) {
				var out map[string]any
				status, _ := httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/"+jobID+"/dispatch", map[string]any{}, &out)
				if status != http.StatusOK {
					t.Fatalf("dispatch: status = %d, want 200", status)
				}
				childID, _ := out["DispatchedJobID"].(string)
				if !strings.HasPrefix(childID, jobID+"/dispatch-") {
					t.Fatalf("dispatch: DispatchedJobID = %q, want prefix %q", childID, jobID+"/dispatch-")
				}
			},
		},
		{
			name: "job-read",
			run: func(t *testing.T, jobID string) {
				var out map[string]any
				status, idx := httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+jobID, nil, &out)
				if status != http.StatusOK {
					t.Fatalf("job read: status = %d, want 200", status)
				}
				if idx == 0 {
					t.Fatalf("job read: X-Nomad-Index missing or zero")
				}
				if out["ID"] != jobID {
					t.Fatalf("job read: ID = %v, want %q", out["ID"], jobID)
				}
			},
		},
		{
			name: "allocations: list-for-job",
			run: func(t *testing.T, jobID string) {
				var out []map[string]any
				httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/"+jobID+"/dispatch", map[string]any{}, &map[string]any{})
				status, _ := httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+jobID+"/allocations", nil, &out)
				if status != http.StatusOK {
					t.Fatalf("list allocations: status = %d, want 200", status)
				}
			},
		},
		{
			name: "system: gc",
			run: func(t *testing.T, jobID string) {
				status, _ := httpJSON(t, http.MethodPut, srv.URL()+"/v1/system/gc", nil, nil)
				if status != http.StatusOK {
					t.Fatalf("system gc: status = %d, want 200", status)
				}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobID := fmt.Sprintf("job-%d", i)
			var out map[string]any
			httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": jobID}}, &out)
			tc.run(t, jobID)
		})
	}
}

// TestBlockingQueryResolvedByIndexAdvance drives a blocking read on an
// allocation and confirms it unblocks once the index advances, rather than
// waiting out the full timeout.
func TestBlockingQueryResolvedByIndexAdvance(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	var dispatchOut map[string]any
	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "blocker"}}, &map[string]any{})
	status, _ := httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/blocker/dispatch", map[string]any{}, &dispatchOut)
	if status != http.StatusOK {
		t.Fatalf("dispatch: status = %d, want 200", status)
	}
	childID, _ := dispatchOut["DispatchedJobID"].(string)

	var allocs []map[string]any
	_, startIdx := httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+childID+"/allocations", nil, &allocs)
	if len(allocs) != 1 {
		t.Fatalf("expected exactly one allocation, got %d", len(allocs))
	}
	allocID, _ := allocs[0]["ID"].(string)

	type result struct {
		status int
		index  uint64
		body   map[string]any
	}
	resultCh := make(chan result, 1)
	go func() {
		var out map[string]any
		status, idx := httpJSON(t, http.MethodGet,
			fmt.Sprintf("%s/v1/allocation/%s?index=%d&wait=10s", srv.URL(), allocID, startIdx), nil, &out)
		resultCh <- result{status: status, index: idx, body: out}
	}()

	// Give the blocking request time to register with the server before
	// advancing the index; if this races and the read observes the new
	// index immediately, the test still passes (it returns the updated
	// status either way) — the real assertion is that it returns well
	// before the 10s wait would otherwise elapse.
	time.Sleep(50 * time.Millisecond)
	if !srv.SetAllocStatus(allocID, "running") {
		t.Fatalf("SetAllocStatus: alloc %q not found", allocID)
	}

	select {
	case r := <-resultCh:
		if r.status != http.StatusOK {
			t.Fatalf("blocking read: status = %d, want 200", r.status)
		}
		if r.body["ClientStatus"] != "running" {
			t.Fatalf("blocking read: ClientStatus = %v, want %q", r.body["ClientStatus"], "running")
		}
		if r.index <= startIdx {
			t.Fatalf("blocking read: index = %d, want > start index %d", r.index, startIdx)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocking read did not unblock within 2s of the index advance")
	}
}

// TestScriptedFault drives one scripted 5xx fault: the first matching
// request fails as scripted, and the very next identical request succeeds
// normally (FailNext is one-shot).
func TestScriptedFault(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "faulty"}}, &map[string]any{})

	srv.FailNext(http.MethodPost, "/v1/job/faulty/dispatch", http.StatusInternalServerError, `{"error":"injected"}`)

	var errOut map[string]any
	status, _ := httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/faulty/dispatch", map[string]any{}, &errOut)
	if status != http.StatusInternalServerError {
		t.Fatalf("faulted dispatch: status = %d, want 500", status)
	}
	if errOut["error"] != "injected" {
		t.Fatalf("faulted dispatch: body = %v, want error=injected", errOut)
	}

	var okOut map[string]any
	status, _ = httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/faulty/dispatch", map[string]any{}, &okOut)
	if status != http.StatusOK {
		t.Fatalf("retry dispatch: status = %d, want 200 (fault should be one-shot)", status)
	}
}

// TestDeregisterWithoutPurge drives the stop-without-purge contract a
// provider's stop op relies on: the job record survives (still 200 on
// read), but every non-terminal allocation is driven to a terminal
// ClientStatus so a confirm-terminal blocking read resolves.
func TestDeregisterWithoutPurge(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "stopme"}}, &map[string]any{})
	var dispatchOut map[string]any
	httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/stopme/dispatch", map[string]any{}, &dispatchOut)
	childID, _ := dispatchOut["DispatchedJobID"].(string)

	var allocsBefore []map[string]any
	httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+childID+"/allocations", nil, &allocsBefore)
	if len(allocsBefore) != 1 || allocsBefore[0]["ClientStatus"] != "pending" {
		t.Fatalf("allocs before deregister = %v, want one pending alloc", allocsBefore)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL()+"/v1/job/"+childID, nil)
	if err != nil {
		t.Fatalf("build deregister request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deregister: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deregister: status = %d, want 200", resp.StatusCode)
	}

	status, _ := httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+childID, nil, &map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("job read after non-purge deregister: status = %d, want 200 (job should survive)", status)
	}

	var allocsAfter []map[string]any
	httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+childID+"/allocations", nil, &allocsAfter)
	if len(allocsAfter) != 1 || allocsAfter[0]["ClientStatus"] != "complete" {
		t.Fatalf("allocs after deregister = %v, want one complete alloc", allocsAfter)
	}

	// Deregistering an unknown job 404s (the idempotency contract lives in
	// the provider, which treats 404 as already-gone).
	req2, _ := http.NewRequest(http.MethodDelete, srv.URL()+"/v1/job/does-not-exist", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("deregister unknown job: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("deregister unknown job: status = %d, want 404", resp2.StatusCode)
	}
}

// TestDeregisterWithPurge confirms ?purge=true removes the job record
// outright, so a subsequent read 404s (confirmed absence, per the wire rule
// that only a 200-children-list-lacking-the-entry or a direct 404 counts).
func TestDeregisterWithPurge(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "purgeme"}}, &map[string]any{})

	req, err := http.NewRequest(http.MethodDelete, srv.URL()+"/v1/job/purgeme?purge=true", nil)
	if err != nil {
		t.Fatalf("build deregister request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deregister: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deregister: status = %d, want 200", resp.StatusCode)
	}

	status, _ := httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/purgeme", nil, &map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("job read after purge: status = %d, want 404", status)
	}
}

// TestAllocExecWS drives the alloc-exec-WS endpoint family: connect, send a
// stdin-close frame, and expect one stdout frame followed by an exited
// frame with exit_code 0.
func TestAllocExecWS(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "execjob"}}, &map[string]any{})
	var dispatchOut map[string]any
	httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/execjob/dispatch", map[string]any{}, &dispatchOut)
	childID, _ := dispatchOut["DispatchedJobID"].(string)
	var allocs []map[string]any
	httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+childID+"/allocations", nil, &allocs)
	if len(allocs) != 1 {
		t.Fatalf("expected exactly one allocation, got %d", len(allocs))
	}
	allocID, _ := allocs[0]["ID"].(string)

	host := strings.TrimPrefix(srv.URL(), "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	path := fmt.Sprintf("/v1/client/allocation/%s/exec?command=%%5B%%22echo%%22%%5D&task=main", allocID)
	clientKey := "dGhlIHNhbXBsZSBub25jZQ=="
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + clientKey + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("handshake status line = %q, want 101", statusLine)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	closeFrame, _ := json.Marshal(map[string]any{"stdin": map[string]any{"close": true}})
	if err := writeMaskedFrame(conn, wsOpText, closeFrame); err != nil {
		t.Fatalf("write stdin-close frame: %v", err)
	}

	stdoutFrame := readServerFrame(t, br)
	var stdoutMsg struct {
		Stdout struct {
			Data string `json:"data"`
		} `json:"stdout"`
	}
	if err := json.Unmarshal(stdoutFrame, &stdoutMsg); err != nil {
		t.Fatalf("unmarshal stdout frame: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(stdoutMsg.Stdout.Data)
	if err != nil || len(decoded) == 0 {
		t.Fatalf("stdout frame data = %q, decode err = %v", stdoutMsg.Stdout.Data, err)
	}

	exitFrame := readServerFrame(t, br)
	var exitMsg struct {
		Exited bool `json:"exited"`
		Result struct {
			ExitCode int `json:"exit_code"`
		} `json:"result"`
	}
	if err := json.Unmarshal(exitFrame, &exitMsg); err != nil {
		t.Fatalf("unmarshal exit frame: %v", err)
	}
	if !exitMsg.Exited || exitMsg.Result.ExitCode != 0 {
		t.Fatalf("exit frame = %+v, want exited=true exit_code=0", exitMsg)
	}
}

// TestClientFSCat drives the client fs "cat" endpoint a stop-path egress
// reads: a freshly dispatched allocation has both seeded files readable,
// an unknown path 404s, and an unknown allocation 404s.
func TestClientFSCat(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "fs-job"}}, &map[string]any{})
	var dispatchOut map[string]any
	httpJSON(t, http.MethodPost, srv.URL()+"/v1/job/fs-job/dispatch", map[string]any{}, &dispatchOut)
	childID, _ := dispatchOut["DispatchedJobID"].(string)

	var allocs []map[string]any
	httpJSON(t, http.MethodGet, srv.URL()+"/v1/job/"+childID+"/allocations", nil, &allocs)
	if len(allocs) != 1 {
		t.Fatalf("expected exactly one allocation, got %d", len(allocs))
	}
	allocID, _ := allocs[0]["ID"].(string)

	for _, path := range []string{"alloc/logs/transcript.log", "alloc/data/evidence.json"} {
		resp, err := http.Get(fmt.Sprintf("%s/v1/client/fs/cat/%s?path=%s", srv.URL(), allocID, path))
		if err != nil {
			t.Fatalf("cat %q: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cat %q: status = %d, want 200 (body %q)", path, resp.StatusCode, body)
		}
		if len(body) == 0 {
			t.Fatalf("cat %q: empty body, want seeded content", path)
		}
	}

	resp, err := http.Get(fmt.Sprintf("%s/v1/client/fs/cat/%s?path=nope", srv.URL(), allocID))
	if err != nil {
		t.Fatalf("cat unknown path: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cat unknown path: status = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL() + "/v1/client/fs/cat/no-such-alloc?path=alloc/logs/transcript.log")
	if err != nil {
		t.Fatalf("cat unknown alloc: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cat unknown alloc: status = %d, want 404", resp.StatusCode)
	}
}

// TestTraceRecordsRequestOrder confirms Trace() reflects requests in
// arrival order — the ordering guarantee a stop-path egress test relies on
// to assert fs reads precede deregister.
func TestTraceRecordsRequestOrder(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	httpJSON(t, http.MethodPost, srv.URL()+"/v1/jobs", map[string]any{"Job": map[string]any{"ID": "trace-job"}}, &map[string]any{})
	httpJSON(t, http.MethodPut, srv.URL()+"/v1/system/gc", nil, nil)

	trace := srv.Trace()
	want := []string{"POST /v1/jobs", "PUT /v1/system/gc"}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i, w := range want {
		if trace[i] != w {
			t.Fatalf("trace[%d] = %q, want %q (full trace %v)", i, trace[i], w, trace)
		}
	}
}

// writeMaskedFrame writes one client-to-server text frame; RFC 6455
// requires client frames to be masked.
func writeMaskedFrame(w io.Writer, opcode byte, payload []byte) error {
	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}
	head := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	if _, err := w.Write(head); err != nil {
		return err
	}
	if _, err := w.Write(maskKey[:]); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// readServerFrame reads one unmasked server frame (small payloads only,
// matching what this fake ever sends) and returns its payload.
func readServerFrame(t *testing.T, r *bufio.Reader) []byte {
	t.Helper()
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	length := int(head[1] & 0x7f)
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return payload
}
