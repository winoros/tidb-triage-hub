package store

import (
	"errors"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

func prEvidenceReviewStore(t *testing.T, detail model.PendingPRDetail, cache bool) *Store {
	t.Helper()
	st := reviewTestStore(t)
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ManualPRLinks: map[string][]string{"100": {"12345"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPRStatuses([]model.PRStatus{detail.Facts()}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	pc := model.PendingCandidate{Number: 200, Title: "pending planner bug", Body: "select 1", PRs: []model.LinkedPRRef{(model.LinkedPRRef{Repo: detail.Repo, Num: detail.Num}).WithFacts(detail.Facts())}}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	if cache {
		saveReviewDetail(t, st, detail)
	}
	return st
}

func saveReviewDetail(t *testing.T, st *Store, d model.PendingPRDetail) {
	t.Helper()
	if err := st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{"pr": d}); err != nil {
		t.Fatal(err)
	}
}

func evidenceReviewSnapshot(t *testing.T, st *Store) ReviewSnapshot {
	t.Helper()
	s, err := st.LoadReviewSnapshot(true, true, []int{100})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 1 || len(s.Retriage) != 1 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	return s
}

func assertEvidenceReviewChange(t *testing.T, before, after ReviewSnapshot, delta int64) {
	t.Helper()
	for i, pair := range [][2]*model.ReviewVersion{{before.Pending[0].Version, after.Pending[0].Version}, {before.Retriage[0].Version, after.Retriage[0].Version}} {
		if pair[1].Review != pair[0].Review+delta || pair[1].Notes != pair[0].Notes {
			t.Fatalf("queue %d: version %+v -> %+v, want review delta %d only", i, pair[0], pair[1], delta)
		}
	}
	if (before.Pending[0].SyncWatermark != after.Pending[0].SyncWatermark) != (delta != 0) {
		t.Fatal("pending watermark does not match visible evidence change")
	}
}

func checkEvidenceReviewSubmissions(t *testing.T, st *Store, snap ReviewSnapshot, conflict bool) {
	t.Helper()
	errs := []error{
		st.ApplyIssueOverride(100, reviewPatch("HARD", snap.Retriage[0].Version)),
		st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: snap.Pending[0].Version}),
	}
	for _, err := range errs {
		if conflict && !errors.Is(err, ErrOverrideConflict) {
			t.Fatalf("stale review not rejected: %v", err)
		}
		if !conflict && err != nil {
			t.Fatalf("unchanged evidence invalidated review: %v", err)
		}
	}
}

func reviewEvidenceDetail() model.PendingPRDetail {
	return model.PendingPRDetail{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: "head", BaseOID: "base", BodyExcerpt: "wrong result in plan cache", NonTestFiles: []string{"pkg/planner/core/plan.go"}}
}

func TestPRTimestampWithoutVisibleEvidenceChangeKeepsReview(t *testing.T) {
	for _, mode := range []string{"no-cache", "empty-body", "already-hidden"} {
		t.Run(mode, func(t *testing.T) {
			d := reviewEvidenceDetail()
			if mode == "empty-body" {
				d.BodyExcerpt = ""
			}
			st := prEvidenceReviewStore(t, d, mode != "no-cache")
			fact := d.Facts()
			if mode == "already-hidden" {
				fact.UpdatedAt = "2026-11-17T01:00:00Z"
				if err := st.UpsertPRStatuses([]model.PRStatus{fact}, time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			before := evidenceReviewSnapshot(t, st)
			for _, stamp := range []string{"2026-11-17T02:00:00Z", "2026-11-17T03:00:00Z"} {
				fact.UpdatedAt = stamp
				if err := st.UpsertPRStatuses([]model.PRStatus{fact}, time.Time{}); err != nil {
					t.Fatal(err)
				}
				after := evidenceReviewSnapshot(t, st)
				assertEvidenceReviewChange(t, before, after, 0)
				if len(after.Pending[0].PRDetails) != len(before.Pending[0].PRDetails) {
					t.Fatal("timestamp-only update removed matching files")
				}
			}
			checkEvidenceReviewSubmissions(t, st, before, false)
		})
	}
}

func TestPRProseHideAndRestoreAdvancePendingReviewOnce(t *testing.T) {
	d := reviewEvidenceDetail()
	st := prEvidenceReviewStore(t, d, true)
	visible := evidenceReviewSnapshot(t, st)
	fact := d.Facts()
	fact.UpdatedAt = "2026-11-17T01:00:00Z"
	if err := st.UpsertPRStatuses([]model.PRStatus{fact}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	hidden := evidenceReviewSnapshot(t, st)
	assertEvidenceReviewChange(t, visible, hidden, 1)
	for _, details := range [][]model.PendingPRDetail{hidden.Pending[0].PRDetails, hidden.Retriage[0].PRDetails} {
		if len(details) != 1 || details[0].BodyExcerpt != "" || len(details[0].NonTestFiles) != 1 {
			t.Fatalf("wrong hidden evidence: %+v", details)
		}
	}
	checkEvidenceReviewSubmissions(t, st, visible, true)
	// A repeated observation has no further effect while prose is already hidden.
	if err := st.UpsertPRStatuses([]model.PRStatus{fact}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	assertEvidenceReviewChange(t, hidden, evidenceReviewSnapshot(t, st), 0)
	d.UpdatedAt = fact.UpdatedAt
	saveReviewDetail(t, st, d)
	restored := evidenceReviewSnapshot(t, st)
	assertEvidenceReviewChange(t, hidden, restored, 1)
	if restored.Pending[0].PRDetails[0].BodyExcerpt != d.BodyExcerpt || restored.Retriage[0].PRDetails[0].BodyExcerpt != d.BodyExcerpt {
		t.Fatal("prose was not restored")
	}
	checkEvidenceReviewSubmissions(t, st, hidden, true)
	saveReviewDetail(t, st, d)
	assertEvidenceReviewChange(t, restored, evidenceReviewSnapshot(t, st), 0)
	checkEvidenceReviewSubmissions(t, st, restored, false)
}

func TestAtomicPRDetailRefreshComparesFinalVisibleEvidence(t *testing.T) {
	for _, path := range []string{"details", "pending-enrichment", "issue-sync"} {
		for _, mode := range []string{"empty", "unchanged", "changed"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				d := reviewEvidenceDetail()
				if mode == "empty" {
					d.BodyExcerpt = ""
				}
				st := prEvidenceReviewStore(t, d, true)
				before := evidenceReviewSnapshot(t, st)
				d.UpdatedAt = "2026-11-17T01:00:00Z"
				delta := int64(0)
				if mode == "changed" {
					d.BodyExcerpt = "different root cause"
					delta = 1
				}
				pc := before.Pending[0]
				pc.PRs = []model.LinkedPRRef{pc.PRs[0].WithFacts(d.Facts())}
				pc.PRDetails = []model.PendingPRDetail{d}
				switch path {
				case "details":
					saveReviewDetail(t, st, d)
				case "pending-enrichment":
					if _, err := st.PersistEnrichedPendingCandidates([]model.PendingCandidate{pc}); err != nil {
						t.Fatal(err)
					}
				case "issue-sync":
					if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
						t.Fatal(err)
					}
				}
				after := evidenceReviewSnapshot(t, st)
				assertEvidenceReviewChange(t, before, after, delta)
				if after.Pending[0].PRDetails[0].BodyExcerpt != d.BodyExcerpt || after.Retriage[0].PRDetails[0].BodyExcerpt != d.BodyExcerpt {
					t.Fatal("wrong final prose")
				}
				checkEvidenceReviewSubmissions(t, st, before, delta != 0)
			})
		}
	}
}

func TestPRIdentityAndLegacyTimeStillInvalidateReviews(t *testing.T) {
	for _, mode := range []string{"head", "base", "legacy-time", "incomplete-identity-files"} {
		t.Run(mode, func(t *testing.T) {
			d := reviewEvidenceDetail()
			d.BodyExcerpt = ""
			if mode == "legacy-time" {
				d.HeadOID, d.BaseOID = "", ""
			}
			if mode == "incomplete-identity-files" {
				d.BaseOID = ""
			}
			st := prEvidenceReviewStore(t, d, mode == "incomplete-identity-files")
			before := evidenceReviewSnapshot(t, st)
			fact := d.Facts()
			fact.UpdatedAt = "2026-11-17T01:00:00Z"
			switch mode {
			case "head":
				fact.HeadOID = "new-head"
			case "base":
				fact.BaseOID = "new-base"
			}
			if err := st.UpsertPRStatuses([]model.PRStatus{fact}, time.Time{}); err != nil {
				t.Fatal(err)
			}
			assertEvidenceReviewChange(t, before, evidenceReviewSnapshot(t, st), 1)
			checkEvidenceReviewSubmissions(t, st, before, true)
		})
	}
}

func TestPendingPRRefreshOrdersRelationshipAndDetails(t *testing.T) {
	for _, mode := range []string{"fresher-detail", "stale-detail", "conflicting-detail"} {
		t.Run(mode, func(t *testing.T) {
			d := reviewEvidenceDetail()
			st := prEvidenceReviewStore(t, d, true)
			before := evidenceReviewSnapshot(t, st)
			pc := before.Pending[0]
			pc.PRs = append([]model.LinkedPRRef(nil), pc.PRs...)
			newer := "2026-11-17T01:00:00Z"
			delta := int64(1)
			switch mode {
			case "fresher-detail":
				d.UpdatedAt = newer
				delta = 0
			case "stale-detail":
				pc.PRs[0].UpdatedAt = newer
			case "conflicting-detail":
				pc.PRs[0].UpdatedAt, pc.PRs[0].HeadOID, d.UpdatedAt = newer, "new-head", newer
			}
			pc.PRDetails = []model.PendingPRDetail{d}
			if _, err := st.PersistEnrichedPendingCandidates([]model.PendingCandidate{pc}); err != nil {
				t.Fatal(err)
			}
			after := evidenceReviewSnapshot(t, st)
			assertEvidenceReviewChange(t, before, after, delta)
			if after.Pending[0].PRs[0].UpdatedAt != newer {
				t.Fatal("newer PR facts lost")
			}
			for _, details := range [][]model.PendingPRDetail{after.Pending[0].PRDetails, after.Retriage[0].PRDetails} {
				switch mode {
				case "fresher-detail":
					if len(details) != 1 || details[0].BodyExcerpt != d.BodyExcerpt {
						t.Fatal("fresh detail lost")
					}
				case "stale-detail":
					if len(details) != 1 || details[0].BodyExcerpt != "" {
						t.Fatal("stale prose exposed")
					}
				case "conflicting-detail":
					if len(details) != 0 {
						t.Fatal("conflicting commit evidence exposed")
					}
				}
			}
			checkEvidenceReviewSubmissions(t, st, before, delta != 0)
		})
	}
}
