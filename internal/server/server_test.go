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
			TotalCandidates:              1,
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

	sy := syncer.New(st, filepath.Join(tmp, "scripts"), filepath.Join(tmp, "staging"))
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

	// 4. RW PATCH succeeds
	patchBody := `{"cat":"EASY","severity":"CRITICAL","subsystem":"logical_rewrite","note":{"zh-CN":"已确认","en":"Confirmed"}}`
	req = httptest.NewRequest(http.MethodPatch, "/api/issues/72001/triage", bytes.NewBufferString(patchBody))
	rec = httptest.NewRecorder()
	rwSrv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("RW PATCH failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5. ReadOnly mode blocks mutations with 403 Forbidden
	roSrv := setupTestServer(t, true)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/api/issues/72001/triage"},
		{http.MethodDelete, "/api/issues/72001/triage"},
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
