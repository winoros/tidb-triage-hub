package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

func reviewTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var records []model.IssueRecord
	for _, n := range []int{100, 101} {
		records = append(records, model.IssueRecord{Number: n, Title: "planner: initial issue", Author: "alice", CreatedAt: "2020-01-01T00:00:00Z", UpdatedAt: "2020-01-01T00:00:00Z", Labels: []string{"type/bug"}, Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite", Note: &model.LocalizedNote{ZhCN: "原始根因", En: "Original cause"}})
	}
	err = st.IngestSyncedDataset(model.Dataset{SyncMeta: model.SyncMeta{SnapshotUTC: "2020-01-01T00:00:00Z"}, Records: records}, []model.PendingCandidate{{Number: 200, Title: "pending planner bug", Body: "original SQL", UpdatedAt: "2020-01-01T00:00:00Z", Labels: []string{"type/bug"}}})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func reviewRecord(t *testing.T, st *Store, n int) model.IssueRecord {
	t.Helper()
	ds, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ds.Records {
		if r.Number == n {
			if r.Version == nil {
				t.Fatal("missing version")
			}
			return r
		}
	}
	t.Fatalf("missing issue %d", n)
	return model.IssueRecord{}
}

func reviewPatch(cat string, v *model.ReviewVersion) model.TriagePatchRequest {
	return model.TriagePatchRequest{Cat: cat, Severity: "MAJOR", Subsystem: "logical_rewrite", ExpectedVersion: v}
}

func TestReviewVersionsSeparateNotesAndClassification(t *testing.T) {
	st := reviewTestStore(t)
	original := reviewRecord(t, st, 100)
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "Updated cause", ExpectedVersion: original.Version}}}); err != nil {
		t.Fatal(err)
	}
	noted := reviewRecord(t, st, 100)
	if noted.Version.Review != original.Version.Review || noted.Version.Notes <= original.Version.Notes {
		t.Fatalf("notes changed wrong counter: before=%+v after=%+v", original.Version, noted.Version)
	}
	// A classification-only decision can coexist with an independent note edit.
	if err := st.ApplyIssueOverride(100, reviewPatch("HARD", original.Version)); err != nil {
		t.Fatal(err)
	}
	classified := reviewRecord(t, st, 100)
	if classified.Note.En != "Updated cause" || classified.Version.Review <= noted.Version.Review || classified.Version.Notes != noted.Version.Notes {
		t.Fatalf("classification clobbered notes: %+v", classified)
	}
	// Likewise, a note-only edit need not re-review a concurrent classification.
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {ZhCN: "新的中文根因", ExpectedVersion: noted.Version}}}); err != nil {
		t.Fatal(err)
	}
	// But a combined classification+note edit must check both versions.
	combined := reviewPatch("EASY", classified.Version)
	combined.Note = &model.LocalizedNote{ZhCN: "过期根因", En: "Stale cause"}
	if err := st.ApplyIssueOverride(100, combined); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("expected note conflict, got %v", err)
	}
	if r := reviewRecord(t, st, 100); r.Cat != "HARD" || r.Note.ZhCN != "新的中文根因" {
		t.Fatalf("stale combined edit committed: %+v", r)
	}
}

func TestReviewVersionRejectsFactsChangedWithoutRetriageTrigger(t *testing.T) {
	for _, action := range []string{"override", "reset", "ack", "exclude"} {
		t.Run(action, func(t *testing.T) {
			st := reviewTestStore(t)
			before := reviewRecord(t, st, 100)
			// Same GitHub timestamp and unchanged labels: only the review evidence changes.
			_, err := st.ApplyIssueSync(issuesync.SyncPlan{UpdatedIssues: []issuesync.LiveIssueUpdate{{Number: 100, Title: before.Title, Body: "a different SQL reproducer", HasBody: true, Author: before.Author, UpdatedAt: before.UpdatedAt, Labels: before.Labels}}})
			if err != nil {
				t.Fatal(err)
			}
			after := reviewRecord(t, st, 100)
			if after.NeedsRetriage || after.Version.Review <= before.Version.Review {
				t.Fatalf("facts did not advance version independently of triggers: %+v", after)
			}
			guard := model.ReviewPrecondition{ExpectedVersion: before.Version, IfUnmodifiedSince: "2999-01-01T00:00:00Z"}
			switch action {
			case "override":
				err = st.ApplyIssueOverride(100, reviewPatch("HARD", before.Version))
			case "reset":
				err = st.ResetIssueOverrideWithPrecondition(100, guard)
			case "ack":
				_, err = st.AcknowledgeIssueRetriageWithPrecondition(100, "reviewer", guard)
			case "exclude":
				err = st.ExcludeExistingIssueWithPrecondition(100, "old judgment", false, guard)
			}
			if !errors.Is(err, ErrOverrideConflict) {
				t.Fatalf("stale %s accepted: %v", action, err)
			}
			if *reviewRecord(t, st, 100).Version != *after.Version {
				t.Fatal("rejected command changed version")
			}
		})
	}
}

func TestReviewBatchVersionsAtomicSkipAndCompanionWrites(t *testing.T) {
	st := reviewTestStore(t)
	a, b := reviewRecord(t, st, 100), reviewRecord(t, st, 101)
	if err := st.ApplyIssueOverride(101, reviewPatch("HARD", b.Version)); err != nil {
		t.Fatal(err)
	}
	dec := model.BatchTriageDecisions{
		ExpectedVersions: map[string]model.ReviewVersion{"100": *a.Version, "101": *b.Version},
		BaseExportedAt:   "2999-01-01T00:00:00Z", // Cannot override the explicit version check.
		Overrides:        map[string]model.TriagePatchRequest{"100": reviewPatch("EASY", nil), "101": reviewPatch("EASY", nil)},
		ManualPRLinks:    map[string][]string{"101": {"12345"}},
	}
	if _, err := st.ApplyBatchTriageDecisions(dec); !errors.Is(err, ErrBatchConflict) {
		t.Fatalf("expected atomic conflict, got %v", err)
	}
	if got := reviewRecord(t, st, 100); got.Cat != "MEDIUM" || *got.Version != *a.Version {
		t.Fatal("failed batch partially committed")
	}
	res, err := st.ApplyBatchTriageDecisionsWithOptions(dec, BatchApplyOptions{SkipConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OverriddenIssues) != 1 || res.OverriddenIssues[0] != 100 || len(res.SkippedConflicts) != 2 {
		t.Fatalf("unexpected skip result: %+v", res)
	}
	if len(reviewRecord(t, st, 101).PRs) != 0 {
		t.Fatal("skipped companion manual link was written")
	}
	// A valid companion link changes the version within this transaction, but
	// must not invalidate the same batch's already-checked classification.
	a = reviewRecord(t, st, 100)
	_, err = st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"100": *a.Version}, Overrides: map[string]model.TriagePatchRequest{"100": reviewPatch("HARD", nil)}, ManualPRLinks: map[string][]string{"100": {"12345"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewRecord(t, st, 100); got.Cat != "HARD" || len(got.PRs) != 1 {
		t.Fatalf("companion batch failed: %+v", got)
	}
	// Even an error after the first mutation rolls its version counters back.
	a = reviewRecord(t, st, 100)
	_, err = st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Overrides: map[string]model.TriagePatchRequest{"100": reviewPatch("EASY", a.Version)}, Notes: map[string]model.NotePatchRequest{"101": {}}})
	if err == nil {
		t.Fatal("expected invalid note error")
	}
	if got := reviewRecord(t, st, 100); got.Cat != a.Cat || *got.Version != *a.Version {
		t.Fatal("rollback leaked facts or versions")
	}
}

func TestPendingReviewVersionSurvivesCacheWritesAndRequeue(t *testing.T) {
	st := reviewTestStore(t)
	pending, err := st.ListPendingCandidates()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %v", pending, err)
	}
	old := pending[0]
	cached := old
	cached.SuggestedTriage = &model.SuggestedTriage{Cat: "EASY"}
	persisted, err := st.PersistEnrichedPendingCandidates([]model.PendingCandidate{cached})
	if err != nil {
		t.Fatal(err)
	}
	if *persisted[0].Version != *old.Version || persisted[0].Revision <= old.Revision {
		t.Fatal("cache revision leaked into review version")
	}
	// Newly available evidence, including the first PR file details, invalidates review.
	cached = persisted[0]
	cached.PRs = []model.LinkedPRRef{{Repo: DefaultRepo, Num: 12345, State: "OPEN"}}
	cached.PRDetails = []model.PendingPRDetail{{Repo: DefaultRepo, Num: 12345, State: "OPEN", NonTestFiles: []string{"pkg/planner/core/plan.go"}}}
	persisted, err = st.PersistEnrichedPendingCandidates([]model.PendingCandidate{cached})
	if err != nil {
		t.Fatal(err)
	}
	if persisted[0].Version.Review <= old.Version.Review {
		t.Fatal("new evidence did not advance review version")
	}
	if err := st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: old.Version}); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("stale pending decision accepted: %v", err)
	}
	if err := st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: persisted[0].Version}); err != nil {
		t.Fatal(err)
	}
	excluded, err := st.ListExcludedIssues()
	if err != nil || len(excluded) != 1 {
		t.Fatalf("excluded: %v %v", excluded, err)
	}
	if err := st.RestoreExcludedIssueWithPrecondition(200, model.ReviewPrecondition{ExpectedVersion: excluded[0].Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{old}}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ListPendingCandidates()
	if fresh[0].Version.Review <= persisted[0].Version.Review {
		t.Fatal("requeue reset version")
	}
	if err := st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: persisted[0].Version}); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("old incarnation accepted: %v", err)
	}
}

func TestConcurrentReviewCASAcrossStoreConnections(t *testing.T) {
	st := reviewTestStore(t)
	before := reviewRecord(t, st, 100)
	other, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if *reviewRecord(t, other, 100).Version != *before.Version {
		t.Fatal("opening a connection changed review version")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, conn := range []*Store{st, other} {
		wg.Add(1)
		go func(conn *Store, cat string) {
			defer wg.Done()
			<-start
			err := conn.ApplyIssueOverride(100, reviewPatch(cat, before.Version))
			results <- err
		}(conn, []string{"EASY", "HARD"}[i])
	}
	close(start)
	wg.Wait()
	close(results)
	var success, conflict int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrOverrideConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("expected one commit and one conflict: success=%d conflict=%d", success, conflict)
	}
}

func TestReviewReadSnapshotStaysConsistentAcrossConcurrentCommit(t *testing.T) {
	st := reviewTestStore(t)
	writer, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := st.beginReviewRead()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before, err := listRetriageCandidatesFrom(tx, false, []int{100})
	if err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() {
		patch := reviewPatch("HARD", before[0].Version)
		patch.Note = &model.LocalizedNote{ZhCN: "并发更新的根因", En: "Concurrently updated cause"}
		err := writer.ApplyIssueOverride(100, patch)
		committed <- err
	}()
	select {
	case err := <-committed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read snapshot blocked a WAL writer")
	}
	after, err := listRetriageCandidatesFrom(tx, false, []int{100})
	if err != nil {
		t.Fatal(err)
	}
	if after[0].CurrentCat != before[0].CurrentCat || *after[0].Version != *before[0].Version || after[0].CurrentNote.En != before[0].CurrentNote.En {
		t.Fatal("snapshot mixed facts or versions from a concurrent commit")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	fresh := reviewRecord(t, st, 100)
	if fresh.Cat != "HARD" || fresh.Note.En != "Concurrently updated cause" || fresh.Version.Review <= before[0].Version.Review || fresh.Version.Notes <= before[0].Version.Notes {
		t.Fatalf("new snapshot missed committed facts/version: %+v", fresh)
	}
}

func TestPendingPRCacheInvalidatesReview(t *testing.T) {
	st := reviewTestStore(t)
	pcs, _ := st.ListPendingCandidates()
	pcs[0].PRs = []model.LinkedPRRef{{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z"}}
	pcs, err := st.PersistEnrichedPendingCandidates(pcs)
	if err != nil {
		t.Fatal(err)
	}
	old := pcs[0]
	err = st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "MERGED", UpdatedAt: "2020-01-02T00:00:00Z", MergedAt: "2020-01-02T00:00:00Z"}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ListPendingCandidates()
	if fresh[0].Version.Review <= old.Version.Review || fresh[0].PRs[0].State != "MERGED" {
		t.Fatal("PR status and review version were not advanced together")
	}
	// In-flight enrichment cannot overwrite the new status or borrow its version.
	survived, err := st.PersistEnrichedPendingCandidates([]model.PendingCandidate{old})
	if err != nil {
		t.Fatal(err)
	}
	if survived[0].PRs[0].State != "MERGED" || *survived[0].Version != *fresh[0].Version {
		t.Fatal("stale enrichment replaced new facts")
	}
	err = st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "accept", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite", ExpectedVersion: old.Version})
	if err == nil {
		r := reviewRecord(t, st, 200)
		t.Fatalf("stale review accepted unreviewed PR state=%s needsRetriage=%v", r.PRs[0].State, r.NeedsRetriage)
	}
	if !errors.Is(err, ErrOverrideConflict) {
		t.Fatal(err)
	}
	if err := st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "accept", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite", ExpectedVersion: fresh[0].Version}); err != nil {
		t.Fatal(err)
	}
	if got := reviewRecord(t, st, 200); got.PRs[0].State != "MERGED" || got.NeedsRetriage {
		t.Fatal("fresh review did not accept the reviewed baseline")
	}

}
func TestPendingPREvidenceInvalidatesReview(t *testing.T) {
	for name, change := range map[string]func(*model.PendingPRDetail){
		"truncation": func(d *model.PendingPRDetail) { d.FilesTruncated = true },
		"body":       func(d *model.PendingPRDetail) { d.BodyExcerpt = "New root cause evidence" },
		"files": func(d *model.PendingPRDetail) {
			d.ChangedFiles = []model.PRFileStat{{Path: "pkg/planner/core/plan.go"}}
		},
		"title": func(d *model.PendingPRDetail) { d.Title = "Changed title" },
		"base":  func(d *model.PendingPRDetail) { d.BaseRefName = "release-8.5" },
	} {
		t.Run(name, func(t *testing.T) {
			st := reviewTestStore(t)
			pcs, err := st.ListPendingCandidates()
			if err != nil {
				t.Fatal(err)
			}
			pcs[0].PRs = []model.LinkedPRRef{{Repo: DefaultRepo, Num: 12345, State: "OPEN"}}
			pcs[0].PRDetails = []model.PendingPRDetail{{Repo: DefaultRepo, Num: 12345, State: "OPEN", NonTestFiles: []string{"pkg/executor/executor.go"}}}
			pcs, err = st.PersistEnrichedPendingCandidates(pcs)
			if err != nil {
				t.Fatal(err)
			}
			old := *pcs[0].Version
			change(&pcs[0].PRDetails[0])
			pcs, err = st.PersistEnrichedPendingCandidates(pcs)
			if err != nil {
				t.Fatal(err)
			}
			if pcs[0].Version.Review <= old.Review {
				t.Fatalf("changed evidence retained old version %+v", old)
			}
		})
	}
}

func TestPendingCandidateUsesExistingPRCache(t *testing.T) {
	st := reviewTestStore(t)
	status := model.PRStatus{Repo: DefaultRepo, Num: 12345, State: "MERGED", UpdatedAt: "2020-01-02T00:00:00Z", MergedAt: "2020-01-02T00:00:00Z"}
	if err := st.UpsertPRStatuses([]model.PRStatus{status}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	pcs, err := st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	pcs[0].PRs = []model.LinkedPRRef{{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z"}}
	pcs[0].PRDetails = []model.PendingPRDetail{{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z"}}
	pcs, err = st.PersistEnrichedPendingCandidates(pcs)
	if err != nil {
		t.Fatal(err)
	}
	if pcs[0].PRs[0].State != "MERGED" || len(pcs[0].PRDetails) != 0 {
		t.Fatal("pending review retained old cached evidence")
	}
	before := *pcs[0].Version
	if err := st.UpsertPRStatuses([]model.PRStatus{status}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	pcs, err = st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if *pcs[0].Version != before {
		t.Fatal("unchanged PR refresh invalidated review")
	}
}

func TestVersionMigrationReconcilesLegacyPendingPRCache(t *testing.T) {
	st := reviewTestStore(t)
	status := model.PRStatus{Repo: DefaultRepo, Num: 12345, State: "MERGED", UpdatedAt: "2020-01-02T00:00:00Z"}
	if err := st.UpsertPRStatuses([]model.PRStatus{status}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// A database produced by the old implementation can have stale pending JSON
	// even though pull_requests already contains the accepted baseline facts.
	_, err := st.db.Exec(`UPDATE pending_candidates SET payload_json = json_set(payload_json, '$.prs', json(?)) WHERE number = 200`, `[{"repo":"pingcap/tidb","num":12345,"state":"OPEN","updated_at":"2020-01-01T00:00:00Z"}]`)
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	other, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	fresh, err := other.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if fresh[0].PRs[0].State != "MERGED" || fresh[0].Version.Review <= old[0].Version.Review {
		t.Fatal("migration did not reconcile stale pending facts")
	}
}

func TestClosedPRProjectionKeepsNewMetadata(t *testing.T) {
	st := reviewTestStore(t)
	pcs, _ := st.ListPendingCandidates()
	pcs[0].PRs = []model.LinkedPRRef{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "old title", UpdatedAt: "2020-01-01T00:00:00Z"}}
	_, err := st.PersistEnrichedPendingCandidates(pcs)
	if err != nil {
		t.Fatal(err)
	}
	err = st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "old title", UpdatedAt: "2020-01-01T00:00:00Z"}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{model.PRKey(DefaultRepo, 12345): {Repo: DefaultRepo, Num: 12345, State: "MERGED", Title: "new title", IsCherryPick: true, UpdatedAt: "2020-01-02T00:00:00Z", MergedAt: "2020-01-02T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	pcs, err = st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	p := pcs[0].PRs[0]
	if p.Title != "new title" || !p.IsCherryPick {
		t.Fatalf("fresh enrichment metadata rolled back by second cache projection: %+v", p)
	}
	// Subsequent cache normalization must also retain the new metadata.
	pcs, err = st.PersistEnrichedPendingCandidates(pcs)
	if err != nil {
		t.Fatal(err)
	}
	if p := pcs[0].PRs[0]; p.Title != "new title" || !p.IsCherryPick {
		t.Fatalf("cached metadata regressed on reload: %+v", p)
	}

}

func TestNoteVersionRejectsDecisionBeforeReadmission(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint("independent-note=", override), func(t *testing.T) {
			st := reviewTestStore(t)
			if override {
				if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "An independent note"}}}); err != nil {
					t.Fatal(err)
				}
			}
			original := reviewRecord(t, st, 100)
			if err := st.ExcludeExistingIssueWithPrecondition(100, "excluded", false, model.ReviewPrecondition{ExpectedVersion: original.Version}); err != nil {
				t.Fatal(err)
			}
			if err := st.RestoreExcludedIssue(100); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{{Number: 100, Title: "readmitted", Labels: []string{"type/bug"}}}}); err != nil {
				t.Fatal(err)
			}
			if err := st.ResolvePendingCandidate(100, model.PendingResolveRequest{Action: "accept", Cat: "HARD", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "重新审阅后的根因", En: "New cause after readmission"}}); err != nil {
				t.Fatal(err)
			}
			fresh := reviewRecord(t, st, 100)
			if fresh.Version.Notes <= original.Version.Notes {
				t.Fatalf("note lifetime reused: old=%+v new=%+v", original.Version, fresh.Version)
			}
			_, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "Stale overwrite", ExpectedVersion: original.Version}}})
			if !errors.Is(err, ErrBatchConflict) {
				t.Fatalf("stale note accepted: %v", err)
			}
			if reviewRecord(t, st, 100).Note.En != fresh.Note.En {
				t.Fatal("stale write changed note")
			}
			if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "Reviewed current note", ExpectedVersion: fresh.Version}}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpgradeInvalidatesOldNoteDecisionsAndReplacesTriggers(t *testing.T) {
	st := reviewTestStore(t)
	old := reviewRecord(t, st, 100)
	// Model a prior schema without canonical note lifetime triggers.
	for _, query := range []string{
		"DROP TRIGGER version_issue_notes_note_version_insert",
		"DROP TRIGGER version_issue_notes_note_version_delete",
		"PRAGMA user_version = 1",
	} {
		if _, err := st.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	upgraded, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	fresh := reviewRecord(t, upgraded, 100)
	if fresh.Version.Notes <= old.Version.Notes {
		t.Fatal("upgrade preserved potentially stale note decisions")
	}
	var triggers int
	if err := upgraded.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN ('version_issue_notes_note_version_insert', 'version_issue_notes_note_version_delete')`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if triggers != 2 {
		t.Fatal("lifecycle triggers not installed")
	}
	_, err = upgraded.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "Old export", ExpectedVersion: old.Version}}})
	if !errors.Is(err, ErrBatchConflict) {
		t.Fatalf("old export accepted after upgrade: %v", err)
	}
}
