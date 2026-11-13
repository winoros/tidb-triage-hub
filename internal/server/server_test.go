package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
)

func setupTestServer(t *testing.T, readOnly bool) *Server {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seed := model.Dataset{
		SyncMeta: model.SyncMeta{
			SchemaVersion:                2,
			SupportedLocales:             []string{"zh-CN", "en"},
			SnapshotUTC:                  "2026-11-12T10:00:00Z",
			SnapshotUTC8:                 "2026-11-12 18:00 (UTC+8)",
			MaxIssueUpdatedUTC:           "2026-11-12T09:50:00Z",
			MaxIssueUpdatedUTC8:          "2026-11-12 17:50 (UTC+8)",
			IncrementalGHSearchQualifier: "updated:>=2026-11-12T09:50:00Z",
			TotalCandidates:              2,
			CanonicalCandidates:          1,
			RealBugs:                     1,
		},
		Enums: model.DatasetEnums{
			Categories: model.ValidCategories,
			Severities: model.ValidSeverities,
			Subsystems: model.ValidSubsystems,
		},
		Records: []model.IssueRecord{
			{
				Number:         72001,
				Title:          "wrong result on correlated subquery",
				Author:         "alice",
				Assignees:      []string{},
				Created:        "2026-11-01",
				CreatedAt:      "2026-11-01T00:00:00Z",
				UpdatedAt:      "2026-11-12T09:50:00Z",
				UpdatedDisplay: "2026-11-12 17:50",
				Labels:         []string{"sig/planner", "type/bug"},
				IsCanonical:    true,
				Cat:            "MEDIUM",
				DiffRank:       2,
				Severity:       "MAJOR",
				SevRank:        3,
				GHSeverity:     "major",
				Subsystem:      "logical_rewrite",
				PRs:            []model.LinkedPRRef{},
			},
			{
				Number:         72002,
				Title:          "mview dashboard UI display glitch",
				Author:         "bob",
				Assignees:      []string{},
				Created:        "2026-11-02",
				CreatedAt:      "2026-11-02T00:00:00Z",
				UpdatedAt:      "2026-11-12T09:40:00Z",
				UpdatedDisplay: "2026-11-12 17:40",
				Labels:         []string{"component/mview", "type/bug"},
				IsCanonical:    false,
				Cat:            "FILTER_NON_OPT_OR_FIXED",
				DiffRank:       0,
				Severity:       "N/A",
				SevRank:        0,
				GHSeverity:     "minor",
				Subsystem:      "logical_rewrite",
				PRs:            []model.LinkedPRRef{},
			},
		},
	}
	seedBytes, _ := json.Marshal(seed)
	seedPath := filepath.Join(tmp, "seed.json")
	if err := os.WriteFile(seedPath, seedBytes, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if _, err := st.BootstrapIfEmpty(seedPath); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	webDir := filepath.Join(tmp, "web")
	if err := os.MkdirAll(filepath.Join(webDir, "i18n"), 0o755); err != nil {
		t.Fatalf("mkdir web: %v", err)
	}
	for _, name := range []string{"i18n.js", "zh-CN.js", "en.js"} {
		if err := os.WriteFile(filepath.Join(webDir, "i18n", name), []byte("/* "+name+" */"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<!doctype html><title>Hub</title><!--SERVER_INLINE_BUNDLE-->"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	sy := syncer.New(st)
	return New(st, sy, Options{WebDir: webDir, ReadOnly: readOnly})
}

func TestStandaloneRoutesAndRBACGuard(t *testing.T) {
	rwSrv := setupTestServer(t, false)

	// 1. Root index served directly at / with inlined i18n + dataset bundle
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "<title>Hub</title>") ||
		!strings.Contains(rec.Body.String(), "/* zh-CN.js */") ||
		!strings.Contains(rec.Body.String(), "window.REPORT_DATASET") {
		t.Fatalf("GET / failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 2. Legacy bookmark redirects to / preserving query string
	req = httptest.NewRequest(http.MethodGet, "/reports/tidb-optimizer-issues/?view=prs&lang=zh", nil)
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/?view=prs&lang=zh" {
		t.Fatalf("expected 302 to /?view=prs&lang=zh, got code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	// 3. /bootstrap.js includes session permissions & dataset
	req = httptest.NewRequest(http.MethodGet, "/bootstrap.js", nil)
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"can_triage":true`) {
		t.Fatalf("GET /bootstrap.js unexpected: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 4. RW PATCH & retriage ack succeed and return refreshed dataset
	patchBody := `{"cat":"EASY","severity":"CRITICAL","subsystem":"logical_rewrite","note":{"zh-CN":"已确认","en":"Confirmed"}}`
	req = httptest.NewRequest(http.MethodPatch, "/api/issues/72001/triage", bytes.NewBufferString(patchBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dataset":`) {
		t.Fatalf("RW PATCH failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/issues/72001/retriage/ack", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST retriage/ack failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/retriage/ack-batch", bytes.NewBufferString(`{"numbers":[72001]}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST retriage/ack-batch failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5. Canonical exclude returns 400; supplemental exclude + list + restore succeed
	req = httptest.NewRequest(http.MethodPost, "/api/issues/72001/exclude", bytes.NewBufferString(`{"reason":"canonical"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when excluding canonical issue #72001, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/issues/72002/exclude", bytes.NewBufferString(`{"reason":"UI issue"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when excluding supplemental issue #72002, got %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/excluded", nil)
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "72002") {
		t.Fatalf("GET /api/excluded failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/excluded/72002", nil)
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/excluded/72002 failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6. ReadOnly mode blocks all mutations & triage-only endpoints with 403 Forbidden
	roSrv := setupTestServer(t, true)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/api/issues/72001/triage"},
		{http.MethodDelete, "/api/issues/72001/triage"},
		{http.MethodPost, "/api/issues/72001/retriage/ack"},
		{http.MethodPost, "/api/retriage/ack-batch"},
		{http.MethodPost, "/api/issues/72002/exclude"},
		{http.MethodGet, "/api/excluded"},
		{http.MethodDelete, "/api/excluded/72002"},
		{http.MethodPost, "/api/pending/72001/resolve"},
		{http.MethodPost, "/api/sync/prs"},
		{http.MethodPost, "/api/sync/issues"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		roSrv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for %s %s in readonly mode, got %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestRealWebAssetsBundleRender(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "real_web.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.BootstrapIfEmpty("../../seed/initial_dataset.json"); err != nil {
		t.Fatalf("bootstrap seed: %v", err)
	}
	srv := New(st, syncer.New(st), Options{WebDir: "../../web", ReadOnly: false})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with real web dir failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, needle := range []string{
		"inboxTabPendingBtn",
		"inboxTabRetriageBtn",
		"inboxTabExcludedBtn",
		"editRetriageBanner",
		"editExcludeZone",
		"fix_pr_merged_still_open",
		"window.REPORT_DATASET =",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("expected GET / bundle to contain %q", needle)
		}
	}
}

func TestCSRFOriginAndContentTypeGuard(t *testing.T) {
	rwSrv := setupTestServer(t, false)

	// 1. Cross-site Sec-Fetch-Site is rejected with 403
	req := httptest.NewRequest(http.MethodPost, "/api/issues/72001/retriage/ack", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for Sec-Fetch-Site=cross-site, got %d", rec.Code)
	}

	// 2. Cross-origin Origin header (and Origin: null) is rejected with 403
	for _, badOrigin := range []string{"https://evil.example", "null"} {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8090/api/issues/72001/retriage/ack", bytes.NewBufferString(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", badOrigin)
		w := httptest.NewRecorder()
		rwSrv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for Origin=%q, got %d", badOrigin, w.Code)
		}
	}

	// 3. HTML form Content-Type (simple cross-origin POST) is rejected with 403
	req = httptest.NewRequest(http.MethodPost, "/api/issues/72001/retriage/ack", bytes.NewBufferString(`a=1`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for form-urlencoded POST, got %d", rec.Code)
	}

	// 4. Same-origin Origin + application/json succeeds with 200
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8090/api/issues/72001/retriage/ack", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Origin", "http://127.0.0.1:8090")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-origin JSON POST, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. Chunked unknown-length body (ContentLength == -1) without application/json is rejected with 403
	req = httptest.NewRequest(http.MethodPost, "/api/issues/72001/retriage/ack", bytes.NewBufferString(`a=1`))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for chunked POST without application/json Content-Type, got %d", rec.Code)
	}

	// 6. Malformed JSON on optional-body endpoint (/exclude) returns 400 Bad Request instead of silently ignoring
	req = httptest.NewRequest(http.MethodPost, "/api/issues/72001/exclude", bytes.NewBufferString(`{"reason":`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON on /exclude, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrapJSHtmlCommentAndScriptBreakoutEscaping(t *testing.T) {
	rwSrv := setupTestServer(t, false)
	payload := `{
		"cat": "MEDIUM",
		"severity": "MAJOR",
		"subsystem": "statistics",
		"note": {
			"zh-CN": "包含 <!-- <script> 与 </SCRIPT> 标签",
			"en": "Contains <!-- <script> and </SCRIPT> sequence"
		}
	}`
	req := httptest.NewRequest(http.MethodPatch, "/api/issues/72001/triage", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH triage failed: %d %s", rec.Code, rec.Body.String())
	}

	reqIdx := httptest.NewRequest(http.MethodGet, "/", nil)
	recIdx := httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(recIdx, reqIdx)
	if recIdx.Code != http.StatusOK {
		t.Fatalf("GET / failed: %d", recIdx.Code)
	}
	html := recIdx.Body.String()
	if strings.Contains(html, "Contains <!--") || strings.Contains(html, "</SCRIPT>") {
		t.Fatalf("expected HTML comment opener and closing script sequence to be escaped in inline bundle, got unescaped sequence")
	}
}
