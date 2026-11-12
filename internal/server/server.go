package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
		safeJS := strings.ReplaceAll(string(b), "</script", "<\\/script")
		bundle.WriteString(safeJS)
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

func (s *Server) handlePatchIssueTriage(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
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
	writeJSON(w, http.StatusOK, map[string]any{"record": rec})
}

func (s *Server) handleDeleteIssueTriage(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
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
	writeJSON(w, http.StatusOK, map[string]any{"record": rec})
}

func (s *Server) handleResolvePending(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanTriage {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: triage permission required"))
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
	if err := s.store.ResolvePendingCandidate(num, req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
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

func (s *Server) handleSyncPRs(w http.ResponseWriter, r *http.Request) {
	if !s.resolveSession(r).Permissions.CanSync {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: sync permission required"))
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var body struct {
		Full bool `json:"full"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

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
