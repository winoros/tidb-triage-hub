package store

import (
	"errors"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

func TestReviewProtocolAndSemanticNotes(t *testing.T) {
	st := reviewTestStore(t)
	original := reviewRecord(t, st, 100)
	patch := reviewPatch("HARD", original.Version)
	patch.Note = original.Note
	patch.IfUnmodifiedSince = "invalid legacy timestamp"
	if err := st.ApplyIssueOverride(100, patch); err != nil {
		t.Fatal(err)
	}
	after := reviewRecord(t, st, 100)
	if after.Version.Notes != original.Version.Notes {
		t.Fatal("unchanged effective note advanced notes version")
	}
	// Same note again, even after another classification, is still a semantic no-op.
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: original.Note.En, ExpectedVersion: original.Version}}}); err != nil {
		t.Fatal(err)
	}
	if reviewRecord(t, st, 100).Version.Notes != original.Version.Notes {
		t.Fatal("note no-op advanced version")
	}
	// An inline version opts the entire batch into versioned review.
	_, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Overrides: map[string]model.TriagePatchRequest{"100": reviewPatch("EASY", after.Version)}, AckRetriage: []int{101}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mixed protocol accepted: %v", err)
	}
	if reviewRecord(t, st, 100).Cat != "HARD" {
		t.Fatal("invalid batch partially committed")
	}
	// Version guards take precedence over export time; aliases normalize once.
	_, err = st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{" #100 ": *after.Version}, BaseExportedAt: "1999-01-01T00:00:00Z", Overrides: map[string]model.TriagePatchRequest{" 100 ": reviewPatch("EASY", nil)}})
	if err != nil {
		t.Fatalf("valid version rejected by legacy time: %v", err)
	}
	_, err = st.ApplyBatchTriageDecisionsWithOptions(model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"99999": {Review: 1, Notes: 1}}, AckRetriage: []int{99999}}, BatchApplyOptions{SkipConflicts: true})
	if !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("unknown issue hidden as skipped conflict: %v", err)
	}
}

func TestAckInputProtocolIsAtomic(t *testing.T) {
	st := reviewTestStore(t)
	v := *reviewRecord(t, st, 100).Version
	for _, tc := range []struct {
		nums     []int
		versions map[string]model.ReviewVersion
	}{
		{[]int{100, 0}, nil},
		{[]int{-1}, nil},
		{[]int{100}, map[string]model.ReviewVersion{"100": v, "#100": v}},
		{[]int{100, 101}, map[string]model.ReviewVersion{"100": v}},
	} {
		if _, err := st.AcknowledgeBatchRetriageWithVersions(tc.nums, "test", "", nil, tc.versions); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid ack accepted: %v", err)
		}
		if *reviewRecord(t, st, 100).Version != v {
			t.Fatal("invalid ack partially committed")
		}
	}
	if _, err := st.AcknowledgeBatchRetriageWithVersions([]int{100}, "test", "invalid", map[string]string{"100": "invalid"}, map[string]model.ReviewVersion{" #100 ": v}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedPRFactsPreserveEvidenceAcrossQueues(t *testing.T) {
	st := reviewTestStore(t)
	const stamp = "2020-01-02T00:00:00Z"
	ref := model.LinkedPRRef{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "planner", UpdatedAt: stamp, HeadOID: "head1", BaseOID: "base1"}
	pc := model.PendingCandidate{Number: 200, Title: "pending", PRs: []model.LinkedPRRef{ref}}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ManualPRLinks: map[string][]string{"100": {"12345"}}}); err != nil {
		t.Fatal(err)
	}
	detail := model.PendingPRDetail{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "planner", UpdatedAt: stamp, HeadOID: "head1", BaseOID: "base1", NonTestFiles: []string{"pkg/planner/core/plan.go"}, BodyExcerpt: "root cause"}
	save := func(d model.PendingPRDetail) {
		t.Helper()
		if err := st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{model.PRKey(d.Repo, d.Num): d}); err != nil {
			t.Fatal(err)
		}
	}
	save(detail)
	first, _ := st.ListPendingCandidates()
	v := *first[0].Version
	// A sync plan assembled before enrichment must not remove newly cached files.
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ListPendingCandidates()
	if len(fresh[0].PRDetails) != 1 {
		t.Fatal("old sync plan deleted shared evidence")
	}
	// A newer timestamp retains immutable files, but hides old prose and invalidates review.
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "planner", UpdatedAt: "2020-01-03T00:00:00Z", HeadOID: "head1", BaseOID: "base1"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	fresh, _ = st.ListPendingCandidates()
	if len(fresh[0].PRDetails) != 1 || fresh[0].PRDetails[0].BodyExcerpt != "" || fresh[0].Version.Review <= v.Review {
		t.Fatalf("mutable prose was not invalidated: old=%+v new=%+v", v, fresh[0])
	}
	snapshot, err := st.LoadReviewSnapshot(true, true, []int{100})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Retriage) != 1 || len(snapshot.Retriage[0].PRDetails) != 1 {
		t.Fatal("retriage did not use shared evidence")
	}
	// Unqueried labels in a later detail response cannot erase known cherry-pick facts.
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "planner", IsCherryPick: true, UpdatedAt: "2020-01-04T00:00:00Z", HeadOID: "head1", BaseOID: "base1"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	detail.UpdatedAt = "2020-01-05T00:00:00Z"
	save(detail)
	fresh, _ = st.ListPendingCandidates()
	if !fresh[0].PRs[0].IsCherryPick || !reviewRecord(t, st, 100).PRs[0].IsCherryPick {
		t.Fatal("unknown labels erased cherry-pick evidence")
	}
	// An authoritative commit change hides old code evidence in every queue.
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "planner", UpdatedAt: "2020-01-06T00:00:00Z", HeadOID: "head2", BaseOID: "base1"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = st.LoadReviewSnapshot(true, true, []int{100})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pending[0].PRDetails) != 0 || len(snapshot.Retriage[0].PRDetails) != 0 {
		t.Fatal("old commit evidence survived new revision")
	}
}

func TestCurrentSchemaOpenDoesNotRepairOrWrite(t *testing.T) {
	st := reviewTestStore(t)
	before := reviewRecord(t, st, 100)
	var changeBefore int
	if err := st.db.QueryRow(`PRAGMA data_version`).Scan(&changeBefore); err != nil {
		t.Fatal(err)
	}
	other, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	var changeAfter int
	if err := st.db.QueryRow(`PRAGMA data_version`).Scan(&changeAfter); err != nil {
		t.Fatal(err)
	}
	if changeAfter != changeBefore || *reviewRecord(t, st, 100).Version != *before.Version {
		t.Fatal("opening current schema wrote data")
	}
	ro, err := Open(sqliteFileURI(st.DBPath(), "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.LoadWorkbenchSnapshot(); err != nil {
		t.Fatal(err)
	}
	copy, cleanup, err := OpenSnapshotForDryRun(st.DBPath(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if *reviewRecord(t, copy, 100).Version != *before.Version {
		t.Fatal("dry-run copy invented a review version")
	}
}

func TestSharedEqualTimestampOldCommitDeletesFreshEvidence(t *testing.T) {
	st := reviewTestStore(t)
	pc := model.PendingCandidate{Number: 200, Title: "pending", PRs: []model.LinkedPRRef{{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z", HeadOID: "h2", BaseOID: "b2"}}}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z", HeadOID: "h2", BaseOID: "b2"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	fresh := model.PendingPRDetail{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z", HeadOID: "h2", BaseOID: "b2", NonTestFiles: []string{"pkg/planner/fresh.go"}}
	if err := st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{"p": fresh}); err != nil {
		t.Fatal(err)
	}
	old := fresh
	old.HeadOID = "h1"
	old.BaseOID = "b1"
	old.NonTestFiles = []string{"pkg/planner/old.go"}
	if err := st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{"p": old}); err != nil {
		t.Fatal(err)
	}
	pcs, err := st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(pcs[0].PRDetails) != 1 || pcs[0].PRDetails[0].HeadOID != "h2" {
		t.Fatalf("fresh evidence lost after old same-time response: %+v", pcs[0])
	}
}

func TestSharedTimelineProjectionReevaluatesAllLinkedIssues(t *testing.T) {
	st := reviewTestStore(t)
	ref := model.LinkedPRRef{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-01T00:00:00Z"}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpdatedIssues: []issuesync.LiveIssueUpdate{{Number: 100, Title: "one", Labels: []string{"type/bug"}, TimelinePRs: []model.LinkedPRRef{ref}}, {Number: 101, Title: "two", Labels: []string{"type/bug"}, TimelinePRs: []model.LinkedPRRef{ref}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcknowledgeBatchRetriage([]int{100, 101}, "test"); err != nil {
		t.Fatal(err)
	}
	ref.State = "MERGED"
	ref.UpdatedAt = "2020-01-02T00:00:00Z"
	ref.MergedAt = ref.UpdatedAt
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpdatedIssues: []issuesync.LiveIssueUpdate{{Number: 100, Title: "one", Labels: []string{"type/bug"}, TimelinePRs: []model.LinkedPRRef{ref}}}}); err != nil {
		t.Fatal(err)
	}
	one := reviewRecord(t, st, 100)
	two := reviewRecord(t, st, 101)
	if !one.NeedsRetriage {
		t.Fatal("fixture did not trigger merge re-triage")
	}
	if two.PRs[0].State != "MERGED" || !two.NeedsRetriage {
		t.Fatalf("shared PR projected without re-triage evaluation: %+v", two)
	}
}

func TestSharedPartialTimelineCannotClearKnownCherryPick(t *testing.T) {
	st := reviewTestStore(t)
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "fix planner", IsCherryPick: true, UpdatedAt: "2020-01-01T00:00:00Z"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy or truncated label observation. Absence of positive
	// evidence in that response must not clear an established cherry-pick fact.
	ref := model.LinkedPRRef{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "fix planner", UpdatedAt: "2020-01-02T00:00:00Z", IsCherryPick: false}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpdatedIssues: []issuesync.LiveIssueUpdate{{Number: 100, Title: "one", Labels: []string{"type/bug"}, TimelinePRs: []model.LinkedPRRef{ref}}}}); err != nil {
		t.Fatal(err)
	}
	if !reviewRecord(t, st, 100).PRs[0].IsCherryPick {
		t.Fatal("partial timeline cleared canonical cherry-pick fact")
	}
}

func TestCompletePRObservationCanClearCherryPick(t *testing.T) {
	st := reviewTestStore(t)
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", IsCherryPick: true, UpdatedAt: "2020-01-01T00:00:00Z"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	ref := model.LinkedPRRef{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2020-01-02T00:00:00Z", CherryPickKnown: true}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpdatedIssues: []issuesync.LiveIssueUpdate{{Number: 100, Title: "one", Labels: []string{"type/bug"}, TimelinePRs: []model.LinkedPRRef{ref}}}}); err != nil {
		t.Fatal(err)
	}
	if reviewRecord(t, st, 100).PRs[0].IsCherryPick {
		t.Fatal("complete negative evidence ignored")
	}
}
