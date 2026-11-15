package httputil

import (
	"net/url"
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
		{"http://example.com:80/a", "http://EXAMPLE.COM/b", true},
		{"https://[::1]:443/api", "https://[::1]/other", true},
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
			raw:      "hub.example.com/?view=prs#inbox",
			subpath:  "/api/triage/pending",
			wantBase: "https://hub.example.com",
			wantFull: "https://hub.example.com/api/triage/pending",
		},
		{
			raw:      "127.0.0.1:8787?",
			subpath:  "/api/admin/backup.db",
			wantBase: "http://127.0.0.1:8787",
			wantFull: "http://127.0.0.1:8787/api/admin/backup.db",
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

	for _, bad := range []string{"", "ftp://hub.example.com", "http://example.com:"} {
		if _, err := ParseBaseURL(bad); err == nil {
			t.Fatalf("expected ParseBaseURL(%q) to fail", bad)
		}
	}
}
