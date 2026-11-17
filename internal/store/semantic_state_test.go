package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

func TestVersionedCommandsRejectMovedTargets(t *testing.T) {
	st := reviewTestStore(t)
	pcs, err := st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	oldPending := pcs[0].Version
	if err := st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: oldPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolvePendingCandidate(200, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: oldPending}); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("stale pending: %v", err)
	}
	old := reviewRecord(t, st, 100)
	if err := st.ExcludeExistingIssue(100, "exclude", false); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyIssueOverride(100, reviewPatch("HARD", old.Version)); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("stale override: %v", err)
	}
	if err := st.ResetIssueOverrideWithPrecondition(100, model.ReviewPrecondition{ExpectedVersion: old.Version}); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("stale reset: %v", err)
	}
	if err := st.ApplyIssueOverride(99999, reviewPatch("HARD", old.Version)); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("unknown identity: %v", err)
	}
	// Even a current excluded-object version cannot acknowledge an active issue.
	v, err := issueVersion(st.db, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, nums := range [][]int{{100}, {100, 101}} {
		versions := map[string]model.ReviewVersion{"100": v, "101": *reviewRecord(t, st, 101).Version}
		if _, err := st.AcknowledgeBatchRetriageWithVersions(nums, "test", "", nil, versions); !errors.Is(err, ErrOverrideConflict) {
			t.Fatalf("ack %v: %v", nums, err)
		}
	}
}

func TestExcludeChecksConcurrentNotesInSingleAndBatch(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			st := reviewTestStore(t)
			old := reviewRecord(t, st, 100)
			if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "Concurrent note", ExpectedVersion: old.Version}}}); err != nil {
				t.Fatal(err)
			}
			var err error
			if batch {
				_, err = st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"100": *old.Version}, ExcludeIssues: map[string]string{"100": "exclude"}})
			} else {
				err = st.ExcludeExistingIssueWithPrecondition(100, "exclude", false, model.ReviewPrecondition{ExpectedVersion: old.Version})
			}
			if !errors.Is(err, ErrOverrideConflict) && !errors.Is(err, ErrBatchConflict) {
				t.Fatalf("stale exclusion accepted: %v", err)
			}
			if reviewRecord(t, st, 100).Note.En != "Concurrent note" {
				t.Fatal("concurrent note was lost")
			}
		})
	}
}

func TestCanonicalNotesHaveOneLifetimeAcrossArchiveAndAccept(t *testing.T) {
	st := reviewTestStore(t)
	old := reviewRecord(t, st, 100)
	// Legacy import columns cannot become an alternate effective note source.
	if _, err := st.db.Exec(`UPDATE issues SET auto_note_en='ignored legacy value' WHERE number=100`); err != nil {
		t.Fatal(err)
	}
	if reviewRecord(t, st, 100).Note.En != old.Note.En {
		t.Fatal("legacy note leaked into effective note")
	}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{ClosedRemovals: []model.DeltaClosedIssue{{Number: 100, Reason: "closed"}}}); err != nil {
		t.Fatal(err)
	}
	arch, err := issueVersion(st.db, 100)
	if err != nil {
		t.Fatal(err)
	}
	if arch.Notes != old.Version.Notes {
		t.Fatal("archive changed a surviving note's version")
	}
	// A note command cannot target the archived queue even with unchanged notes.
	if err := checkReviewCommand(st.db, 100, old.Version, "notes", true); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("archived notes accepted: %v", err)
	}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{{Number: 100, Title: "reopened", Labels: []string{"type/bug"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolvePendingCandidate(100, model.PendingResolveRequest{Action: "accept", Cat: "HARD", Severity: "MAJOR", Subsystem: "statistics", Note: old.Note}); err != nil {
		t.Fatal(err)
	}
	current := reviewRecord(t, st, 100)
	if current.Version.Notes != old.Version.Notes || current.Note.En != old.Note.En {
		t.Fatal("same surviving note acquired a different lifetime")
	}
	// Once active again, a note-only edit can coexist with the new classification.
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{"100": {En: "Continued analysis", ExpectedVersion: old.Version}}}); err != nil {
		t.Fatal(err)
	}
}

func TestExcludeEmptyCanonicalNoteStillEndsLifetime(t *testing.T) {
	st := reviewTestStore(t)
	if err := st.ApplyIssueOverride(100, model.TriagePatchRequest{Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{}}); err != nil {
		t.Fatal(err)
	}
	old := reviewRecord(t, st, 100)
	if err := st.ExcludeExistingIssueWithPrecondition(100, "exclude", false, model.ReviewPrecondition{ExpectedVersion: old.Version}); err != nil {
		t.Fatal(err)
	}
	after, err := issueVersion(st.db, 100)
	if err != nil {
		t.Fatal(err)
	}
	if after.Notes <= old.Version.Notes {
		t.Fatal("empty note deletion did not end its lifetime")
	}
	var count int
	if err := st.db.QueryRow(`SELECT count(*) FROM issue_notes WHERE number=100`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("excluded note survived deletion")
	}
}

func TestMigrationRefusesNewerSchemaAfterWaitingForWriter(t *testing.T) {
	st := reviewTestStore(t)
	if _, err := st.db.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", sqliteFileURI(st.DBPath(), "_pragma=busy_timeout(5000)&_txlock=immediate"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	done := make(chan error, 1)
	go func() { done <- st.ensureSchema(false) }()
	select {
	case err := <-done:
		t.Fatalf("migration did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := tx.Exec(`PRAGMA user_version=999`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("older migration accepted newer schema")
	}
	var version int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 999 {
		t.Fatalf("downgraded schema to %d", version)
	}
}

func TestPartialFactsInvalidateAssessmentAndNormalizePendingSet(t *testing.T) {
	st := reviewTestStore(t)
	old := model.PRStatus{Repo: DefaultRepo, Num: 91001, State: "DRAFT", IsDraft: true, BlockerStage: model.StageDraftOrHold, HeadOID: "h1", BaseOID: "b", UpdatedAt: "2026-11-16T00:00:00Z"}
	if err := st.UpsertPRStatuses([]model.PRStatus{old}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	pc := model.PendingCandidate{Number: 300, Title: "pending", PRs: []model.LinkedPRRef{{Repo: DefaultRepo, Num: 91001, State: "OPEN", HeadOID: "h2", BaseOID: "b", UpdatedAt: "2026-11-17T00:00:00Z"}, {Repo: DefaultRepo, Num: 91000, State: "OPEN"}}}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	got, complete, err := readPRFact(st.db, DefaultRepo, 91001)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "OPEN" || got.IsDraft || got.BlockerStage != "" || complete {
		t.Fatalf("mixed metadata and stale assessment: %+v complete=%v", got, complete)
	}
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 91001, State: "MERGED", HeadOID: "h2", BaseOID: "b", UpdatedAt: "2026-11-18T00:00:00Z"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	load := func() model.PendingCandidate {
		t.Helper()
		pcs, err := st.ListPendingCandidates()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pcs {
			if p.Number == 300 {
				return p
			}
		}
		t.Fatal("missing pending")
		return model.PendingCandidate{}
	}
	pc = load()
	before := *pc.Version
	watermark := pc.SyncWatermark
	if pc.PRs[0].Num != 91000 {
		t.Fatal("projection did not normalize display order")
	}
	pc.PRs[0], pc.PRs[1] = pc.PRs[1], pc.PRs[0]
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pc}}); err != nil {
		t.Fatal(err)
	}
	pc = load()
	if *pc.Version != before || pc.SyncWatermark != watermark {
		t.Fatal("PR ordering changed review facts")
	}
}

func TestUnresolvedLinksAreResolvedOnceRegardlessOfBoardFilter(t *testing.T) {
	st := reviewTestStore(t)
	if err := st.ApplyIssueOverride(100, model.TriagePatchRequest{Cat: "FILTER_NON_OPT_OR_FIXED", Severity: "N/A", Subsystem: "logical_rewrite"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ManualPRLinks: map[string][]string{"100": {"91000"}}}); err != nil {
		t.Fatal(err)
	}
	targets, err := st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Number != 91000 {
		t.Fatalf("unresolved filtered link missed: %+v", targets)
	}
	if err := st.ApplyPRFetchResults(nil, []prstatus.PRTarget{{Repo: DefaultRepo, Number: 91000}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	targets, err = st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("failed lookup automatically retried: %+v", targets)
	}
	if len(reviewRecord(t, st, 100).PRs) != 1 {
		t.Fatal("failed lookup deleted the manual relationship")
	}
	// Explicit retry bypasses target selection; a successful observation clears failure.
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 91000, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z"}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var failures int
	if err := st.db.QueryRow(`SELECT count(*) FROM pr_lookup_failures`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Fatal("success did not clear failed lookup")
	}
	targets, err = st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatal("fully resolved filtered PR leaked into recurring board sync")
	}
}

func TestPendingExclusionEndsCanonicalNoteLifetime(t *testing.T) {
	st := reviewTestStore(t)
	old := reviewRecord(t, st, 100)
	if _, e := st.ApplyIssueSync(issuesync.SyncPlan{ClosedRemovals: []model.DeltaClosedIssue{{Number: 100, Reason: "closed"}}}); e != nil {
		t.Fatal(e)
	}
	if _, e := st.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{{Number: 100, Title: "reopened", Labels: []string{"type/bug"}}}}); e != nil {
		t.Fatal(e)
	}
	pc, e := st.ListPendingCandidates()
	if e != nil {
		t.Fatal(e)
	}
	var v *model.ReviewVersion
	for _, p := range pc {
		if p.Number == 100 {
			v = p.Version
		}
	}
	if v == nil {
		t.Fatal("missing pending version")
	}
	if e := st.ResolvePendingCandidate(100, model.PendingResolveRequest{Action: "exclude", ExpectedVersion: v}); e != nil {
		t.Fatal(e)
	}
	var count int
	if e := st.db.QueryRow(`SELECT count(*) FROM issue_notes WHERE number=100`).Scan(&count); e != nil {
		t.Fatal(e)
	}
	after, e := issueVersion(st.db, 100)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("pending exclude: surviving notes=%d version old=%+v now=%+v", count, old.Version, after)
	if after.Notes <= old.Version.Notes {
		t.Fatal("note deletion did not advance version")
	}
	if count != 0 {
		t.Fatal("pending exclusion did not end canonical note lifetime")
	}
}

func TestImportNotesFillsOnlyUntouchedEmptyPlaceholder(t *testing.T) {
	st := reviewTestStore(t)
	rec := model.IssueRecord{Number: 300, Title: "imported", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "statistics"}
	ingest := func() {
		t.Helper()
		if err := st.IngestSyncedDataset(model.Dataset{Records: []model.IssueRecord{rec}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ingest()
	before := reviewRecord(t, st, 300)
	rec.Note = &model.LocalizedNote{En: "Imported root cause"}
	ingest()
	after := reviewRecord(t, st, 300)
	if after.Note == nil || after.Note.En != rec.Note.En || after.Version.Notes <= before.Version.Notes {
		t.Fatalf("placeholder not filled: %+v", after)
	}
	ingest()
	if reviewRecord(t, st, 300).Version.Notes != after.Version.Notes {
		t.Fatal("same import changed note version")
	}
	rec.Note = &model.LocalizedNote{En: "Replacement import"}
	ingest()
	if reviewRecord(t, st, 300).Note.En != after.Note.En {
		t.Fatal("import replaced existing note")
	}
	if err := st.ApplyIssueOverride(300, model.TriagePatchRequest{Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{}}); err != nil {
		t.Fatal(err)
	}
	cleared := reviewRecord(t, st, 300)
	ingest()
	final := reviewRecord(t, st, 300)
	if (final.Note != nil && (final.Note.En != "" || final.Note.ZhCN != "")) || final.Version.Notes != cleared.Version.Notes {
		t.Fatal("import overwrote manually cleared note")
	}
}

func TestSameSecondMergedEnrichmentInvalidatesOpenAssessment(t *testing.T) {
	st := reviewTestStore(t)
	const stamp = "2099-11-17T00:00:00Z"
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ManualPRLinks: map[string][]string{"100": {"12345"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: stamp, HeadOID: "head-before-push", BaseOID: "base", BlockerStage: model.StageDraftOrHold}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	d := model.PendingPRDetail{Repo: DefaultRepo, Num: 12345, State: "MERGED", UpdatedAt: stamp, MergedAt: stamp, HeadOID: "head-after-push", BaseOID: "base", NonTestFiles: []string{"pkg/planner/new.go"}}
	if err := st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{"pr": d}); err != nil {
		t.Fatal(err)
	}
	fact, complete, err := readPRFact(st.db, DefaultRepo, 12345)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after merged enrichment state=%s head=%s mergedAt=%q assessmentComplete=%v", fact.State, fact.HeadOID, fact.MergedAt, complete)
	r := reviewRecord(t, st, 100)
	t.Logf("exported issue PR state=%s mergedAt=%q", r.PRs[0].State, r.PRs[0].MergedAt)
	if complete || fact.BlockerStage != "" {
		t.Fatal("old OPEN assessment survived terminal transition")
	}
	if r.PRs[0].State != "MERGED" || !r.NeedsRetriage {
		t.Fatalf("merge not projected into retriage: %+v", r)
	}
	if fact.State != "MERGED" {
		t.Fatal("strictly newer terminal MERGED observation was rejected on equal timestamp commit conflict")
	}
}

func TestRestoredVisibleProseInvalidatesReviewedSnapshot(t *testing.T) {
	for _, mode := range []string{"retriage", "backfill"} {
		for _, body := range []string{"wrong result in plan cache", ""} {
			t.Run(mode+"/"+map[bool]string{true: "body", false: "empty"}[body != ""], func(t *testing.T) {
				st := reviewTestStore(t)
				if mode == "backfill" {
					if err := st.ApplyIssueOverride(100, model.TriagePatchRequest{Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{}}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{ManualPRLinks: map[string][]string{"100": {"12345"}}}); err != nil {
					t.Fatal(err)
				}
				d := model.PendingPRDetail{Repo: DefaultRepo, Num: 12345, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: "head", BaseOID: "base", BodyExcerpt: body, NonTestFiles: []string{"pkg/planner/plan.go"}}
				save := func(detail model.PendingPRDetail) {
					t.Helper()
					if err := st.SyncFetchedPRDetails(map[string]model.PendingPRDetail{"pr": detail}); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := func() (model.ReviewVersion, string) {
					t.Helper()
					if mode == "backfill" {
						recs, err := st.ListBackfillNoteCandidates(10, "")
						if err != nil {
							t.Fatal(err)
						}
						for _, r := range recs {
							if r.Number == 100 {
								return *r.Version, r.PRDetails[0].BodyExcerpt
							}
						}
						t.Fatal("missing backfill candidate")
					}
					snap, err := st.LoadReviewSnapshot(false, true, []int{100})
					if err != nil {
						t.Fatal(err)
					}
					return *snap.Retriage[0].Version, snap.Retriage[0].PRDetails[0].BodyExcerpt
				}
				save(d)
				fact := d.Facts()
				fact.UpdatedAt = "2026-11-17T01:00:00Z"
				if err := st.UpsertPRStatuses([]model.PRStatus{fact}, time.Time{}); err != nil {
					t.Fatal(err)
				}
				before, hidden := snapshot()
				if hidden != "" {
					t.Fatal("old prose was not hidden")
				}
				staleDetail := d
				d.UpdatedAt = fact.UpdatedAt
				save(d)
				after, visible := snapshot()
				if visible != body || after.Notes != before.Notes {
					t.Fatalf("wrong evidence or note version: %+v body=%q", after, visible)
				}
				if body != "" {
					if after.Review <= before.Review {
						t.Fatal("restored prose did not invalidate reviewed evidence")
					}
					if err := st.ApplyIssueOverride(100, reviewPatch("HARD", &before)); !errors.Is(err, ErrOverrideConflict) {
						t.Fatalf("decision based on hidden prose was accepted: %v", err)
					}
				} else if after != before {
					t.Fatal("unchanged visible evidence advanced review version")
				}
				save(d)
				save(staleDetail)
				again, visible := snapshot()
				if again != after || visible != body {
					t.Fatal("duplicate or stale fetch changed evidence/version")
				}
			})
		}
	}
}
