// Package proto defines the wire protocol between the tunnel agent and the
// tunnel server.
//
// Every message is a length-prefixed JSON envelope:
//
//	[4 bytes big-endian length][JSON envelope]
//
// Length prefixing (rather than a json.Decoder) matters on data streams: a
// Decoder buffers ahead and would swallow the first bytes of the tunnelled
// payload that follows the header.
//
// Two kinds of streams run inside a single yamux session:
//
//   - the control stream, opened by the agent right after connecting, carries
//     the handshake and tunnel registrations and stays open for the lifetime of
//     the session;
//   - data streams, opened by the server, one per incoming user connection.
//     The server writes a StreamOpen header, the agent answers with a
//     StreamAck, and everything after that is raw tunnelled bytes.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Version is the protocol version. The server rejects agents that do not match.
const Version = 1

// maxFrame caps a single control frame. Data payloads are not framed, so this
// only ever bounds handshake-sized messages.
const maxFrame = 1 << 20

// Type identifies an envelope's payload.
type Type string

const (
	TypeHello      Type = "hello"
	TypeHelloResp  Type = "hello_resp"
	TypeTunnelReq  Type = "tunnel_req"
	TypeTunnelResp Type = "tunnel_resp"
	TypeShutdown   Type = "shutdown"

	TypeStreamOpen Type = "stream_open"
	TypeStreamAck  Type = "stream_ack"
)

// Proto values accepted in TunnelReq.Proto.
const (
	ProtoHTTP = "http"
	ProtoTCP  = "tcp"
)

// Envelope wraps every message on the wire.
type Envelope struct {
	Type Type            `json:"t"`
	Data json.RawMessage `json:"d,omitempty"`
}

// Hello is the first message on the control stream.
type Hello struct {
	Version  int    `json:"version"`
	Token    string `json:"token"`
	Agent    string `json:"agent"`
	Hostname string `json:"hostname"`
}

// HelloResp answers Hello. A rejected handshake carries Error and closes.
type HelloResp struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Server     string `json:"server,omitempty"`
	BaseDomain string `json:"base_domain,omitempty"`
}

// TunnelReq asks the server to expose one local address.
//
// Subdomain and RemotePort are wishes, not demands: leave them empty and the
// server allocates. The server may refuse a wish that is taken or reserved by
// another token.
type TunnelReq struct {
	Proto      string `json:"proto"`
	Subdomain  string `json:"subdomain,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	LocalAddr  string `json:"local_addr"`
}

// TunnelResp answers TunnelReq with the public address that was allocated.
type TunnelResp struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	ID         string `json:"id,omitempty"`
	Proto      string `json:"proto,omitempty"`
	URL        string `json:"url,omitempty"`
	RemoteHost string `json:"remote_host,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// Shutdown tells the agent why the server is dropping it.
type Shutdown struct {
	Reason string `json:"reason,omitempty"`
}

// StreamOpen is the header the server writes on every new data stream.
type StreamOpen struct {
	TunnelID   string `json:"tunnel_id"`
	RemoteAddr string `json:"remote_addr,omitempty"`
}

// StreamAck reports whether the agent reached the local service. A failed ack
// lets the server render a useful error instead of a dead connection.
type StreamAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Write marshals v as the payload of a typed envelope and writes one frame.
func Write(w io.Writer, t Type, v any) error {
	var raw json.RawMessage
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("proto: marshal %s payload: %w", t, err)
		}
		raw = b
	}
	body, err := json.Marshal(Envelope{Type: t, Data: raw})
	if err != nil {
		return fmt.Errorf("proto: marshal %s envelope: %w", t, err)
	}
	if len(body) > maxFrame {
		return fmt.Errorf("proto: %s frame too large (%d bytes)", t, len(body))
	}
	// One Write call so a frame never straddles two yamux windows needlessly.
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("proto: write %s: %w", t, err)
	}
	return nil
}

// Read reads exactly one frame.
func Read(r io.Reader) (*Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrame {
		return nil, fmt.Errorf("proto: bad frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var e Envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("proto: unmarshal envelope: %w", err)
	}
	return &e, nil
}

// ReadAs reads one frame, asserts its type and unmarshals the payload into v.
func ReadAs(r io.Reader, want Type, v any) error {
	e, err := Read(r)
	if err != nil {
		return err
	}
	if e.Type != want {
		if e.Type == TypeShutdown {
			var sd Shutdown
			_ = json.Unmarshal(e.Data, &sd)
			return fmt.Errorf("proto: server shut down the session: %s", sd.Reason)
		}
		return fmt.Errorf("proto: got %q, want %q", e.Type, want)
	}
	if v == nil {
		return nil
	}
	if len(e.Data) == 0 {
		return fmt.Errorf("proto: %s has empty payload", want)
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("proto: unmarshal %s payload: %w", want, err)
	}
	return nil
}
