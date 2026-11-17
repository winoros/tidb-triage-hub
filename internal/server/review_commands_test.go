package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

func TestHTTPReviewVersionsProtectAllMutationAdapters(t *testing.T) {
	for _, action := range []string{"override", "reset", "ack", "ack_batch", "exclude", "batch"} {
		t.Run(action, func(t *testing.T) {
			srv := setupTestServer(t, false)
			ds, err := srv.store.BuildDataset(true)
			if err != nil {
				t.Fatal(err)
			}
			var original model.IssueRecord
			for _, r := range ds.Records {
				if r.Number == 72002 {
					original = r
				}
			}
			if original.Version == nil {
				t.Fatal("missing review version")
			}
			// A GitHub change not represented by the old triaged_at guard.
			_, err = srv.store.ApplyIssueSync(issuesync.SyncPlan{UpdatedIssues: []issuesync.LiveIssueUpdate{{Number: original.Number, Title: "changed evidence", Body: "new reproducer", HasBody: true, Author: original.Author, UpdatedAt: original.UpdatedAt, Labels: original.Labels}}})
			if err != nil {
				t.Fatal(err)
			}
			method, path := http.MethodPost, "/api/issues/72002/retriage/ack"
			payload := map[string]any{"expected_version": original.Version, "if_unmodified_since": "2999-01-01T00:00:00Z"}
			switch action {
			case "override":
				method, path = http.MethodPatch, "/api/issues/72002/triage"
				payload["cat"], payload["severity"], payload["subsystem"] = "HARD", "MAJOR", "logical_rewrite"
			case "reset":
				method, path = http.MethodDelete, "/api/issues/72002/triage"
			case "exclude":
				path = "/api/issues/72002/exclude"
				payload["reason"] = "stale exclusion"
			case "ack_batch":
				path = "/api/retriage/ack-batch"
				payload = map[string]any{"numbers": []int{72002}, "expected_versions": map[string]any{"72002": original.Version}}
			case "batch":
				path = "/api/triage/decisions"
				payload = map[string]any{"sync_new_prs": false, "decisions": map[string]any{"ack_retriage": []int{72002}, "expected_versions": map[string]any{"72002": original.Version}}}
			}
			raw, _ := json.Marshal(payload)
			req := httptest.NewRequest(method, path, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Triage-CSRF", "1")
			response := httptest.NewRecorder()
			serveTestRequest(srv, response, req)
			if response.Code != http.StatusConflict {
				t.Fatalf("stale %s: HTTP %d %s", action, response.Code, response.Body.String())
			}
		})
	}
}

func TestHTTPPendingAndRestoreKeepReviewedVersions(t *testing.T) {
	srv := setupTestServer(t, false)
	pc := model.PendingCandidate{Number: 73000, Title: "pending issue", Body: "old evidence", UpdatedAt: "2020-01-01T00:00:00Z", Labels: []string{"type/bug"}}
	if _, err := srv.store.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	old, _ := srv.store.ListPendingCandidates()
	pc.Body = "new evidence"
	if _, err := srv.store.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Triage-CSRF", "1")
		resp := httptest.NewRecorder()
		serveTestRequest(srv, resp, req)
		return resp
	}
	resp := request(http.MethodPost, "/api/pending/73000/resolve", model.PendingResolveRequest{Action: "exclude", ExpectedVersion: old[0].Version})
	if resp.Code != http.StatusConflict {
		t.Fatalf("old pending version accepted: %d %s", resp.Code, resp.Body.String())
	}
	fresh, _ := srv.store.ListPendingCandidates()
	resp = request(http.MethodPost, "/api/pending/73000/resolve", model.PendingResolveRequest{Action: "exclude", ExpectedVersion: fresh[0].Version})
	if resp.Code != http.StatusOK {
		t.Fatalf("fresh pending version rejected: %d %s", resp.Code, resp.Body.String())
	}
	excluded, _ := srv.store.ListExcludedIssues()
	if len(excluded) != 1 || excluded[0].Version == nil {
		t.Fatal("missing excluded version")
	}
	resp = request(http.MethodDelete, "/api/excluded/73000", model.ReviewPrecondition{ExpectedVersion: fresh[0].Version})
	if resp.Code != http.StatusConflict {
		t.Fatalf("old incarnation restored: %d %s", resp.Code, resp.Body.String())
	}
	resp = request(http.MethodDelete, "/api/excluded/73000", model.ReviewPrecondition{ExpectedVersion: excluded[0].Version})
	if resp.Code != http.StatusOK {
		t.Fatalf("fresh restore failed: %d %s", resp.Code, resp.Body.String())
	}
}

func TestHTTPReviewSnapshotAndErrorContract(t *testing.T) {
	srv := setupTestServer(t, false)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("X-Triage-CSRF", "1")
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		serveTestRequest(srv, resp, req)
		return resp
	}
	resp := request(http.MethodGet, "/api/state", "")
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"snapshot_revision", "dataset", "pending_candidates", "excluded_issues"} {
		if _, ok := snapshot[key]; !ok {
			t.Fatalf("state omitted %s", key)
		}
	}
	for _, body := range []string{`{"numbers":[0]}`, `{"numbers":[72002],"expected_versions":{"72002":{"review":1,"notes":1},"#72002":{"review":1,"notes":1}}}`} {
		resp = request(http.MethodPost, "/api/retriage/ack-batch", body)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("invalid ack returned %d: %s", resp.Code, resp.Body.String())
		}
	}
	// Fail only the post-commit snapshot by corrupting a pending payload from a
	// transaction trigger. The decision itself must remain committed and visible.
	if _, err := srv.store.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{{Number: 99900, Title: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", srv.store.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER test_snapshot_failure AFTER INSERT ON issue_overrides BEGIN UPDATE pending_candidates SET payload_json='invalid' WHERE number=99900; END;`); err != nil {
		t.Fatal(err)
	}
	resp = request(http.MethodPatch, "/api/issues/72002/triage", `{"cat":"HARD","severity":"MAJOR","subsystem":"logical_rewrite"}`)
	if resp.Code != http.StatusInternalServerError || !bytes.Contains(resp.Body.Bytes(), []byte(`"committed":true`)) {
		t.Fatalf("postcommit error misreported: %d %s", resp.Code, resp.Body.String())
	}
	var cat string
	if err := db.QueryRow(`SELECT cat FROM issue_overrides WHERE number=72002`).Scan(&cat); err != nil || cat != "HARD" {
		t.Fatalf("decision missing: %q %v", cat, err)
	}
	if err := srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	resp = request(http.MethodPatch, "/api/issues/72002/triage", `{"cat":"HARD","severity":"MAJOR","subsystem":"logical_rewrite"}`)
	if resp.Code != http.StatusInternalServerError || bytes.Contains(resp.Body.Bytes(), []byte(`"committed":true`)) {
		t.Fatalf("storage error misreported: %d %s", resp.Code, resp.Body.String())
	}
}
