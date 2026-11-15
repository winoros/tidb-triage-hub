package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/httputil"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/jsonutil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
	webassets "github.com/winoros/tidb-triage-hub/web"
)

const sessionCookieName = "triage_hub_session"

type Options struct {
	WebDir     string
	ReadOnly   bool
	AuthToken  string
	DBDisplay  string
	TrustProxy bool
}

type cachedIndexAssets struct {
	modTimes       [4]time.Time
	htmlPrefix     string
	htmlSuffix     string
	hasPlaceholder bool
	i18nInline     string
}

type Server struct {
	store         *store.Store
	syncer        *syncer.Syncer
	opts          Options
	hasAuthToken  bool
	authTokenHash [32]byte
	sessionDigest string
	backupMu      sync.Mutex
	indexCacheMu  sync.RWMutex
	indexCache    *cachedIndexAssets
	mux           *http.ServeMux
}

func New(st *store.Store, sy *syncer.Syncer, opts Options) *Server {
	if sy == nil && st != nil {
		sy = syncer.New(st)
	}
	tok := strings.TrimSpace(opts.AuthToken)
	s := &Server{
		store:        st,
		syncer:       sy,
		opts:         opts,
		hasAuthToken: tok != "",
		mux:          http.NewServeMux(),
	}
	if tok != "" {
		s.authTokenHash = sha256.Sum256([]byte(tok))
		s.sessionDigest = sessionCookieDigest(tok)
	}
	s.registerRoutes()
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		s.mux.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

func sessionCookieDigest(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("tidb-triage-hub:session:v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func firstHeaderHop(val string) string {
	if idx := strings.IndexByte(val, ','); idx >= 0 {
		val = val[:idx]
	}
	return strings.TrimSpace(val)
}

func (s *Server) verifySecretToken(candidate string) bool {
	if !s.hasAuthToken {
		return true
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(candidate)))
	return subtle.ConstantTimeCompare(got[:], s.authTokenHash[:]) == 1
}

func isLoopbackPeer(remoteAddr string) bool {
	raw := strings.TrimSpace(remoteAddr)
	if raw == "" {
		return false
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) isTrustedProxyPeer(r *http.Request) bool {
	return s.opts.TrustProxy || isLoopbackPeer(r.RemoteAddr)
}

func (s *Server) effectiveRequestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.isTrustedProxyPeer(r) {
		xfp := firstHeaderHop(r.Header.Get("X-Forwarded-Proto"))
		if strings.EqualFold(xfp, "https") {
			return "https"
		}
		if strings.EqualFold(xfp, "http") {
			return "http"
		}
	}
	return "http"
}

// hasInvalidAuthHeader reports whether the caller explicitly supplied a Bearer Authorization header
// that fails token verification when token auth is enabled. Non-Bearer schemes (e.g. reverse-proxy Basic Auth)
// are ignored so cookie-authenticated browser sessions continue to work behind Basic Auth proxies.
func (s *Server) hasInvalidAuthHeader(r *http.Request) bool {
	if s.opts.ReadOnly || !s.hasAuthToken {
		return false
	}
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 0 || !strings.EqualFold(fields[0], "Bearer") {
		return false
	}
	if len(fields) != 2 {
		return true
	}
	return !s.verifySecretToken(fields[1])
}

func (s *Server) isRequestAuthenticated(r *http.Request) bool {
	if !s.hasAuthToken {
		return true
	}
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) > 0 && strings.EqualFold(fields[0], "Bearer") {
		return len(fields) == 2 && s.verifySecretToken(fields[1])
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		got := sha256.Sum256([]byte(strings.TrimSpace(c.Value)))
		want := sha256.Sum256([]byte(s.sessionDigest))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) sessionFor(authenticated bool) model.SessionInfo {
	if s.opts.ReadOnly {
		return model.SessionInfo{
			Viewer:      model.Viewer{Login: "viewer", Role: "viewer"},
			Permissions: model.Permissions{CanTriage: false, CanSync: false},
		}
	}
	if !s.hasAuthToken {
		return model.SessionInfo{
			Viewer:      model.Viewer{Login: "local-maintainer", Role: "maintainer"},
			Permissions: model.Permissions{CanTriage: true, CanSync: true},
		}
	}
	if authenticated {
		return model.SessionInfo{
			Viewer:        model.Viewer{Login: "maintainer", Role: "maintainer"},
			Permissions:   model.Permissions{CanTriage: true, CanSync: true},
			AuthRequired:  true,
			Authenticated: true,
		}
	}
	return model.SessionInfo{
		Viewer:        model.Viewer{Login: "viewer", Role: "viewer"},
		Permissions:   model.Permissions{CanTriage: false, CanSync: false},
		AuthRequired:  true,
		Authenticated: false,
	}
}

// resolveSession resolves RBAC permissions from ReadOnly flag, Bearer token, or maintainer cookie.
func (s *Server) resolveSession(r *http.Request) model.SessionInfo {
	if s.opts.ReadOnly || !s.hasAuthToken {
		return s.sessionFor(true)
	}
	return s.sessionFor(s.isRequestAuthenticated(r))
}

func (s *Server) requireTriageSession(w http.ResponseWriter, r *http.Request) (model.SessionInfo, bool) {
	if s.hasInvalidAuthHeader(r) {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: invalid bearer token"))
		return model.SessionInfo{}, false
	}
	sess := s.resolveSession(r)
	if !sess.Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return model.SessionInfo{}, false
	}
	return sess, true
}

func (s *Server) requireSyncSession(w http.ResponseWriter, r *http.Request) (model.SessionInfo, bool) {
	if s.hasInvalidAuthHeader(r) {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: invalid bearer token"))
		return model.SessionInfo{}, false
	}
	sess := s.resolveSession(r)
	if !sess.Permissions.CanSync {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: sync permission required"))
		return model.SessionInfo{}, false
	}
	return sess, true
}

func (s *Server) writeSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	secure := false
	if r.TLS != nil {
		secure = true
	} else if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "null" {
		if u, err := url.Parse(origin); err == nil && u.Scheme != "" {
			secure = strings.EqualFold(u.Scheme, "https")
		} else {
			secure = s.effectiveRequestScheme(r) == "https"
		}
	} else {
		secure = s.effectiveRequestScheme(r) == "https"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func redactDatasetForViewer(ds *model.Dataset, canTriage bool) {
	if canTriage || ds == nil {
		return
	}
	ds.SyncMeta.PendingCandidateNums = nil
	ds.SyncMeta.ExcludedIssueNums = nil
	ds.SyncMeta.RetriageIssueNums = nil
}

func resolveRequestTimeout(r *http.Request, defDur, maxDur time.Duration) time.Duration {
	if raw := strings.TrimSpace(r.Header.Get("X-Triage-Timeout-Ms")); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			const maxSafeMs = int64(time.Hour/time.Millisecond) * 24
			if ms > maxSafeMs {
				ms = maxSafeMs
			}
			d := time.Duration(ms) * time.Millisecond
			if d < time.Second {
				d = time.Second
			}
			if maxDur > 0 && d > maxDur {
				d = maxDur
			}
			return d
		}
	}
	return defDur
}

func (s *Server) serveI18nAsset(prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, prefix)
		cleanRel := strings.TrimPrefix(path.Clean("/"+rel), "/")
		if cleanRel == "" || strings.Contains(cleanRel, "/") || strings.Contains(cleanRel, "..") {
			http.NotFound(w, r)
			return
		}
		b, err := s.readWebAsset(filepath.Join("i18n", cleanRel))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(strings.ToLower(cleanRel), ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		} else if ct := mime.TypeByExtension(filepath.Ext(cleanRel)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	}
}

func (s *Server) readWebAsset(relPath string) ([]byte, error) {
	if dir := strings.TrimSpace(s.opts.WebDir); dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, relPath))
		if err == nil {
			return b, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return fs.ReadFile(webassets.FS, filepath.ToSlash(relPath))
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /index.html", s.handleIndex)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// Legacy path redirect for old bookmarks
	s.mux.HandleFunc("GET /reports/tidb-optimizer-issues/", func(w http.ResponseWriter, r *http.Request) {
		target := "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
	})

	// Standalone static fallback & live bootstrap payload
	s.mux.HandleFunc("GET /assets/i18n/", s.serveI18nAsset("/assets/i18n/"))
	s.mux.HandleFunc("GET /i18n/", s.serveI18nAsset("/i18n/"))
	s.mux.HandleFunc("GET /bootstrap.js", s.handleBootstrapJS)

	// REST API
	s.mux.HandleFunc("GET /api/session", s.handleGetSession)
	s.mux.HandleFunc("POST /api/session/login", s.handleSessionLogin)
	s.mux.HandleFunc("POST /api/session/logout", s.handleSessionLogout)
	s.mux.HandleFunc("GET /api/status", s.handleGetStatus)
	s.mux.HandleFunc("GET /api/state", s.handleGetState)
	s.mux.HandleFunc("GET /api/dataset.json", s.handleGetDatasetJSON)
	s.mux.HandleFunc("GET /api/triage/pending", s.handleGetTriagePending)
	s.mux.HandleFunc("POST /api/triage/decisions", s.handlePostTriageDecisions)
	s.mux.HandleFunc("GET /api/admin/backup.db", s.handleGetBackupDB)
	s.mux.HandleFunc("PATCH /api/issues/{number}/triage", s.handlePatchIssueTriage)
	s.mux.HandleFunc("DELETE /api/issues/{number}/triage", s.handleDeleteIssueTriage)
	s.mux.HandleFunc("POST /api/issues/{number}/retriage/ack", s.handleAckIssueRetriage)
	s.mux.HandleFunc("POST /api/retriage/ack-batch", s.handleAckBatchRetriage)
	s.mux.HandleFunc("POST /api/issues/{number}/exclude", s.handleExcludeIssue)
	s.mux.HandleFunc("GET /api/excluded", s.handleGetExcluded)
	s.mux.HandleFunc("DELETE /api/excluded/{number}", s.handleDeleteExcluded)
	s.mux.HandleFunc("POST /api/pending/{number}/resolve", s.handleResolvePending)
	s.mux.HandleFunc("POST /api/sync/prs", s.handleSyncPRs)
	s.mux.HandleFunc("POST /api/sync/issues", s.handleSyncIssues)
}

func (s *Server) buildBootstrapPayloadJS(r *http.Request) (string, error) {
	sess := s.resolveSession(r)
	ds, err := s.store.BuildDataset(sess.Permissions.CanTriage)
	if err != nil {
		return "", err
	}
	redactDatasetForViewer(&ds, sess.Permissions.CanTriage)
	var pending []model.PendingCandidate
	if sess.Permissions.CanTriage {
		pending, err = s.store.ListPendingCandidates()
		if err != nil {
			return "", err
		}
	} else {
		pending = []model.PendingCandidate{}
	}

	sessJS, err := jsonutil.MarshalCompactJS(sess)
	if err != nil {
		return "", err
	}
	dsJS, err := jsonutil.MarshalCompactJS(ds)
	if err != nil {
		return "", err
	}
	pendJS, err := jsonutil.MarshalCompactJS(pending)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("window.TRIAGE_SESSION = %s;\nwindow.REPORT_DATASET = %s;\nwindow.TRIAGE_PENDING = %s;\n", sessJS, dsJS, pendJS), nil
}

var reInlineScriptClose = regexp.MustCompile(`(?i)</script`)

func sanitizeInlineScript(js string) string {
	s := reInlineScriptClose.ReplaceAllString(js, `<\/script`)
	return strings.ReplaceAll(s, "<!--", `<`+`\!--`)
}

func (s *Server) loadIndexAssets() (*cachedIndexAssets, error) {
	relPaths := [4]string{
		"index.html",
		filepath.Join("i18n", "i18n.js"),
		filepath.Join("i18n", "zh-CN.js"),
		filepath.Join("i18n", "en.js"),
	}
	var mtimes [4]time.Time
	if dir := strings.TrimSpace(s.opts.WebDir); dir != "" {
		for i, rel := range relPaths {
			if info, err := os.Stat(filepath.Join(dir, rel)); err == nil {
				mtimes[i] = info.ModTime()
			}
		}
	}

	s.indexCacheMu.RLock()
	cached := s.indexCache
	s.indexCacheMu.RUnlock()
	if cached != nil && cached.modTimes == mtimes {
		return cached, nil
	}

	rawHTML, err := s.readWebAsset(relPaths[0])
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}
	var i18nBuf strings.Builder
	for idx, name := range []string{"i18n.js", "zh-CN.js", "en.js"} {
		b, rErr := s.readWebAsset(relPaths[idx+1])
		if rErr != nil {
			return nil, fmt.Errorf("read %s: %w", name, rErr)
		}
		i18nBuf.WriteString(sanitizeInlineScript(string(b)))
		i18nBuf.WriteByte('\n')
	}

	htmlStr := string(rawHTML)
	const marker = "<!--SERVER_INLINE_BUNDLE-->"
	prefix, suffix, hasMarker := strings.Cut(htmlStr, marker)
	built := &cachedIndexAssets{
		modTimes:       mtimes,
		htmlPrefix:     prefix,
		htmlSuffix:     suffix,
		hasPlaceholder: hasMarker,
		i18nInline:     i18nBuf.String(),
	}

	s.indexCacheMu.Lock()
	s.indexCache = built
	s.indexCacheMu.Unlock()
	return built, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	assets, err := s.loadIndexAssets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bootJS, err := s.buildBootstrapPayloadJS(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("build bootstrap state: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, assets.htmlPrefix)
	if assets.hasPlaceholder {
		_, _ = io.WriteString(w, "<script>\n")
		_, _ = io.WriteString(w, assets.i18nInline)
		_, _ = io.WriteString(w, bootJS)
		_, _ = io.WriteString(w, "</script>")
		_, _ = io.WriteString(w, assets.htmlSuffix)
	}
}

func (s *Server) handleBootstrapJS(w http.ResponseWriter, r *http.Request) {
	bootJS, err := s.buildBootstrapPayloadJS(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(bootJS))
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if s.hasInvalidAuthHeader(r) {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: invalid bearer token"))
		return
	}
	writeJSON(w, http.StatusOK, s.resolveSession(r))
}

func (s *Server) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	if !s.guardMutation(w, r) {
		return
	}
	if s.opts.ReadOnly {
		writeErr(w, http.StatusForbidden, fmt.Errorf("server is running in read-only mode"))
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.hasAuthToken {
		if !s.verifySecretToken(req.Token) {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("invalid access token"))
			return
		}
		s.writeSessionCookie(w, r, s.sessionDigest, 30*24*3600)
	}
	writeJSON(w, http.StatusOK, s.sessionFor(true))
}

func (s *Server) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	if !s.guardMutation(w, r) {
		return
	}
	s.writeSessionCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, s.sessionFor(false))
}

func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if s.hasInvalidAuthHeader(r) {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: invalid bearer token"))
		return
	}
	sess := s.resolveSession(r)
	q := r.URL.Query()
	requireAuth := model.ParseBoolDefault(q.Get("require_auth"), false)
	if requireAuth && sess.AuthRequired && !sess.Permissions.CanTriage {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: maintainer token required"))
		return
	}
	wantGHReq := model.ParseBoolDefault(q.Get("check_gh"), false)
	if wantGHReq && sess.AuthRequired && !sess.Permissions.CanSync {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: maintainer token required for check_gh=1"))
		return
	}
	wantGH := wantGHReq && sess.Permissions.CanSync
	dbDisp := strings.TrimSpace(s.opts.DBDisplay)
	if dbDisp == "" {
		dbDisp = "remote:tidb_triage.db"
	}
	out, err := s.syncer.BuildStatusReport(r.Context(), dbDisp, wantGH)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if wantGHReq && !wantGH && s.opts.ReadOnly {
		out["gh_check_error"] = "GitHub quota check is disabled on read-only server"
	}
	if !sess.Permissions.CanTriage {
		if dbMap, ok := out["database"].(map[string]any); ok {
			dbMap["db_path"] = "redacted"
			delete(dbMap, "pending_numbers")
			delete(dbMap, "retriage_numbers")
		}
		out["recent_sync_runs"] = []store.SyncRunRecord{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTriagePending(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	q := r.URL.Query()
	var opts syncer.PendingBundleOptions
	if v := strings.TrimSpace(q.Get("number")); v != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(v, "#"))
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid number %q", v))
			return
		}
		opts.OnlyNum = n
	}
	if v := strings.TrimSpace(q.Get("numbers")); v != "" {
		nums, err := model.ParseIssueNumCSV(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("numbers: %w", err))
			return
		}
		opts.Numbers = nums
	}
	if v := strings.TrimSpace(q.Get("re_triage")); v != "" {
		nums, err := model.ParseIssueNumCSV(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("re_triage: %w", err))
			return
		}
		opts.ReTriageNums = nums
	}
	if v := strings.TrimSpace(q.Get("backfill_notes")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid backfill_notes %q", v))
			return
		}
		opts.BackfillNotes = n
	}
	opts.ScopeFilter = q.Get("scope")
	opts.OnlyPending = model.ParseBoolDefault(q.Get("only_pending"), false)
	opts.OnlyRetriage = model.ParseBoolDefault(q.Get("only_retriage"), false)
	opts.CatFilter = q.Get("cat")
	opts.Compact = model.ParseBoolDefault(q.Get("compact"), false)
	opts.EnrichPRs = model.ParseBoolDefault(q.Get("enrich_prs"), true)

	ctx, cancel := context.WithTimeout(r.Context(), resolveRequestTimeout(r, 90*time.Second, 10*time.Minute))
	defer cancel()

	bundle, _, err := s.syncer.BuildPendingBundle(ctx, opts)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, http.StatusGatewayTimeout, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

func (s *Server) handlePostTriageDecisions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	var req syncer.BatchTriageExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), resolveRequestTimeout(r, 3*time.Minute, 30*time.Minute))
	defer cancel()

	res, err := s.syncer.ExecuteBatchTriage(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBatchConflict) || errors.Is(err, store.ErrOverrideConflict):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, syncer.ErrBatchInputInvalid) || errors.Is(err, store.ErrCanonicalCannotExclude) || errors.Is(err, store.ErrIssueNotFound):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, syncer.ErrMissingGitHubPR):
			writeErr(w, http.StatusUnprocessableEntity, err)
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusGatewayTimeout, err)
		default:
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  err.Error(),
				"result": res,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGetBackupDB(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	reqTimeout := resolveRequestTimeout(r, 2*time.Minute, 30*time.Minute)
	bakCtx, cancel := context.WithTimeout(r.Context(), reqTimeout)
	defer cancel()

	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	stageParent := ""
	if dbp := strings.TrimSpace(s.store.DBPath()); dbp != "" {
		stageParent = filepath.Dir(dbp)
	}
	tmpDir, err := os.MkdirTemp(stageParent, ".triage-hub-backup-*")
	if err != nil && stageParent != "" {
		tmpDir, err = os.MkdirTemp("", ".triage-hub-backup-*")
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(tmpDir)

	snapPath := filepath.Join(tmpDir, "tidb_triage_backup.db")
	if err := s.store.BackupToContext(bakCtx, snapPath); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, http.StatusGatewayTimeout, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	f, err := os.Open(snapPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	rc := http.NewResponseController(w)
	if dl, ok := bakCtx.Deadline(); ok {
		_ = rc.SetWriteDeadline(dl)
	} else {
		_ = rc.SetWriteDeadline(time.Now().Add(reqTimeout))
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tidb_triage_%s.db"`, time.Now().UTC().Format("20060102T150405Z")))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (s *Server) handleGetDatasetJSON(w http.ResponseWriter, r *http.Request) {
	if s.hasInvalidAuthHeader(r) {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: invalid bearer token"))
		return
	}
	sess := s.resolveSession(r)
	ds, err := s.store.BuildDataset(sess.Permissions.CanTriage)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	redactDatasetForViewer(&ds, sess.Permissions.CanTriage)
	b, err := jsonutil.MarshalPrettyJSON(ds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	if s.hasInvalidAuthHeader(r) {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: invalid bearer token"))
		return
	}
	sess := s.resolveSession(r)
	ds, err := s.store.BuildDataset(sess.Permissions.CanTriage)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	redactDatasetForViewer(&ds, sess.Permissions.CanTriage)
	pending := []model.PendingCandidate{}
	if sess.Permissions.CanTriage {
		pending, err = s.store.ListPendingCandidates()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session":            sess,
		"dataset":            ds,
		"pending_candidates": pending,
	})
}

// guardMutation enforces CSRF defenses on state-changing HTTP endpoints:
// 1. Sec-Fetch-Site (if sent by browser) must be "same-origin" or "none".
// 2. Origin (if sent by browser) must have an http/https scheme (no https->http downgrade)
//    and match normalized r.Host (or X-Forwarded-Host when peer is a trusted proxy).
// 3. Requests carrying a body or Content-Type header must use "application/json"
//    (blocking cross-origin HTML form / navigator.sendBeacon simple-request CSRF).
func (s *Server) guardMutation(w http.ResponseWriter, r *http.Request) bool {
	if sfs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); sfs != "" {
		if !strings.EqualFold(sfs, "same-origin") && !strings.EqualFold(sfs, "none") {
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: cross-site %s request blocked", r.Method))
			return false
		}
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		schemeOK := u != nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))
		if schemeOK && s.effectiveRequestScheme(r) == "https" && !strings.EqualFold(u.Scheme, "https") {
			schemeOK = false
		}
		hostOK := false
		if schemeOK && u.Host != "" {
			originHost := httputil.NormalizeOriginHost(u.Host, u.Scheme)
			reqHost := httputil.NormalizeOriginHost(r.Host, u.Scheme)
			hostOK = originHost != "" && originHost == reqHost
			if !hostOK && s.isTrustedProxyPeer(r) {
				xfh := httputil.NormalizeOriginHost(firstHeaderHop(r.Header.Get("X-Forwarded-Host")), u.Scheme)
				hostOK = xfh != "" && originHost == xfh
			}
		}
		if origin == "null" || err != nil || !schemeOK || !hostOK {
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: cross-origin %s request blocked", r.Method))
			return false
		}
	}
	rawCT := strings.TrimSpace(r.Header.Get("Content-Type"))
	if r.ContentLength != 0 || rawCT != "" {
		mediaType, _, err := mime.ParseMediaType(rawCT)
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: application/json Content-Type required"))
			return false
		}
	}
	if r.Body != nil {
		baseW := w
		if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
			baseW = u.Unwrap()
		}
		r.Body = http.MaxBytesReader(baseW, r.Body, 4<<20)
	}
	return true
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return err
	} else if err == nil {
		return fmt.Errorf("unexpected trailing JSON input")
	}
	return nil
}

func (s *Server) syncTargetsBestEffort(ctx context.Context, targets []prstatus.PRTarget) (string, bool) {
	if s.syncer == nil || len(targets) == 0 {
		return "", false
	}
	prCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, syncErr := s.syncer.SyncSpecificPRs(prCtx, targets, 10); syncErr != nil {
		return syncErr.Error(), false
	}
	return "", true
}

func (s *Server) handlePatchIssueTriage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	var req model.TriagePatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var prSyncWarn string
	if preTargets, tErr := s.store.ListIssueMissingPRTargets(num); tErr == nil && len(preTargets) > 0 {
		prSyncWarn, _ = s.syncTargetsBestEffort(r.Context(), preTargets)
	}
	missingTargets, err := s.store.ApplyIssueOverride(num, req)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOverrideConflict):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, store.ErrIssueNotFound):
			writeErr(w, http.StatusNotFound, err)
		default:
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	if prSyncWarn == "" && len(missingTargets) > 0 {
		prSyncWarn, _ = s.syncTargetsBestEffort(r.Context(), missingTargets)
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var rec model.IssueRecord
	found := false
	for _, r0 := range ds.Records {
		if r0.Number == num {
			rec = r0
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("issue #%d not found after update", num))
		return
	}
	resp := map[string]any{
		"record":  rec,
		"dataset": ds,
	}
	if prSyncWarn != "" {
		resp["pr_sync_warning"] = prSyncWarn
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteIssueTriage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	var prSyncWarn string
	if preTargets, tErr := s.store.ListIssueMissingPRTargets(num); tErr == nil && len(preTargets) > 0 {
		prSyncWarn, _ = s.syncTargetsBestEffort(r.Context(), preTargets)
	}
	missingTargets, err := s.store.ResetIssueOverride(num)
	if err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	if prSyncWarn == "" && len(missingTargets) > 0 {
		prSyncWarn, _ = s.syncTargetsBestEffort(r.Context(), missingTargets)
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var rec model.IssueRecord
	found := false
	for _, r0 := range ds.Records {
		if r0.Number == num {
			rec = r0
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("issue #%d not found after reset", num))
		return
	}
	resp := map[string]any{
		"record":  rec,
		"dataset": ds,
	}
	if prSyncWarn != "" {
		resp["pr_sync_warning"] = prSyncWarn
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAckIssueRetriage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireTriageSession(w, r)
	if !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	if _, err := s.store.AcknowledgeBatchRetriage([]int{num}, sess.Viewer.Login); err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var rec model.IssueRecord
	for _, r0 := range ds.Records {
		if r0.Number == num {
			rec = r0
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"record":  rec,
		"dataset": ds,
	})
}

func (s *Server) handleAckBatchRetriage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireTriageSession(w, r)
	if !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	var req struct {
		Numbers model.FlexIntList `json:"numbers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Numbers) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("numbers list cannot be empty"))
		return
	}
	acked, err := s.store.AcknowledgeBatchRetriage([]int(req.Numbers), sess.Viewer.Login)
	if err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"acknowledged": acked,
		"dataset":      ds,
	})
}

func (s *Server) handleExcludeIssue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.ExcludeExistingIssue(num, req.Reason, false); err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, err := s.store.ListPendingCandidates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dataset":            ds,
		"pending_candidates": pending,
	})
}

func (s *Server) handleGetExcluded(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	items, err := s.store.ListExcludedIssues()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"excluded_issues": items,
	})
}

func (s *Server) handleDeleteExcluded(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	if err := s.store.RestoreExcludedIssue(num); err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	items, err := s.store.ListExcludedIssues()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"excluded_issues": items,
		"dataset":         ds,
	})
}

func (s *Server) handleResolvePending(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTriageSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	var req model.PendingResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var prSyncWarn string
	if strings.EqualFold(strings.TrimSpace(req.Action), "accept") {
		if preTargets, tErr := s.store.ListPendingMissingPRTargets(num); tErr == nil && len(preTargets) > 0 {
			prSyncWarn, _ = s.syncTargetsBestEffort(r.Context(), preTargets)
		}
	}
	missingTargets, err := s.store.ResolvePendingCandidateWithTargets(num, req)
	if err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	if prSyncWarn == "" && len(missingTargets) > 0 {
		prSyncWarn, _ = s.syncTargetsBestEffort(r.Context(), missingTargets)
	}

	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, err := s.store.ListPendingCandidates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := map[string]any{
		"dataset":            ds,
		"pending_candidates": pending,
	}
	if prSyncWarn != "" {
		resp["pr_sync_warning"] = prSyncWarn
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSyncPRs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSyncSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	if s.syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("syncer is not configured"))
		return
	}

	var body struct {
		PR           string `json:"pr"`
		MinQuota     *int   `json:"min_quota"`
		IncludeState *bool  `json:"include_state"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), resolveRequestTimeout(r, 4*time.Minute, 30*time.Minute))
	defer cancel()

	mq := -1
	if body.MinQuota != nil {
		mq = *body.MinQuota
	}
	stats, err := s.syncer.ExecuteSyncPRs(ctx, body.PR, mq)
	if err != nil {
		switch {
		case errors.Is(err, syncer.ErrInvalidSyncInput):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, syncer.ErrMissingGitHubPR):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusGatewayTimeout, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	if body.IncludeState != nil && !*body.IncludeState {
		writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, err := s.store.ListPendingCandidates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":              stats,
		"dataset":            ds,
		"pending_candidates": pending,
	})
}

func (s *Server) handleSyncIssues(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSyncSession(w, r); !ok {
		return
	}
	if !s.guardMutation(w, r) {
		return
	}
	if s.syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("syncer is not configured"))
		return
	}

	var body struct {
		Full                  bool   `json:"full"`
		SkipPRs               bool   `json:"skip_prs"`
		DryRun                bool   `json:"dry_run"`
		Since                 string `json:"since"`
		OverlapMinutes        *int   `json:"overlap_minutes"`
		MaxIncrementalDays    *int   `json:"max_incremental_days"`
		UntriagedLookbackDays *int   `json:"untriaged_lookback_days"`
		MinQuota              *int   `json:"min_quota"`
		IncludeState          *bool  `json:"include_state"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	overlap := 15
	if body.OverlapMinutes != nil {
		overlap = *body.OverlapMinutes
	}
	maxIncDays := 14
	if body.MaxIncrementalDays != nil {
		maxIncDays = *body.MaxIncrementalDays
	}
	lookbackDays := issuesync.DefaultUntriagedLookbackDays
	if body.UntriagedLookbackDays != nil {
		lookbackDays = *body.UntriagedLookbackDays
	}
	minQuota := -1
	if body.MinQuota != nil {
		minQuota = *body.MinQuota
	}

	ctx, cancel := context.WithTimeout(r.Context(), resolveRequestTimeout(r, 6*time.Minute, 30*time.Minute))
	defer cancel()

	res, err := s.syncer.SyncIssuesWithOptions(ctx, syncer.IssueSyncOptions{
		Full:                  body.Full,
		SkipPRs:               body.SkipPRs || body.DryRun,
		DryRun:                body.DryRun,
		Since:                 body.Since,
		OverlapMinutes:        overlap,
		MaxIncrementalDays:    maxIncDays,
		UntriagedLookbackDays: lookbackDays,
		MinQuota:              minQuota,
	})
	if err != nil {
		switch {
		case errors.Is(err, syncer.ErrInvalidSyncInput):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusGatewayTimeout, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	if body.DryRun || (body.IncludeState != nil && !*body.IncludeState) {
		writeJSON(w, http.StatusOK, map[string]any{"result": res})
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, err := s.store.ListPendingCandidates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result":             res,
		"dataset":            ds,
		"pending_candidates": pending,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
