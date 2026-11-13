package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/jsonutil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
)

type Options struct {
	WebDir   string
	ReadOnly bool
}

type Server struct {
	store  *store.Store
	syncer *syncer.Syncer
	opts   Options
	mux    *http.ServeMux
}

func New(st *store.Store, sy *syncer.Syncer, opts Options) *Server {
	s := &Server{
		store:  st,
		syncer: sy,
		opts:   opts,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
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

// resolveSession is the single authorization hook for future OAuth / reverse-proxy RBAC.
func (s *Server) resolveSession(_ *http.Request) model.SessionInfo {
	if s.opts.ReadOnly {
		return model.SessionInfo{
			Viewer: model.Viewer{Login: "viewer", Role: "viewer"},
			Permissions: model.Permissions{
				CanTriage: false,
				CanSync:   false,
			},
		}
	}
	return model.SessionInfo{
		Viewer: model.Viewer{Login: "local-maintainer", Role: "maintainer"},
		Permissions: model.Permissions{
			CanTriage: true,
			CanSync:   true,
		},
	}
}

func noStoreMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
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
	i18nFS := noStoreMiddleware(http.FileServer(http.Dir(filepath.Join(s.opts.WebDir, "i18n"))))
	s.mux.Handle("GET /assets/i18n/", http.StripPrefix("/assets/i18n/", i18nFS))
	s.mux.Handle("GET /i18n/", http.StripPrefix("/i18n/", i18nFS))
	s.mux.HandleFunc("GET /bootstrap.js", s.handleBootstrapJS)

	// REST API
	s.mux.HandleFunc("GET /api/session", s.handleGetSession)
	s.mux.HandleFunc("GET /api/state", s.handleGetState)
	s.mux.HandleFunc("GET /api/dataset.json", s.handleGetDatasetJSON)
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
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		return "", err
	}
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	idxPath := filepath.Join(s.opts.WebDir, "index.html")
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("read index.html: %v", err), http.StatusInternalServerError)
		return
	}

	var bundle strings.Builder
	bundle.WriteString("<script>\n")
	for _, name := range []string{"i18n.js", "zh-CN.js", "en.js"} {
		p := filepath.Join(s.opts.WebDir, "i18n", name)
		b, err := os.ReadFile(p)
		if err != nil {
			http.Error(w, fmt.Sprintf("read %s: %v", name, err), http.StatusInternalServerError)
			return
		}
		bundle.WriteString(sanitizeInlineScript(string(b)))
		bundle.WriteString("\n")
	}
	bootJS, err := s.buildBootstrapPayloadJS(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("build bootstrap state: %v", err), http.StatusInternalServerError)
		return
	}
	bundle.WriteString(bootJS)
	bundle.WriteString("</script>")

	html := strings.Replace(string(raw), "<!--SERVER_INLINE_BUNDLE-->", bundle.String(), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
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
	writeJSON(w, http.StatusOK, s.resolveSession(r))
}

func (s *Server) handleGetDatasetJSON(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
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
	sess := s.resolveSession(r)
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
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
// 2. Origin (if sent by browser) must match r.Host.
// 3. Requests carrying a body or Content-Type header must use "application/json"
//    (blocking cross-origin HTML form / navigator.sendBeacon simple-request CSRF).
func guardMutation(w http.ResponseWriter, r *http.Request) bool {
	if sfs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); sfs != "" {
		if !strings.EqualFold(sfs, "same-origin") && !strings.EqualFold(sfs, "none") {
			writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: cross-site %s request blocked", r.Method))
			return false
		}
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if origin == "null" || err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
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
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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

func (s *Server) syncMissingOpenPRsBestEffort(ctx context.Context) string {
	if s.syncer == nil {
		return ""
	}
	missing, err := s.store.ListMissingOpenPRTargets()
	if err != nil || len(missing) == 0 {
		return ""
	}
	prCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, syncErr := s.syncer.SyncSpecificPRs(prCtx, missing, 10); syncErr != nil {
		return syncErr.Error()
	}
	return ""
}

func (s *Server) handlePatchIssueTriage(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
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
	rec, err := s.store.SaveIssueOverride(num, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	prSyncWarn := s.syncMissingOpenPRsBestEffort(r.Context())
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, r0 := range ds.Records {
		if r0.Number == num {
			rec = r0
			break
		}
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
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	rec, err := s.store.DeleteIssueOverride(num)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	prSyncWarn := s.syncMissingOpenPRsBestEffort(r.Context())
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, r0 := range ds.Records {
		if r0.Number == num {
			rec = r0
			break
		}
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
	sess := s.resolveSession(r)
	if !sess.Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	if _, err := s.store.AcknowledgeBatchRetriage([]int{num}, sess.Viewer.Login); err != nil {
		writeErr(w, http.StatusBadRequest, err)
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
	sess := s.resolveSession(r)
	if !sess.Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
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
		writeErr(w, http.StatusBadRequest, err)
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
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
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
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, _ := s.store.ListPendingCandidates()
	writeJSON(w, http.StatusOK, map[string]any{
		"dataset":            ds,
		"pending_candidates": pending,
	})
}

func (s *Server) handleGetExcluded(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
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
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || num <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid issue number"))
		return
	}
	if err := s.store.RestoreExcludedIssue(num); err != nil {
		writeErr(w, http.StatusBadRequest, err)
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
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
		return
	}
	if !guardMutation(w, r) {
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
	missingTargets, err := s.store.ResolvePendingCandidateWithTargets(num, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var prSyncWarn string
	if len(missingTargets) > 0 && s.syncer != nil {
		prCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		_, syncErr := s.syncer.SyncSpecificPRs(prCtx, missingTargets, 10)
		cancel()
		if syncErr != nil {
			prSyncWarn = syncErr.Error()
		} else if req.Action == "accept" {
			_ = s.store.RefreshAcceptedIssueBaselines([]int{num})
		}
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
	if !s.resolveSession(r).Permissions.CanSync {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: sync permission required"))
		return
	}
	if !guardMutation(w, r) {
		return
	}
	if s.syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("syncer is not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	stats, err := s.syncer.SyncPRStatuses(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, _ := s.store.ListPendingCandidates()
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":              stats,
		"dataset":            ds,
		"pending_candidates": pending,
	})
}

func (s *Server) handleSyncIssues(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanSync {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: sync permission required"))
		return
	}
	if !guardMutation(w, r) {
		return
	}
	if s.syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("syncer is not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var body struct {
		Full bool `json:"full"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	res, err := s.syncer.SyncIssuesDualStream(ctx, body.Full)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ds, err := s.store.BuildDataset(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending, _ := s.store.ListPendingCandidates()
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
