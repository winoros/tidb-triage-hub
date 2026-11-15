package server

import (
	"bytes"
	"context"
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

type mergedPRGraphQLRunner struct{}

func (m *mergedPRGraphQLRunner) Query(_ context.Context, gql string, _ map[string]string) ([]byte, error) {
	if strings.Contains(gql, "...PRStatusFields") {
		return []byte(`{
			"data": {
				"rateLimit": {"limit":5000,"cost":1,"remaining":4999,"resetAt":"2026-11-12T13:00:00Z"},
				"repository": {
					"p88901": {
						"number": 88901,
						"title": "planner: pre-merged fix PR",
						"url": "https://github.com/pingcap/tidb/pull/88901",
						"baseRefName": "master",
						"state": "MERGED",
						"isDraft": false,
						"mergeable": "UNKNOWN",
						"createdAt": "2026-11-12T09:00:00Z",
						"updatedAt": "2026-11-12T12:00:00Z",
						"author": {"login": "winoros"},
						"labels": {"nodes": [{"name": "sig/planner"}]}
					}
				}
			}
		}`), nil
	}
	return []byte(`{"data":{"rateLimit":{"limit":5000,"cost":0,"remaining":5000,"resetAt":"2026-11-12T13:00:00Z"}}}`), nil
}

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

func TestTokenAuthCookieBearerAndEmbeddedWebAssets(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seed := model.Dataset{
		SyncMeta: model.SyncMeta{
			SchemaVersion:      2,
			SnapshotUTC:        "2026-11-12T10:00:00Z",
			MaxIssueUpdatedUTC: "2026-11-12T09:50:00Z",
		},
		Enums: model.DatasetEnums{
			Categories: model.ValidCategories,
			Severities: model.ValidSeverities,
			Subsystems: model.ValidSubsystems,
		},
		Records: []model.IssueRecord{
			{
				Number:      72001,
				Title:       "planner: test auth issue",
				Author:      "alice",
				Created:     "2026-11-01",
				CreatedAt:   "2026-11-01T00:00:00Z",
				UpdatedAt:   "2026-11-12T09:50:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				GHSeverity:  "major",
				Subsystem:   "logical_rewrite",
			},
		},
	}
	if err := st.IngestSyncedDataset(seed, []model.PendingCandidate{
		{
			Number:    72100,
			Title:     "planner: pending candidate hidden from anonymous viewers",
			Author:    "bob",
			CreatedAt: "2026-11-12T10:01:00Z",
			UpdatedAt: "2026-11-12T10:02:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Empty WebDir exercises embedded webassets.FS!
	srv := New(st, syncer.New(st), Options{
		WebDir:    filepath.Join(tmp, "nonexistent_web_dir"),
		AuthToken: "secret-maint-token",
	})

	// 1. Anonymous GET / renders embedded index.html in viewer mode (can_triage:false, pending:[])
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET / with embedded assets, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"syncPrsBtn",
		"syncIssuesBtn",
		"authUnlockBtn",
		`"can_triage":false`,
		`"auth_required":true`,
		`window.TRIAGE_PENDING = [];`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected anonymous GET / to contain %q", want)
		}
	}

	// Embedded i18n static fallback works too
	reqI18n := httptest.NewRequest(http.MethodGet, "/i18n/zh-CN.js", nil)
	recI18n := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recI18n, reqI18n)
	if recI18n.Code != http.StatusOK || !strings.Contains(recI18n.Body.String(), "unlockBtn") {
		t.Fatalf("expected embedded /i18n/zh-CN.js 200 OK, got %d", recI18n.Code)
	}

	// 2. Unauthenticated mutations & protected endpoints return 403 Forbidden
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/api/issues/72001/triage"},
		{http.MethodGet, "/api/triage/pending"},
		{http.MethodPost, "/api/triage/decisions"},
		{http.MethodGet, "/api/admin/backup.db"},
		{http.MethodPost, "/api/sync/prs"},
		{http.MethodPost, "/api/sync/issues"},
	} {
		var rBody *bytes.Buffer
		if tc.method == http.MethodGet {
			rBody = bytes.NewBuffer(nil)
		} else {
			rBody = bytes.NewBufferString(`{}`)
		}
		r := httptest.NewRequest(tc.method, tc.path, rBody)
		if tc.method != http.MethodGet {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for unauthenticated %s %s, got %d", tc.method, tc.path, w.Code)
		}
	}

	// 3. Invalid token login -> 401; valid token login -> sets HttpOnly SameSite=Lax cookie & unlocks RW
	badLogin := httptest.NewRequest(http.MethodPost, "/api/session/login", bytes.NewBufferString(`{"token":"wrong"}`))
	badLogin.Header.Set("Content-Type", "application/json")
	badRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(badRec, badLogin)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid login token, got %d", badRec.Code)
	}

	// Anonymous GET /api/status redacts db_path & pending_numbers; anonymous check_gh=1 returns 401; bad Bearer returns 401
	anonStatReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	anonStatRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(anonStatRec, anonStatReq)
	if anonStatRec.Code != http.StatusOK || !strings.Contains(anonStatRec.Body.String(), `"db_path":"redacted"`) || strings.Contains(anonStatRec.Body.String(), "72100") {
		t.Fatalf("expected redacted anonymous /api/status, got code=%d body=%s", anonStatRec.Code, anonStatRec.Body.String())
	}
	anonGHReq := httptest.NewRequest(http.MethodGet, "/api/status?check_gh=1", nil)
	anonGHRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(anonGHRec, anonGHReq)
	if anonGHRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous /api/status?check_gh=1 when token auth enabled, got %d", anonGHRec.Code)
	}
	badBearerStatReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	badBearerStatReq.Header.Set("Authorization", "Bearer wrong-token")
	badBearerStatRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(badBearerStatRec, badBearerStatReq)
	if badBearerStatRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid Bearer token on /api/status, got %d", badBearerStatRec.Code)
	}

	// Untrusted external RemoteAddr cannot spoof X-Forwarded-Proto to force Secure cookie over HTTP
	extLogin := httptest.NewRequest(http.MethodPost, "http://internal:8787/api/session/login", bytes.NewBufferString(`{"token":"secret-maint-token"}`))
	extLogin.RemoteAddr = "203.0.113.9:54321"
	extLogin.Header.Set("Content-Type", "application/json")
	extLogin.Header.Set("X-Forwarded-Proto", "https")
	extLoginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(extLoginRec, extLogin)
	if extLoginRec.Code != http.StatusOK || extLoginRec.Result().Cookies()[0].Secure {
		t.Fatalf("expected untrusted peer X-Forwarded-Proto to be ignored (Secure=false), got %+v", extLoginRec.Result().Cookies())
	}

	// Trusted loopback reverse proxy sets Secure cookie via X-Forwarded-Proto
	okLogin := httptest.NewRequest(http.MethodPost, "/api/session/login", bytes.NewBufferString(`{"token":"secret-maint-token"}`))
	okLogin.RemoteAddr = "127.0.0.1:54321"
	okLogin.Header.Set("Content-Type", "application/json")
	okLogin.Header.Set("X-Forwarded-Proto", "https, http")
	okRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(okRec, okLogin)
	if okRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid login token, got %d: %s", okRec.Code, okRec.Body.String())
	}
	cookies := okRec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected Secure HttpOnly SameSiteLax cookie, got %+v", cookies)
	}

	// Untrusted external peer cannot spoof X-Forwarded-Host to bypass Origin CSRF guard
	spoofPatch := httptest.NewRequest(http.MethodPatch, "http://internal:8787/api/issues/72001/triage", bytes.NewBufferString(`{"cat":"HARD","severity":"CRITICAL","subsystem":"logical_rewrite"}`))
	spoofPatch.RemoteAddr = "203.0.113.9:54321"
	spoofPatch.Header.Set("Content-Type", "application/json")
	spoofPatch.Header.Set("Origin", "https://evil.example")
	spoofPatch.Header.Set("X-Forwarded-Host", "evil.example")
	spoofPatch.AddCookie(cookies[0])
	spoofRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(spoofRec, spoofPatch)
	if spoofRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when untrusted peer spoofs X-Forwarded-Host, got %d", spoofRec.Code)
	}

	// Cookie unlocks browser PATCH behind trusted loopback X-Forwarded-Host reverse proxy
	patchReq := httptest.NewRequest(http.MethodPatch, "http://internal:8787/api/issues/72001/triage", bytes.NewBufferString(`{"cat":"HARD","severity":"CRITICAL","subsystem":"logical_rewrite","note":{"zh-CN":"已校准","en":"Calibrated"}}`))
	patchReq.RemoteAddr = "127.0.0.1:54321"
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Origin", "https://hub.example.com")
	patchReq.Header.Set("X-Forwarded-Proto", "https")
	patchReq.Header.Set("X-Forwarded-Host", "hub.example.com, internal:8787")
	patchReq.AddCookie(cookies[0])
	patchRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for cookie-authenticated PATCH, got %d: %s", patchRec.Code, patchRec.Body.String())
	}

	// 4. Bearer token unlocks Machine B endpoints (/api/triage/pending & /api/admin/backup.db)
	pendReq := httptest.NewRequest(http.MethodGet, "/api/triage/pending?compact=1&enrich_prs=0", nil)
	pendReq.Header.Set("Authorization", "Bearer secret-maint-token")
	pendRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(pendRec, pendReq)
	if pendRec.Code != http.StatusOK || !strings.Contains(pendRec.Body.String(), `"pending_count":1`) {
		t.Fatalf("expected 200 with pending_count=1 for Bearer request, got %d: %s", pendRec.Code, pendRec.Body.String())
	}

	bakReq := httptest.NewRequest(http.MethodGet, "/api/admin/backup.db", nil)
	bakReq.Header.Set("Authorization", "Bearer secret-maint-token")
	bakRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bakRec, bakReq)
	if bakRec.Code != http.StatusOK || !bytes.HasPrefix(bakRec.Body.Bytes(), []byte("SQLite format 3")) {
		t.Fatalf("expected valid SQLite header from /api/admin/backup.db, got code=%d len=%d", bakRec.Code, bakRec.Body.Len())
	}

	// 5. Logout clears cookie
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/session/logout", bytes.NewBufferString(`{}`))
	logoutReq.Header.Set("Content-Type", "application/json")
	logoutRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK || !strings.Contains(logoutRec.Body.String(), `"can_triage":false`) {
		t.Fatalf("expected logged-out session, got %d: %s", logoutRec.Code, logoutRec.Body.String())
	}
}

func TestRound2OriginDowngradeBasicAuthCoexistenceAndConflictCodes(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "r2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Enums: model.DatasetEnums{
			Categories: model.ValidCategories,
			Severities: model.ValidSeverities,
			Subsystems: model.ValidSubsystems,
		},
		Records: []model.IssueRecord{
			{
				Number:      72001,
				Title:       "planner: test conflict & proxy headers",
				Author:      "alice",
				Created:     "2026-11-01",
				CreatedAt:   "2026-11-01T00:00:00Z",
				UpdatedAt:   "2026-11-12T09:50:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				GHSeverity:  "major",
				Subsystem:   "logical_rewrite",
			},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	srv := New(st, syncer.New(st), Options{
		AuthToken:  "maint-tok",
		TrustProxy: true,
	})

	// 1. GET /api/status?require_auth=1 returns 401 without token
	reqStat := httptest.NewRequest(http.MethodGet, "/api/status?require_auth=1", nil)
	recStat := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recStat, reqStat)
	if recStat.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /api/status?require_auth=1, got %d", recStat.Code)
	}

	// ReadOnly server returns 200 with gh_check_error on check_gh=1 instead of hitting GitHub
	roSrv := New(st, syncer.New(st), Options{ReadOnly: true})
	roStatReq := httptest.NewRequest(http.MethodGet, "/api/status?check_gh=1", nil)
	roStatRec := httptest.NewRecorder()
	roSrv.Handler().ServeHTTP(roStatRec, roStatReq)
	if roStatRec.Code != http.StatusOK || !strings.Contains(roStatRec.Body.String(), `"gh_check_error"`) {
		t.Fatalf("expected 200 with gh_check_error on readonly check_gh=1, got %d: %s", roStatRec.Code, roStatRec.Body.String())
	}

	// Login to obtain session cookie
	loginReq := httptest.NewRequest(http.MethodPost, "http://hub.example.com/api/session/login", bytes.NewBufferString(`{"token":"maint-tok"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Origin", "https://hub.example.com")
	loginReq.Header.Set("X-Forwarded-Proto", "https")
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login failed: %d", loginRec.Code)
	}
	cookie := loginRec.Result().Cookies()[0]

	// 2. Origin: http://... downgrade against HTTPS endpoint is blocked with 403
	downReq := httptest.NewRequest(http.MethodPost, "http://hub.example.com/api/issues/72001/retriage/ack", bytes.NewBufferString(`{}`))
	downReq.Header.Set("Content-Type", "application/json")
	downReq.Header.Set("X-Forwarded-Proto", "https")
	downReq.Header.Set("Origin", "http://hub.example.com")
	downReq.AddCookie(cookie)
	downRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(downRec, downReq)
	if downRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on http->https Origin downgrade, got %d", downRec.Code)
	}

	// 3. Reverse-proxy Basic Auth header coexists with session cookie AND default :443 port in Origin is normalized
	basicPatch := httptest.NewRequest(http.MethodPatch, "http://hub.example.com/api/issues/72001/triage", bytes.NewBufferString(`{
		"cat":"HARD","severity":"CRITICAL","subsystem":"logical_rewrite",
		"note":{"zh-CN":"Basic认证代理下Cookie正常工作","en":"Cookie works alongside reverse-proxy Basic Auth"}
	}`))
	basicPatch.Header.Set("Content-Type", "application/json")
	basicPatch.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	basicPatch.Header.Set("X-Forwarded-Proto", "https")
	basicPatch.Header.Set("Origin", "https://hub.example.com:443")
	basicPatch.AddCookie(cookie)
	basicRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(basicRec, basicPatch)
	if basicRec.Code != http.StatusOK {
		t.Fatalf("expected 200 with Basic Auth + session cookie + :443 Origin, got %d: %s", basicRec.Code, basicRec.Body.String())
	}

	// 4. Stale if_unmodified_since on PATCH returns 409 Conflict; unknown issue returns 404 Not Found
	conflictPatch := httptest.NewRequest(http.MethodPatch, "/api/issues/72001/triage", bytes.NewBufferString(`{
		"cat":"EASY","severity":"MINOR","subsystem":"statistics",
		"if_unmodified_since":"2020-01-01T00:00:00Z"
	}`))
	conflictPatch.Header.Set("Content-Type", "application/json")
	conflictPatch.Header.Set("Authorization", "Bearer maint-tok")
	conflictRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(conflictRec, conflictPatch)
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on stale if_unmodified_since PATCH, got %d: %s", conflictRec.Code, conflictRec.Body.String())
	}

	notFoundPatch := httptest.NewRequest(http.MethodPatch, "/api/issues/999999/triage", bytes.NewBufferString(`{
		"cat":"EASY","severity":"MINOR","subsystem":"statistics"
	}`))
	notFoundPatch.Header.Set("Content-Type", "application/json")
	notFoundPatch.Header.Set("Authorization", "Bearer maint-tok")
	notFoundRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(notFoundRec, notFoundPatch)
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found on unknown issue PATCH, got %d: %s", notFoundRec.Code, notFoundRec.Body.String())
	}

	// 5. POST /api/triage/decisions returns 409 Conflict when skip_conflicts=false hits a stale override
	batchConflictReq := httptest.NewRequest(http.MethodPost, "/api/triage/decisions", bytes.NewBufferString(`{
		"skip_conflicts": false,
		"decisions": {
			"overrides": {
				"72001": {
					"cat": "EASY",
					"severity": "MINOR",
					"subsystem": "statistics",
					"if_unmodified_since": "2020-01-01T00:00:00Z",
					"note": {"zh-CN": "严格冲突检测返回409状态码", "en": "Strict conflict check returns HTTP 409"}
				}
			}
		}
	}`))
	batchConflictReq.Header.Set("Content-Type", "application/json")
	batchConflictReq.Header.Set("Authorization", "Bearer maint-tok")
	batchConflictRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(batchConflictRec, batchConflictReq)
	if batchConflictRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on strict batch conflict, got %d: %s", batchConflictRec.Code, batchConflictRec.Body.String())
	}
}

func TestBackupTimeoutHeaderAndPreMutationPRFetch(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "pre_fetch.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records: []model.IssueRecord{{
			Number:    88001,
			Title:     "planner: issue with uncached merged PR",
			Author:    "alice",
			Created:   "2026-11-12",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:00:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			Cat:       "MEDIUM",
			Severity:  "MAJOR",
			Subsystem: "logical_rewrite",
			Note:      &model.LocalizedNote{ZhCN: "前置PR同步消除瞬间抖动", En: "Pre-mutation PR fetch avoids post-commit window"},
			PRs: []model.LinkedPRRef{{
				Repo:  "pingcap/tidb",
				Num:   88901,
				Label: "#88901",
				URL:   "https://github.com/pingcap/tidb/pull/88901",
				State: "OPEN",
			}},
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	sy := syncer.NewWithRunner(st, &mergedPRGraphQLRunner{})
	srv := New(st, sy, Options{AuthToken: "maint-tok"})

	// 1. PATCH /api/issues/88001/triage pre-fetches #88901=MERGED BEFORE baseline snapshot -> needs_retriage=false
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/issues/88001/triage", bytes.NewBufferString(`{
		"cat": "HARD",
		"severity": "MAJOR",
		"subsystem": "logical_rewrite",
		"note": {"zh-CN": "更新分类时前置同步已合并PR", "en": "Pre-fetch merged PR before override snapshot"}
	}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Authorization", "Bearer maint-tok")
	patchRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on PATCH, got %d: %s", patchRec.Code, patchRec.Body.String())
	}
	var patchOut struct {
		Record model.IssueRecord `json:"record"`
	}
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patchOut); err != nil {
		t.Fatal(err)
	}
	if patchOut.Record.NeedsRetriage {
		t.Fatalf("expected pre-fetched MERGED PR #88901 not to self-flag needs_retriage, got reasons=%v", patchOut.Record.RetriageReasons)
	}
	if len(patchOut.Record.PRs) != 1 || patchOut.Record.PRs[0].State != "MERGED" {
		t.Fatalf("expected PR #88901 hydrated as MERGED on record, got %+v", patchOut.Record.PRs)
	}

	// 2. GET /api/admin/backup.db honors X-Triage-Timeout-Ms up to 30m
	bakReq := httptest.NewRequest(http.MethodGet, "/api/admin/backup.db", nil)
	bakReq.Header.Set("Authorization", "Bearer maint-tok")
	bakReq.Header.Set("X-Triage-Timeout-Ms", "600000")
	bakRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bakRec, bakReq)
	if bakRec.Code != http.StatusOK || bakRec.Body.Len() < 100 {
		t.Fatalf("expected 200 OK SQLite snapshot with X-Triage-Timeout-Ms=600000, got code=%d len=%d", bakRec.Code, bakRec.Body.Len())
	}
}
