package launcher

import "testing"

func TestNormalizeDNSServer(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "8.8.8.8", want: "8.8.8.8:53"},
		{in: "8.8.8.8:5353", want: "8.8.8.8:5353"},
		{in: "2001:4860:4860::8888", want: "[2001:4860:4860::8888]:53"},
		{in: "[2001:4860:4860::8888]:53", want: "[2001:4860:4860::8888]:53"},
		{in: "dns.google:53", want: "dns.google:53"},
		{in: "", wantErr: true},
		{in: "::::", wantErr: true},
	}

	for _, tc := range cases {
		got, err := normalizeDNSServer(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("normalizeDNSServer(%q): expected error, got nil", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeDNSServer(%q): unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeDNSServer(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeDNSServers(t *testing.T) {
	got, warnings, err := normalizeDNSServers([]string{
		"8.8.8.8",
		"8.8.8.8:53",
		"[2001:4860:4860::8888]:53",
		"bad:::value",
	})
	if err != nil {
		t.Fatalf("normalizeDNSServers unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("normalizeDNSServers got len=%d, want 2", len(got))
	}
	if len(warnings) != 1 {
		t.Fatalf("normalizeDNSServers warnings len=%d, want 1", len(warnings))
	}
}

func TestNormalizeDNSServersAllInvalid(t *testing.T) {
	_, _, err := normalizeDNSServers([]string{"", "::::"})
	if err == nil {
		t.Fatalf("expected error when all dns servers are invalid")
	}
}
