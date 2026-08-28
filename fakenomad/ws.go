package fakenomad

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// This file implements just enough of RFC 6455 to fake the Nomad
// alloc-exec WebSocket (e2a-exec-protocol): a server-side handshake plus
// text-frame read/write, with no compression, fragmentation, or TLS
// support — deliberately out of scope per NRT-P1-02.

const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpText  = 0x1
	wsOpClose = 0x8
)

// handleExecWS fakes `GET /v1/client/allocation/:alloc_id/exec`. After a
// successful handshake it collects every stdin data frame the client sends
// (NRT-P1-06 staging: a tar-over-exec-stdin payload rides here) until it
// sees a stdin-close, then actually runs the command carried in the
// `command` query param (runCommand) with that collected stdin wired to the
// real subprocess, and replies with its real stdout+stderr and exit code —
// RPP-CONN-001's exit-code fidelity requires a genuine per-command result,
// not a canned reply.
func (s *Server) handleExecWS(w http.ResponseWriter, r *http.Request, allocID string) {
	s.mu.Lock()
	_, ok := s.allocs[allocID]
	s.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "alloc not found")
		return
	}

	var command []string
	if raw := r.URL.Query().Get("command"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &command)
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "missing Sec-WebSocket-Key")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "hijack unsupported")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	accept := wsAcceptKey(key)
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")

	var stdin bytes.Buffer
	for {
		opcode, payload, err := wsReadFrame(buf.Reader)
		if err != nil {
			return
		}
		if opcode == wsOpClose {
			_ = wsWriteFrame(conn, wsOpClose, nil)
			return
		}
		if opcode != wsOpText {
			continue
		}
		var frame struct {
			Stdin *struct {
				Data  string `json:"data"`
				Close bool   `json:"close"`
			} `json:"stdin"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			continue
		}
		if frame.Stdin == nil {
			continue
		}
		if frame.Stdin.Data != "" {
			if decoded, err := base64.StdEncoding.DecodeString(frame.Stdin.Data); err == nil {
				stdin.Write(decoded)
			}
			continue
		}
		if frame.Stdin.Close {
			if s.takeExecFailure() {
				return
			}
			exitCode, output := s.runCommand(allocID, command, stdin.Bytes())
			stdout, _ := json.Marshal(map[string]any{
				"stdout": map[string]string{"data": base64.StdEncoding.EncodeToString(output)},
			})
			if err := wsWriteFrame(conn, wsOpText, stdout); err != nil {
				return
			}
			exited, _ := json.Marshal(map[string]any{
				"exited": true,
				"result": map[string]int{"exit_code": exitCode},
			})
			_ = wsWriteFrame(conn, wsOpText, exited)
			return
		}
	}
}

func wsAcceptKey(clientKey string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, clientKey+wsMagicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsReadFrame reads one client (necessarily masked) frame. Fragmentation
// and payloads beyond 16-bit extended length are not supported — this is a
// test double, not a general client.
func wsReadFrame(r *bufio.Reader) (opcode byte, payload []byte, err error) {
	head := make([]byte, 2)
	if _, err = io.ReadFull(r, head); err != nil {
		return 0, nil, err
	}
	opcode = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)

	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, err
		}
	} else {
		return 0, nil, errors.New("fakenomad: client frame must be masked")
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}
	return opcode, payload, nil
}

// wsWriteFrame writes one unmasked server frame (server-to-client frames
// are never masked per RFC 6455 §5.1).
func wsWriteFrame(w io.Writer, opcode byte, payload []byte) error {
	var head []byte
	length := len(payload)
	switch {
	case length <= 125:
		head = []byte{0x80 | opcode, byte(length)}
	case length <= 0xffff:
		head = make([]byte, 4)
		head[0] = 0x80 | opcode
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:], uint16(length))
	default:
		head = make([]byte, 10)
		head[0] = 0x80 | opcode
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:], uint64(length))
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}
