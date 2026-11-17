package syncer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

type evidenceCountingRunner struct{ calls int }

func (r *evidenceCountingRunner) Query(ctx context.Context, q string, a map[string]string) ([]byte, error) {
	if strings.Contains(q, "files(first:") {
		r.calls++
	}
	raw, err := (&stubBundleRunner{}).Query(ctx, q, a)
	if err != nil || !strings.Contains(q, "files(first:") {
		return raw, err
	}
	var response map[string]any
	if err = json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	pr := response["data"].(map[string]any)["repository"].(map[string]any)["p96901"].(map[string]any)
	pr["headRefOid"] = "head"
	pr["baseRefOid"] = "base"
	pr["updatedAt"] = "2026-11-16T01:00:00Z"
	return json.Marshal(response)
}
func TestRepeatedBundleReusesEvidenceAndReadOnlyExportsPersistedSnapshot(t *testing.T) {
	for _, mode := range []string{"pending", "backfill"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "export.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			prs := []model.LinkedPRRef{{Repo: store.DefaultRepo, Num: 96901, State: "OPEN"}}
			rec := model.IssueRecord{Number: 1, Title: "one", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite"}
			pending := []model.PendingCandidate{{Number: 42, Title: "pending", PRs: prs}}
			opts := PendingBundleOptions{OnlyPending: true, EnrichPRs: true}
			if mode == "backfill" {
				rec.PRs = prs
				pending = nil
				opts = PendingBundleOptions{BackfillNotes: 10, EnrichPRs: true}
			}
			if err := st.IngestSyncedDataset(model.Dataset{Records: []model.IssueRecord{rec}}, pending); err != nil {
				t.Fatal(err)
			}
			ro, err := store.Open("file:" + st.DBPath() + "?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			r := &evidenceCountingRunner{}
			// Missing evidence must not make a read-only export fetch or write.
			bundle, count, err := NewWithRunner(ro, r).BuildPendingBundle(context.Background(), opts)
			if err != nil || count != 1 {
				t.Fatalf("read-only export: count=%d err=%v", count, err)
			}
			if r.calls != 0 || bundle["enrichment_warning"] == nil {
				t.Fatalf("read-only refresh: calls=%d warning=%v", r.calls, bundle["enrichment_warning"])
			}
			sy := NewWithRunner(st, r)
			for i := 0; i < 2; i++ {
				if _, count, err = sy.BuildPendingBundle(context.Background(), opts); err != nil || count != 1 {
					t.Fatalf("export: count=%d err=%v", count, err)
				}
			}
			if r.calls != 1 {
				t.Fatalf("matching evidence was fetched again: %d", r.calls)
			}
			for _, enrich := range []bool{true, false} {
				opts.EnrichPRs = enrich
				if bundle, count, err = NewWithRunner(ro, r).BuildPendingBundle(context.Background(), opts); err != nil || count != 1 {
					t.Fatalf("persisted read-only export: count=%d err=%v", count, err)
				}
				if bundle["enrichment_warning"] != nil {
					t.Fatalf("cached evidence produced warning: %v", bundle["enrichment_warning"])
				}
			}
			if r.calls != 1 {
				t.Fatal("read-only export accessed GitHub")
			}
		})
	}
}

func TestExportsSkipFailedLookupsUntilPositiveObservation(t *testing.T) {
	for _, mode := range []string{"pending", "backfill"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "failed.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ref := model.LinkedPRRef{Repo: store.DefaultRepo, Num: 96901, State: "UNKNOWN"}
			rec := model.IssueRecord{Number: 1, Title: "one", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite", PRs: []model.LinkedPRRef{ref}}
			if err := st.IngestSyncedDataset(model.Dataset{Records: []model.IssueRecord{rec}}, []model.PendingCandidate{{Number: 42, Title: "pending", PRs: []model.LinkedPRRef{ref}}}); err != nil {
				t.Fatal(err)
			}
			if err := st.ApplyPRFetchResults(nil, []prstatus.PRTarget{{Repo: ref.Repo, Number: ref.Num}}, time.Time{}); err != nil {
				t.Fatal(err)
			}
			opts := PendingBundleOptions{OnlyPending: true, EnrichPRs: true}
			if mode == "backfill" {
				opts = PendingBundleOptions{BackfillNotes: 10, EnrichPRs: true}
			}
			r := &evidenceCountingRunner{}
			sy := NewWithRunner(st, r)
			for i := 0; i < 2; i++ {
				if _, count, err := sy.BuildPendingBundle(context.Background(), opts); err != nil || count != 1 {
					t.Fatalf("export count=%d err=%v", count, err)
				}
			}
			if r.calls != 0 {
				t.Fatalf("failed lookup retried %d times", r.calls)
			}
			ro, err := store.Open("file:" + st.DBPath() + "?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			bundle, _, err := NewWithRunner(ro, r).BuildPendingBundle(context.Background(), opts)
			if err != nil || bundle["enrichment_warning"] != nil {
				t.Fatalf("skipped failed lookup should not need enrichment: %v %v", bundle, err)
			}
			// An explicit successful sync clears the marker and enables enrichment.
			if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: ref.Repo, Num: ref.Num, State: "OPEN"}}, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := sy.BuildPendingBundle(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			if r.calls != 1 {
				t.Fatalf("positive observation did not re-enable enrichment: %d", r.calls)
			}
		})
	}
}

func TestExportReadOnlyWriteFailureUsesPersistedSnapshot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "readonly.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pc := model.PendingCandidate{Number: 42, Title: "pending", PRs: []model.LinkedPRRef{{Repo: store.DefaultRepo, Num: 96901, State: "OPEN"}}}
	if err := st.IngestSyncedDataset(model.Dataset{Records: []model.IssueRecord{{Number: 1, Title: "one", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "statistics"}}}, []model.PendingCandidate{pc}); err != nil {
		t.Fatal(err)
	}
	// SQLite itself rejects writes, without the explicit URI mode=ro hint.
	queryOnly, err := store.Open("file:" + st.DBPath() + "?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer queryOnly.Close()
	if queryOnly.ReadOnly() {
		t.Fatal("test must exercise write-time detection")
	}
	r := &evidenceCountingRunner{}
	sy := NewWithRunner(queryOnly, r)
	plain, _, err := sy.BuildPendingBundle(context.Background(), PendingBundleOptions{OnlyPending: true})
	if err != nil {
		t.Fatal(err)
	}
	bundle, count, err := sy.BuildPendingBundle(context.Background(), PendingBundleOptions{OnlyPending: true, EnrichPRs: true})
	if err != nil || count != 1 {
		t.Fatalf("export failed: count=%d err=%v", count, err)
	}
	if r.calls != 1 || bundle["enrichment_warning"] == nil {
		t.Fatalf("write failure not degraded: calls=%d warning=%v", r.calls, bundle["enrichment_warning"])
	}
	want, _ := json.Marshal(plain["pending_candidates"])
	got, _ := json.Marshal(bundle["pending_candidates"])
	if string(want) != string(got) {
		t.Fatalf("unpersisted evidence leaked: %s != %s", want, got)
	}
	// No PRs require enrichment in the backfill branch, so no warning is needed.
	bundle, count, err = sy.BuildPendingBundle(context.Background(), PendingBundleOptions{BackfillNotes: 10, EnrichPRs: true})
	if err != nil || count != 1 || bundle["enrichment_warning"] != nil {
		t.Fatalf("no-PR export: count=%d warning=%v err=%v", count, bundle["enrichment_warning"], err)
	}
}

type changedBodyRunner struct{ calls int }

func (r *changedBodyRunner) Query(context.Context, string, map[string]string) ([]byte, error) {
	r.calls++
	return []byte(`{"data":{"repository":{"p12345":{"number":12345,"state":"OPEN","updatedAt":"2026-11-17T01:00:00Z","headRefOid":"h","baseRefOid":"b","body":"Corrected cause: slow query","files":{"nodes":[{"path":"pkg/planner/plan.go"}]}}}}}`), nil
}
func TestExportRefreshesChangedBodyOnUnchangedCommits(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := model.PendingPRDetail{Repo: store.DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: "h", BaseOID: "b", BodyExcerpt: "Old root cause: wrong result", NonTestFiles: []string{"pkg/planner/plan.go"}}
	p := (model.LinkedPRRef{Repo: d.Repo, Num: d.Num}).WithFacts(d.Facts())
	if err := st.IngestSyncedDataset(model.Dataset{}, []model.PendingCandidate{{Number: 42, Title: "planner issue", PRs: []model.LinkedPRRef{p}, PRDetails: []model.PendingPRDetail{d}}}); err != nil {
		t.Fatal(err)
	}
	status := d.Facts()
	status.UpdatedAt = "2026-11-17T01:00:00Z"
	if err := st.UpsertPRStatuses([]model.PRStatus{status}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(before[0].PRDetails) != 1 || before[0].PRDetails[0].BodyExcerpt != "" || len(before[0].PRDetails[0].NonTestFiles) == 0 {
		t.Fatalf("stale prose or lost files before refresh: %+v", before[0])
	}
	runner := &changedBodyRunner{}
	bundle, _, err := NewWithRunner(st, runner).BuildPendingBundle(context.Background(), PendingBundleOptions{OnlyPending: true, EnrichPRs: true})
	if err != nil {
		t.Fatal(err)
	}
	pc := bundle["pending_candidates"].([]model.PendingCandidate)[0]
	t.Logf("requests=%d exported body=%q clues=%+v", runner.calls, pc.PRDetails[0].BodyExcerpt, pc.Clues)
	if runner.calls != 1 || pc.PRDetails[0].BodyExcerpt != "Corrected cause: slow query" {
		t.Fatal("export did not refresh corrected root cause")
	}
	for _, signature := range pc.Clues.ErrorSignatures {
		if signature == "wrong_result" {
			t.Fatal("old prose leaked into clues")
		}
	}
	if _, _, err := NewWithRunner(st, runner).BuildPendingBundle(context.Background(), PendingBundleOptions{OnlyPending: true, EnrichPRs: true}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatal("unchanged body was fetched again")
	}
}
