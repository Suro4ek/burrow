package client

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestExplainHandshake covers the diagnostics for the two ways of pointing an
// agent at the wrong endpoint. Both surface as a bare "EOF" from the
// transport, which tells a user nothing.
func TestExplainHandshake(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		in      error
		want    string
		wantRaw bool // error must pass through untouched
	}{
		{
			name: "plaintext agent against a TLS control port",
			cfg:  Config{TLS: false, ServerAddr: "tun.example.com:7000"},
			in:   io.EOF,
			want: "without -no-tls",
		},
		{
			name: "TLS agent aimed at the public HTTPS port",
			cfg:  Config{TLS: true, ServerAddr: "tun.example.com:443"},
			in:   io.EOF,
			want: "control port",
		},
		{
			name: "truncated handshake is treated the same",
			cfg:  Config{TLS: true, ServerAddr: "tun.example.com:443"},
			in:   io.ErrUnexpectedEOF,
			want: "control port",
		},
		{
			name:    "unrelated errors are left alone",
			cfg:     Config{TLS: true, ServerAddr: "tun.example.com:7000"},
			in:      errors.New("connection refused"),
			wantRaw: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{cfg: tc.cfg}
			got := c.explainHandshake(tc.in)

			if tc.wantRaw {
				if got.Error() != tc.in.Error() {
					t.Errorf("error was rewritten: %v", got)
				}
				return
			}
			if !strings.Contains(got.Error(), tc.want) {
				t.Errorf("message %q does not mention %q", got, tc.want)
			}
			// The cause must survive so callers can still match on it.
			if !errors.Is(got, tc.in) {
				t.Errorf("original error was not wrapped: %v", got)
			}
		})
	}
}
