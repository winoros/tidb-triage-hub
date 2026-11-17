package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

// GitHubOptions configures identity-only OAuth using stable GitHub user IDs.
type GitHubOptions struct {
	ClientID      string
	ClientSecret  string
	CallbackURL   string
	OwnerID       int64
	MaintainerIDs []int64
}

// ParseGitHubMaintainerIDs parses an optional comma-separated allowlist.
func ParseGitHubMaintainerIDs(raw string) ([]int64, error) {
	var ids []int64
	if strings.TrimSpace(raw) == "" {
		return ids, nil
	}
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("GitHub maintainer IDs must be positive integers")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (o GitHubOptions) Enabled() bool {
	return o.ClientID != "" && o.ClientSecret != "" && o.CallbackURL != ""
}
func (o GitHubOptions) Validate() error {
	if o.ClientID == "" && o.ClientSecret == "" && o.CallbackURL == "" {
		return nil
	}
	if !o.Enabled() {
		return fmt.Errorf("GitHub OAuth requires client ID, client secret and callback URL")
	}
	u, err := url.Parse(o.CallbackURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/auth/github/callback" {
		return fmt.Errorf("GitHub callback must be an absolute URL ending in /auth/github/callback")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return fmt.Errorf("GitHub callback requires HTTPS (HTTP allowed only on loopback)")
	}
	if o.OwnerID <= 0 {
		return fmt.Errorf("GitHub owner ID must be positive")
	}
	return nil
}

const githubCookie = "triage_hub_github"
const oauthCookie = "triage_hub_oauth_state"

type githubIdentity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type oauthEntry struct {
	Expires  time.Time
	Verifier string
}
type githubAuth struct {
	mu      sync.Mutex
	pending map[string]oauthEntry
	client  *http.Client
}

func newGitHubAuth() *githubAuth {
	return &githubAuth{pending: make(map[string]oauthEntry),
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func randomAuthValue() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (a *githubAuth) prune() {
	now := time.Now()
	for k, v := range a.pending {
		if !now.Before(v.Expires) {
			delete(a.pending, k)
		}
	}
}
func (s *Server) githubCookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age,
		HttpOnly: true, Secure: strings.HasPrefix(s.opts.GitHub.CallbackURL, "https://"), SameSite: http.SameSiteLaxMode})
}

// A read-only SQLite connection cannot create or revoke durable sessions.
func (s *Server) githubLoginEnabled() bool {
	return s.opts.GitHub.Enabled() && s.store != nil && !s.store.ReadOnly()
}

func (s *Server) githubSession(r *http.Request) (githubIdentity, bool) {
	c, err := r.Cookie(githubCookie)
	if err != nil {
		return githubIdentity{}, false
	}
	if s.store == nil {
		return githubIdentity{}, false
	}
	session, ok, err := s.store.LoadGitHubSession(r.Context(), c.Value, s.githubAudience())
	return githubIdentity{ID: session.UserID, Login: session.Login}, ok && err == nil
}

// Bind sessions to this OAuth app and deployment, while allowing secret rotation.
func (s *Server) githubAudience() string {
	return s.opts.GitHub.ClientID + "\n" + s.opts.GitHub.CallbackURL
}

func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.githubLoginEnabled() || s.opts.GitHub.Validate() != nil {
		http.Error(w, "GitHub login is not configured", http.StatusServiceUnavailable)
		return
	}
	state, verifier := randomAuthValue(), randomAuthValue()
	s.github.mu.Lock()
	s.github.prune()
	if len(s.github.pending) >= 1024 {
		s.github.mu.Unlock()
		http.Error(w, "Too many login attempts; retry shortly", http.StatusTooManyRequests)
		return
	}
	s.github.pending[state] = oauthEntry{time.Now().Add(10 * time.Minute), verifier}
	s.github.mu.Unlock()
	s.githubCookie(w, oauthCookie, state, 600)
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {s.opts.GitHub.ClientID}, "redirect_uri": {s.opts.GitHub.CallbackURL},
		"state": {state}, "scope": {""}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.githubLoginEnabled() || s.opts.GitHub.Validate() != nil {
		http.Error(w, "GitHub login is not configured", http.StatusServiceUnavailable)
		return
	}
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(oauthCookie)
	if err != nil || state == "" || c.Value != state {
		http.Error(w, "Invalid login state; try signing in again", http.StatusBadRequest)
		return
	}
	s.githubCookie(w, oauthCookie, "", -1)
	s.github.mu.Lock()
	entry, ok := s.github.pending[state]
	delete(s.github.pending, state)
	s.github.mu.Unlock()
	if !ok || !time.Now().Before(entry.Expires) || r.URL.Query().Get("code") == "" || r.URL.Query().Get("error") != "" {
		http.Error(w, "Login expired or cancelled; try signing in again", http.StatusBadRequest)
		return
	}
	form := url.Values{"client_id": {s.opts.GitHub.ClientID}, "client_secret": {s.opts.GitHub.ClientSecret},
		"redirect_uri": {s.opts.GitHub.CallbackURL}, "code": {r.URL.Query().Get("code")}, "code_verifier": {entry.Verifier}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.githubJSON(req, &token); err != nil || token.AccessToken == "" {
		http.Error(w, "GitHub login failed; try again", http.StatusBadGateway)
		return
	}
	req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var user githubIdentity
	if err := s.githubJSON(req, &user); err != nil || user.ID <= 0 || user.Login == "" {
		http.Error(w, "Cannot verify GitHub identity; try again", http.StatusBadGateway)
		return
	}
	id := randomAuthValue()
	oldToken := ""
	if old, err := r.Cookie(githubCookie); err == nil {
		oldToken = old.Value
	}
	if s.store == nil {
		http.Error(w, "Login session storage unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.store.CreateGitHubSession(r.Context(), id, oldToken, s.githubAudience(),
		store.GitHubSession{UserID: user.ID, Login: user.Login, Expires: time.Now().Add(24 * time.Hour)}); err != nil {
		http.Error(w, "Cannot save login session; try again", http.StatusServiceUnavailable)
		return
	}
	s.githubCookie(w, githubCookie, id, 86400)
	s.writeSessionCookie(w, r, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (s *Server) githubJSON(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tidb-triage-hub")
	resp, err := s.github.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
func (s *Server) githubSessionInfo(user githubIdentity) model.SessionInfo {
	canWrite := !s.opts.ReadOnly && user.ID > 0 && (user.ID == s.opts.GitHub.OwnerID || slices.Contains(s.opts.GitHub.MaintainerIDs, user.ID))
	role := "viewer"
	if canWrite {
		role = "maintainer"
	}
	return model.SessionInfo{Viewer: model.Viewer{Login: user.Login, Role: role},
		Permissions:  model.Permissions{CanTriage: canWrite, CanSync: canWrite},
		AuthRequired: true, Authenticated: true, GitHubLogin: true}
}
