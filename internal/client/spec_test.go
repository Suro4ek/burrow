package client

import "testing"

func TestParseLocalAddr(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "3000", want: "127.0.0.1:3000"},
		{in: ":8080", want: "127.0.0.1:8080"},
		{in: "localhost:8080", want: "localhost:8080"},
		{in: "10.0.0.5:5432", want: "10.0.0.5:5432"},
		{in: "", wantErr: true},
		{in: "0", wantErr: true},
		{in: "70000", wantErr: true},
		{in: "localhost", wantErr: true},
		{in: "localhost:zero", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseLocalAddr(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseLocalAddr(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLocalAddr(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLocalAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSpec(t *testing.T) {
	tests := []struct {
		in      string
		want    TunnelSpec
		wantErr bool
	}{
		{in: "http:3000", want: TunnelSpec{Proto: "http", LocalAddr: "127.0.0.1:3000"}},
		{in: "http:3000:myapp", want: TunnelSpec{Proto: "http", LocalAddr: "127.0.0.1:3000", Subdomain: "myapp"}},
		{in: "http:localhost:8080", want: TunnelSpec{Proto: "http", LocalAddr: "localhost:8080"}},
		{in: "http:10.0.0.5:8080:myapp", want: TunnelSpec{Proto: "http", LocalAddr: "10.0.0.5:8080", Subdomain: "myapp"}},
		{in: "HTTP:3000:MyApp", want: TunnelSpec{Proto: "http", LocalAddr: "127.0.0.1:3000", Subdomain: "myapp"}},
		{in: "tcp:22", want: TunnelSpec{Proto: "tcp", LocalAddr: "127.0.0.1:22"}},
		{in: "tcp:22:25343", want: TunnelSpec{Proto: "tcp", LocalAddr: "127.0.0.1:22", RemotePort: 25343}},
		{in: "tcp:10.0.0.5:5432", want: TunnelSpec{Proto: "tcp", LocalAddr: "10.0.0.5:5432"}},
		{in: "tcp:10.0.0.5:5432:25343", want: TunnelSpec{Proto: "tcp", LocalAddr: "10.0.0.5:5432", RemotePort: 25343}},
		{in: "3000", wantErr: true},
		{in: "udp:53", wantErr: true},
		{in: "tcp:22:notaport", wantErr: true},
		{in: "http:a:b:c:d", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseSpec(%q) = %+v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSpec(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSpec(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
