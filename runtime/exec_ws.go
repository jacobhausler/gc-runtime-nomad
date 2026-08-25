package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
)

// This file implements just enough of RFC 6455 to speak the client side of
// the Nomad alloc-exec WebSocket (e2a-exec-protocol) — a handshake plus
// masked text-frame writes and unmasked text-frame reads, with no
// compression, fragmentation, or payloads beyond 16-bit extended length.
// It mirrors fakenomad/ws.go's server-side implementation with client and
// server roles reversed (RFC 6455 §5.1: client frames must be masked,
// server frames must not be).

const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpText  = 0x1
	wsOpClose = 0x8
)

// wsAcceptKey computes the Sec-WebSocket-Accept value a compliant server
// must return for clientKey, so the handshake response can be verified.
func wsAcceptKey(clientKey string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, clientKey+wsMagicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsWriteMaskedFrame writes one client-to-server frame.
func wsWriteMaskedFrame(w io.Writer, opcode byte, payload []byte) error {
	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}

	length := len(payload)
	var head []byte
	switch {
	case length <= 125:
		head = []byte{0x80 | opcode, 0x80 | byte(length)}
	case length <= 0xffff:
		head = make([]byte, 4)
		head[0] = 0x80 | opcode
		head[1] = 0x80 | 126
		binary.BigEndian.PutUint16(head[2:], uint16(length))
	default:
		head = make([]byte, 10)
		head[0] = 0x80 | opcode
		head[1] = 0x80 | 127
		binary.BigEndian.PutUint64(head[2:], uint64(length))
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	if _, err := w.Write(maskKey[:]); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// wsReadFrame reads one server (necessarily unmasked) frame.
func wsReadFrame(r io.Reader) (opcode byte, payload []byte, err error) {
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
	if masked {
		return 0, nil, errors.New("nomad exec websocket: unexpected masked server frame")
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}
