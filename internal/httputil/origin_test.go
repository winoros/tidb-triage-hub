package httputil

import (
	"net/url"
	"strings"
	"testing"
)

func TestSameOriginAndNormalizeOriginHost(t *testing.T) {
	mustURL := func(s string) *url.URL {
		t.Helper()
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return u
	}

	cases := []struct {
		a, b string
		want bool
	}{
		{"https://api.github.com/graphql", "https://api.github.com:443/v2", true},
		{"https://api.github.com:0443/graphql", "https://api.github.com/v2", true},
		{"http://example.com:80/a", "http://EXAMPLE.COM/b", true},
		{"http://example.com:0080/a", "http://example.com/b", true},
		{"https://[::1]:443/api", "https://[0:0::1]/other", true},
		{"https://hub.example.com/api", "https://hub.example.com:8443/api", false},
		{"http://127.0.0.1:8787/api", "http://127.0.0.1:9999/api", false},
		{"https://hub.example.com/api", "http://hub.example.com/api", false},
		{"https://hub.example.com/api", "https://evil.example.com/api", false},
	}
	for _, tc := range cases {
		if got := SameOrigin(mustURL(tc.a), mustURL(tc.b)); got != tc.want {
			t.Fatalf("SameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}

	badHosts := []string{
		"",
		"::1",
		"2001:db8::1",
		"example.com:",
		"example.com:0",
		"example.com:70000",
		"example.com:https",
		"example.com:+80",
		"[127.0.0.1]:8080",
		"evil.com/path",
		"user@evil.com",
	}
	for _, bh := range badHosts {
		if got := NormalizeOriginHost(bh, "https"); got != "" {
			t.Fatalf("expected NormalizeOriginHost(%q) to return empty, got %q", bh, got)
		}
	}

	if !IsLoopbackHost("localhost:8787") || !IsLoopbackHost("127.0.0.1:8787") || !IsLoopbackHost("[::1]:8787") {
		t.Fatal("expected localhost, 127.0.0.1, and [::1] to be recognized as loopback")
	}
	if IsLoopbackHost("10.0.0.8:8787") || IsLoopbackHost("hub.internal:8787") || IsLoopbackHost("::1") {
		t.Fatal("expected LAN IP, hostname, and unbracketed IPv6 not to pass IsLoopbackHost")
	}
}

func TestParseBaseURLAndJoinEndpoint(t *testing.T) {
	cases := []struct {
		raw      string
		subpath  string
		qKey     string
		qVal     string
		wantBase string
		wantFull string
	}{
		{
			raw:      "https://hub.example.com?",
			subpath:  "/api/status",
			qKey:     "require_auth",
			qVal:     "1",
			wantBase: "https://hub.example.com",
			wantFull: "https://hub.example.com/api/status?require_auth=1",
		},
		{
			raw:      "https://hub.example.com:443/?view=prs#inbox",
			subpath:  "/api/triage/pending",
			wantBase: "https://hub.example.com",
			wantFull: "https://hub.example.com/api/triage/pending",
		},
		{
			raw:      "http://127.0.0.1:8787?",
			subpath:  "/api/admin/backup.db",
			wantBase: "http://127.0.0.1:8787",
			wantFull: "http://127.0.0.1:8787/api/admin/backup.db",
		},
		{
			raw:      "http://[::1]:8787/",
			subpath:  "/api/status",
			wantBase: "http://[::1]:8787",
			wantFull: "http://[::1]:8787/api/status",
		},
		{
			raw:      "HTTPS://hub.example.com/triage/?#",
			subpath:  "api/status",
			wantBase: "https://hub.example.com/triage",
			wantFull: "https://hub.example.com/triage/api/status",
		},
	}
	for _, tc := range cases {
		u, err := ParseBaseURL(tc.raw)
		if err != nil {
			t.Fatalf("ParseBaseURL(%q) unexpected error: %v", tc.raw, err)
		}
		if u.String() != tc.wantBase {
			t.Fatalf("ParseBaseURL(%q) = %q, want %q", tc.raw, u.String(), tc.wantBase)
		}
		var q url.Values
		if tc.qKey != "" {
			q = url.Values{tc.qKey: []string{tc.qVal}}
		}
		got := JoinEndpoint(u, tc.subpath, q)
		if got != tc.wantFull {
			t.Fatalf("JoinEndpoint(%q, %q) = %q, want %q", tc.raw, tc.subpath, got, tc.wantFull)
		}
	}

	for _, bad := range []string{
		"",
		"hub.example.com",
		"127.0.0.1:8787",
		"::1",
		"ftp://hub.example.com",
		"http://example.com:",
		"http://example.com:99999",
		"https://user:pass@hub.example.com",
	} {
		if _, err := ParseBaseURL(bad); err == nil {
			t.Fatalf("expected ParseBaseURL(%q) to fail", bad)
		}
	}
}

func TestLastForwardedHopTrustedProxyAddrAndReadBoundedBody(t *testing.T) {
	// Multi-line header: client spoofed first line, reverse proxy appended last line
	if got := LastForwardedHop([]string{"https", "http"}); got != "http" {
		t.Fatalf("expected last header line hop 'http', got %q", got)
	}
	if got := LastForwardedHop([]string{"evil.example.com", "edge.internal, hub.example.com"}); got != "hub.example.com" {
		t.Fatalf("expected 'hub.example.com', got %q", got)
	}
	if got := LastForwardedHop([]string{"https", "http,"}); got != "" {
		t.Fatalf("expected malformed trailing comma to fail closed (''), got %q", got)
	}

	if !IsTrustedProxyPeerAddr("127.0.0.1:54321", nil) ||
		!IsTrustedProxyPeerAddr("[::1]:54321", nil) ||
		!IsTrustedProxyPeerAddr("10.24.0.5:41234", nil) ||
		!IsTrustedProxyPeerAddr("[fd00::1234]:8080", nil) {
		t.Fatal("expected loopback and private proxy peers trusted by default")
	}
	if IsTrustedProxyPeerAddr("203.0.113.9:54321", nil) {
		t.Fatal("expected public IP peer NOT trusted by default")
	}
	custom, err := ParseCIDRList("203.0.113.0/24, 198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if !IsTrustedProxyPeerAddr("203.0.113.9:54321", custom) || !IsTrustedProxyPeerAddr("198.51.100.7:1234", custom) {
		t.Fatal("expected custom CIDR/IP to match")
	}
	if IsTrustedProxyPeerAddr("10.0.0.1:54321", custom) {
		t.Fatal("expected non-listed private IP rejected when custom CIDRs are set")
	}

	okBytes, err := ReadBoundedBody(strings.NewReader("hello"), 5)
	if err != nil || string(okBytes) != "hello" {
		t.Fatalf("expected exact 5-byte read to succeed, got %q err=%v", okBytes, err)
	}
	if _, err := ReadBoundedBody(strings.NewReader("hello!"), 5); err == nil {
		t.Fatal("expected 6-byte body over 5-byte limit to return error")
	}
}
