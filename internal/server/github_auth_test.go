package server

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type oauthTransport func(*http.Request) (*http.Response, error)

func (f oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func oauthServer() *Server {
	return New(nil, nil, Options{AuthToken: "cli-secret", GitHub: GitHubOptions{
		ClientID: "client", ClientSecret: "secret", CallbackURL: "https://hub.example/auth/github/callback", OwnerID: 7846227}})
}
func githubLogin(t *testing.T, s *Server, userID int64) *http.Cookie {
	t.Helper()
	start := httptest.NewRecorder()
	s.Handler().ServeHTTP(start, httptest.NewRequest("GET", "https://hub.example/auth/github", nil))
	if start.Code != 302 {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	u, _ := url.Parse(start.Header().Get("Location"))
	state := u.Query().Get("state")
	if state == "" || u.Query().Get("scope") != "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing OAuth protections")
	}
	cookie := start.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe cookie")
	}
	s.github.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(hash[:]) != u.Query().Get("code_challenge") {
				t.Fatal("PKCE mismatch")
			}
			if r.Form.Get("client_secret") != "secret" || r.Form.Get("redirect_uri") != s.opts.GitHub.CallbackURL {
				t.Fatal("invalid exchange")
			}
			body = `{"access_token":"github-token"}`
		case "https://api.github.com/user":
			if r.Header.Get("Authorization") != "Bearer github-token" {
				t.Fatal("missing identity token")
			}
			body = fmt.Sprintf(`{"id":%d,"login":"winoros"}`, userID)
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	req := httptest.NewRequest("GET", "https://hub.example/auth/github/callback?state="+state+"&code=code", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 303 || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	replay := httptest.NewRecorder()
	s.Handler().ServeHTTP(replay, req)
	if replay.Code != 400 {
		t.Fatal("OAuth state replay accepted")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == githubCookie {
			return c
		}
	}
	t.Fatal("missing session")
	return nil
}

func TestGitHubIdentityPermissionsAndLogout(t *testing.T) {
	for _, owner := range []bool{true, false} {
		t.Run(fmt.Sprint(owner), func(t *testing.T) {
			s := oauthServer()
			id := int64(123)
			if owner {
				id = 7846227
			}
			c := githubLogin(t, s, id)
			req := httptest.NewRequest("GET", "/api/session", nil)
			req.AddCookie(c)
			sess := s.resolveSession(req)
			if !sess.Authenticated || sess.Permissions.CanTriage != owner || sess.Permissions.CanSync != owner {
				t.Fatalf("incorrect permissions: %+v", sess)
			}
			s.opts.ReadOnly = true
			if s.resolveSession(req).Permissions.CanTriage {
				t.Fatal("readonly bypass")
			}
			s.opts.ReadOnly = false
			logout := httptest.NewRequest("POST", "/api/session/logout", strings.NewReader("{}"))
			logout.Header.Set("Content-Type", "application/json")
			logout.AddCookie(c)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, logout)
			if rec.Code != 200 || s.resolveSession(req).Authenticated {
				t.Fatal("logout did not revoke session")
			}
		})
	}
}
func TestGitHubStateValidationAndExpiry(t *testing.T) {
	s := oauthServer()
	s.github.pending["state"] = oauthEntry{time.Now().Add(-time.Second), "verifier"}
	for _, cookie := range []string{"", "different", "state"} {
		req := httptest.NewRequest("GET", "/auth/github/callback?state=state&code=code", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: oauthCookie, Value: cookie})
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("accepted invalid state: %s", cookie)
		}
	}
	s.github.sessions["expired"] = githubSession{time.Now().Add(-time.Second), githubIdentity{7846227, "winoros"}}
	req := httptest.NewRequest("GET", "/api/session", nil)
	req.AddCookie(&http.Cookie{Name: githubCookie, Value: "expired"})
	if s.resolveSession(req).Authenticated {
		t.Fatal("accepted expired session")
	}
}
func TestAllMutationsDenyAnonymousAndOtherGitHubUsers(t *testing.T) {
	routes := []string{
		"POST /api/triage/decisions", "PATCH /api/issues/1/triage", "DELETE /api/issues/1/triage",
		"POST /api/issues/1/retriage/ack", "POST /api/retriage/ack-batch", "POST /api/issues/1/exclude",
		"DELETE /api/excluded/1", "POST /api/pending/1/resolve", "POST /api/sync/prs", "POST /api/sync/issues",
		"GET /api/admin/backup.db",
	}
	for _, mode := range []string{"unconfigured", "anonymous", "other-user", "legacy-cookie"} {
		t.Run(mode, func(t *testing.T) {
			s := oauthServer()
			var c *http.Cookie
			switch mode {
			case "unconfigured":
				s = New(nil, nil, Options{})
			case "other-user":
				c = githubLogin(t, s, 123)
			case "legacy-cookie":
				c = &http.Cookie{Name: sessionCookieName, Value: s.sessionDigest}
			}
			for _, route := range routes {
				parts := strings.SplitN(route, " ", 2)
				req := httptest.NewRequest(parts[0], parts[1], strings.NewReader("{}"))
				req.Header.Set("Content-Type", "application/json")
				if c != nil {
					req.AddCookie(c)
				}
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != 403 {
					t.Errorf("%s: got %d", route, rec.Code)
				}
			}
		})
	}
}
func TestGitHubTokenPolicy(t *testing.T) {
	s := oauthServer()
	req := httptest.NewRequest("GET", "/api/session", nil)
	req.Header.Set("Authorization", "Bearer cli-secret")
	if !s.resolveSession(req).Permissions.CanTriage {
		t.Fatal("CLI token rejected")
	}
	for _, srv := range []*Server{s, New(nil, nil, Options{})} {
		req := httptest.NewRequest("POST", "/api/session/login", strings.NewReader(`{"token":"cli-secret"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatal("browser token login accepted")
		}
	}
}
func TestGitHubConfigValidation(t *testing.T) {
	for _, callback := range []string{"http://example.com/auth/github/callback", "https://example.com/wrong", "https://example.com/auth/github/callback?x=1"} {
		opts := oauthServer().opts.GitHub
		opts.CallbackURL = callback
		if opts.Validate() == nil {
			t.Errorf("accepted %s", callback)
		}
	}
	opts := oauthServer().opts.GitHub
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	opts.ClientSecret = ""
	if opts.Validate() == nil {
		t.Fatal("accepted partial config")
	}
}

func TestGitHubProviderFailureNeverCreatesSession(t *testing.T) {
	for _, failure := range []string{"token-status", "token-error", "user-status", "user-invalid"} {
		t.Run(failure, func(t *testing.T) {
			s := oauthServer()
			s.github.pending["state"] = oauthEntry{time.Now().Add(time.Minute), "verifier"}
			s.github.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, `{"access_token":"token"}`
				if r.URL.Host == "github.com" {
					if failure == "token-status" {
						status = 503
					}
					if failure == "token-error" {
						body = `{"error":"bad_verification_code"}`
					}
				} else {
					body = `{"login":"winoros"}`
					if failure == "user-status" {
						status = 401
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			req := httptest.NewRequest("GET", "/auth/github/callback?state=state&code=code", nil)
			req.AddCookie(&http.Cookie{Name: oauthCookie, Value: "state"})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway || len(s.github.sessions) != 0 {
				t.Fatal("failed provider request authenticated a user")
			}
		})
	}
}

func TestAdditionalGitHubMaintainers(t *testing.T) {
	ids, err := ParseGitHubMaintainerIDs("7499936,146148086,111482174,29879298,15825830,25057648,5520054")
	if err != nil {
		t.Fatal(err)
	}
	s := oauthServer()
	s.opts.GitHub.MaintainerIDs = ids
	for _, id := range append([]int64{7846227}, ids...) {
		sess := s.githubSessionInfo(githubIdentity{ID: id, Login: "renamed-user"})
		if !sess.Permissions.CanTriage || !sess.Permissions.CanSync || sess.Viewer.Role != "maintainer" {
			t.Fatalf("maintainer %d denied: %+v", id, sess)
		}
		s.opts.ReadOnly = true
		sess = s.githubSessionInfo(githubIdentity{ID: id})
		if sess.Permissions.CanTriage || sess.Permissions.CanSync {
			t.Fatalf("readonly bypass by %d", id)
		}
		s.opts.ReadOnly = false
	}
	for _, login := range []string{"winoros", "qw4990", "terry1purcell", "elsa0520", "0xPoe", "AilinKid", "time-and-fate", "mjonss"} {
		sess := s.githubSessionInfo(githubIdentity{ID: 123, Login: login})
		if sess.Permissions.CanTriage || sess.Permissions.CanSync {
			t.Fatalf("name granted access: %s", login)
		}
	}
	cookie := githubLogin(t, s, ids[0])
	req := httptest.NewRequest("GET", "/api/session", nil)
	req.AddCookie(cookie)
	if !s.resolveSession(req).Permissions.CanTriage {
		t.Fatal("OAuth maintainer denied")
	}
	s.opts.GitHub.MaintainerIDs = nil
	if s.resolveSession(req).Permissions.CanTriage {
		t.Fatal("removed maintainer still authorized")
	}
}

func TestParseGitHubMaintainerIDs(t *testing.T) {
	for _, raw := range []string{"-1", "0", "abc", "1,", "1,,2", "99999999999999999999999"} {
		if _, err := ParseGitHubMaintainerIDs(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	ids, err := ParseGitHubMaintainerIDs(" 42, 43,42 ")
	if err != nil || len(ids) != 2 || ids[0] != 42 || ids[1] != 43 {
		t.Fatalf("parse: %v %v", ids, err)
	}
	ids, err = ParseGitHubMaintainerIDs(" ")
	if err != nil || len(ids) != 0 {
		t.Fatal("empty allowlist must be valid")
	}
}
