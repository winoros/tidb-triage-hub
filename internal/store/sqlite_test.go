package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

func TestStoreOverrideSurvivalAndPendingResolution(t *testing.T) {
	tmp := t.TempDir()
	seedPath := filepath.Join(tmp, "seed.json")
	seedJSON := `{
  "sync_meta": {
    "schema_version": 2,
    "supported_locales": ["zh-CN", "en"],
    "snapshot_utc": "2026-11-12T11:03:47Z",
    "snapshot_utc8": "2026-11-12 19:03 (UTC+8)",
    "max_issue_updated_utc": "2026-11-12T10:06:19Z",
    "max_issue_updated_utc8": "2026-11-12 18:06 (UTC+8)",
    "incremental_gh_search_qualifier": "updated:>=2026-11-12T10:06:19Z",
    "total_candidates": 1,
    "canonical_candidates": 1,
    "real_bugs": 1
  },
  "enums": {
    "categories": ["EASY", "MEDIUM", "HARD"],
    "severities": ["CRITICAL", "MAJOR", "MODERATE", "MINOR", "N/A"],
    "subsystems": ["statistics", "logical_rewrite"]
  },
  "records": [
    {
      "number": 73063,
      "title": "planner: composite prefix index drops residual OR filter",
      "author": "winoros",
      "assignees": [],
      "created": "2026-11-12",
      "created_at": "2026-11-12T08:17:27Z",
      "updated_at": "2026-11-12T08:39:13Z",
      "updated_display": "2026-11-12 16:39",
      "labels": ["type/bug", "sig/planner", "severity/major"],
      "is_canonical": true,
      "is_customer": false,
      "is_borderline_kept": false,
      "cat": "MEDIUM",
      "diff_rank": 2,
      "severity": "MAJOR",
      "sev_rank": 3,
      "gh_severity": "major",
      "subsystem": "access_path_partition_lock",
      "note": {"zh-CN": "原中文备注", "en": "Original EN note"},
      "prs": [
        {
          "num": 73064,
          "repo": "pingcap/tidb",
          "label": "#73064",
          "url": "https://github.com/pingcap/tidb/pull/73064",
          "state": "OPEN",
          "title": "planner: preserve residual filters",
          "is_cherry_pick": false,
          "updated_at": "2026-11-12T09:32:19Z"
        }
      ],
      "has_pr": true,
      "has_open_pr": true,
      "has_any_open_pr": true,
      "has_merged_pr": false
    }
  ]
}`
	if err := os.WriteFile(seedPath, []byte(seedJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := Open(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seeded, err := st.BootstrapIfEmpty(seedPath)
	if err != nil || !seeded {
		t.Fatalf("bootstrap failed: seeded=%v err=%v", seeded, err)
	}

	// Patch override
	updated, err := st.SaveIssueOverride(73063, model.TriagePatchRequest{
		Cat:              "EASY",
		Severity:         "CRITICAL",
		Subsystem:        "access_path_partition_lock",
		IsBorderlineKept: true,
		ForceKeep:        true,
		Note:             &model.LocalizedNote{ZhCN: "人工校准中文", En: "Manual EN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Cat != "EASY" || updated.Severity != "CRITICAL" || !updated.ManualOverride || !updated.ForceKeep {
		t.Fatalf("unexpected patched record: %+v", updated)
	}

	// Simulate subsequent sync ingesting baseline record + 2 pending candidates
	dsBase, err := st.BuildDataset(false)
	if err != nil {
		t.Fatal(err)
	}
	dsBase.Records[0].Cat = "MEDIUM"
	dsBase.Records[0].Severity = "MAJOR"
	dsBase.Records[0].UpdatedAt = "2026-11-12T12:00:00Z"

	pending := []model.PendingCandidate{
		{
			Number:        73100,
			Title:         "planner: new bug candidate",
			Author:        "alice",
			CreatedAt:     "2026-11-12T11:00:00Z",
			UpdatedAt:     "2026-11-12T11:30:00Z",
			Labels:        []string{"sig/planner", "type/bug"},
			MatchedScopes: []string{"canonical_planner_bug"},
		},
		{
			Number:        73101,
			Title:         "br: backup timeout",
			Author:        "bob",
			CreatedAt:     "2026-11-12T11:10:00Z",
			UpdatedAt:     "2026-11-12T11:20:00Z",
			Labels:        []string{"type/bug"},
			MatchedScopes: []string{"untriaged_2026_bug"},
		},
	}
	if err := st.IngestSyncedDataset(dsBase, pending); err != nil {
		t.Fatal(err)
	}

	// Verify override survived sync while GitHub updated_at refreshed!
	afterSync, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	if afterSync.Records[0].Cat != "EASY" || afterSync.Records[0].Severity != "CRITICAL" || afterSync.Records[0].UpdatedAt != "2026-11-12T12:00:00Z" {
		t.Fatalf("override did not survive sync cleanly: %+v", afterSync.Records[0])
	}

	// Accept #73100 and exclude #73101
	if err := st.ResolvePendingCandidate(73100, model.PendingResolveRequest{
		Action:    "accept",
		Cat:       "EASY",
		Severity:  "MAJOR",
		Subsystem: "logical_rewrite",
		Note:      &model.LocalizedNote{ZhCN: "新单中文", En: "New EN"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolvePendingCandidate(73101, model.PendingResolveRequest{
		Action:        "exclude",
		ExcludeReason: "BR module issue",
	}); err != nil {
		t.Fatal(err)
	}

	remPending, err := st.ListPendingCandidates()
	if err != nil || len(remPending) != 0 {
		t.Fatalf("expected 0 pending candidates, got %d (err=%v)", len(remPending), err)
	}

	finalDS, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalDS.Records) != 2 || finalDS.Records[0].Number != 73100 {
		t.Fatalf("expected #73100 immediately added to dataset, got %d records", len(finalDS.Records))
	}
	if len(finalDS.SyncMeta.ExcludedIssueNums) != 1 || finalDS.SyncMeta.ExcludedIssueNums[0] != 73101 {
		t.Fatalf("expected #73101 in ExcludedIssueNums, got %v", finalDS.SyncMeta.ExcludedIssueNums)
	}

	// Reset override on #73063 restores baseline MEDIUM/MAJOR
	reverted, err := st.DeleteIssueOverride(73063)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Cat != "MEDIUM" || reverted.Severity != "MAJOR" || reverted.ManualOverride {
		t.Fatalf("expected reverted baseline MEDIUM/MAJOR, got %+v", reverted)
	}
}

func TestBatchTriageAndStandalonePRTargets(t *testing.T) {
	tmp := t.TempDir()
	seedPath := filepath.Join(tmp, "seed.json")
	seedJSON := `{
  "sync_meta": {
    "schema_version": 2,
    "supported_locales": ["zh-CN", "en"],
    "snapshot_utc": "2026-11-12T11:03:47Z",
    "snapshot_utc8": "2026-11-12 19:03 (UTC+8)",
    "max_issue_updated_utc": "2026-11-12T10:06:19Z",
    "max_issue_updated_utc8": "2026-11-12 18:06 (UTC+8)",
    "incremental_gh_search_qualifier": "updated:>=2026-11-12T10:06:19Z",
    "pr_snapshot_utc": "2026-11-12T11:05:00Z",
    "pr_snapshot_utc8": "2026-11-12 19:05 (UTC+8)",
    "total_candidates": 1,
    "canonical_candidates": 1,
    "real_bugs": 1
  },
  "enums": {
    "categories": ["EASY", "MEDIUM", "HARD"],
    "severities": ["CRITICAL", "MAJOR", "MODERATE", "MINOR", "N/A"],
    "subsystems": ["statistics", "logical_rewrite"]
  },
  "records": [
    {
      "number": 73063,
      "title": "planner: composite prefix index drops residual OR filter",
      "author": "winoros",
      "assignees": [],
      "created": "2026-11-12",
      "created_at": "2026-11-12T08:17:27Z",
      "updated_at": "2026-11-12T08:39:13Z",
      "updated_display": "2026-11-12 16:39",
      "labels": ["type/bug", "sig/planner"],
      "is_canonical": true,
      "cat": "MEDIUM",
      "diff_rank": 2,
      "severity": "MAJOR",
      "sev_rank": 3,
      "gh_severity": "unlabeled",
      "subsystem": "logical_rewrite",
      "note": {"zh-CN": "基线备注", "en": "Baseline note"},
      "prs": [
        {
          "num": 73064,
          "repo": "pingcap/tidb",
          "label": "#73064",
          "url": "https://github.com/pingcap/tidb/pull/73064",
          "state": "OPEN",
          "title": "planner: preserve residual filters",
          "is_cherry_pick": false,
          "updated_at": "2026-11-12T09:32:19Z"
        }
      ],
      "has_pr": true,
      "has_open_pr": true,
      "has_any_open_pr": true
    }
  ],
  "pr_status": {
    "pingcap/tidb#74000": {
      "repo": "pingcap/tidb",
      "num": 74000,
      "label": "#74000",
      "url": "https://github.com/pingcap/tidb/pull/74000",
      "title": "planner: standalone refactor",
      "author": "hawkingrei",
      "state": "OPEN",
      "mergeable": "MERGEABLE",
      "updated_at": "2026-11-12T10:00:00Z",
      "updated_display": "2026-11-12 18:00",
      "blocker_stage": "WAITING_REVIEWER",
      "ball_owner": "REVIEWER",
      "track_sources": ["planner_label"]
    }
  }
}`
	if err := os.WriteFile(seedPath, []byte(seedJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := Open(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.BootstrapIfEmpty(seedPath); err != nil {
		t.Fatal(err)
	}

	// 1. ListOpenPRTargets unions linked PR #73064 + standalone PR #74000 and preserves TrackSources
	targets, err := st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 open PR targets, got %d (%+v)", len(targets), targets)
	}
	if targets[0].Number != 74000 || len(targets[0].TrackSources) != 1 || targets[0].TrackSources[0] != "planner_label" {
		t.Fatalf("expected standalone PR #74000 with planner_label track source, got %+v", targets[0])
	}

	// 2. Ingest pending candidates and apply batch AI triage decisions
	dsBase, err := st.BuildDataset(false)
	if err != nil {
		t.Fatal(err)
	}
	pending := []model.PendingCandidate{
		{
			Number:        73200,
			Title:         "planner: index merge panic on virtual column",
			Author:        "carol",
			Assignees:     []string{"winoros", "ailinkid"},
			CreatedAt:     "2026-11-12T12:00:00Z",
			UpdatedAt:     "2026-11-12T13:30:00Z",
			Labels:        []string{"sig/planner", "type/bug", "report/customer"},
			MatchedScopes: []string{"canonical_planner_bug"},
			PRs: []model.LinkedPRRef{
				{
					Num:       11200,
					Repo:      "pingcap/tiflash",
					Label:     "pingcap/tiflash#11200",
					URL:       "https://github.com/pingcap/tiflash/pull/11200",
					State:     "OPEN",
					Title:     "tiflash: guard virtual column projection",
					UpdatedAt: "2026-11-12T13:25:00Z",
				},
			},
		},
		{
			Number:        73201,
			Title:         "tikv: raftstore lease slow",
			Author:        "dave",
			CreatedAt:     "2026-11-12T12:10:00Z",
			UpdatedAt:     "2026-11-12T12:20:00Z",
			Labels:        []string{"type/bug"},
			MatchedScopes: []string{"untriaged_2026_bug"},
		},
	}
	if err := st.IngestSyncedDataset(dsBase, pending); err != nil {
		t.Fatal(err)
	}

	bRes, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"73200": {
				Cat:       "EASY",
				Severity:  "CRITICAL",
				Subsystem: "statistics",
				Note:      &model.LocalizedNote{ZhCN: "AI 初筛备注", En: "Initial AI triage note"},
			},
		},
		ExcludeIssues: map[string]string{
			"#73201": "Storage/TiKV issue",
		},
		Overrides: map[string]model.TriagePatchRequest{
			"73063": {
				Cat:       "HARD",
				Severity:  "CRITICAL",
				Subsystem: "logical_rewrite",
				Note:      &model.LocalizedNote{ZhCN: "人工复核", En: "Human review"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ApplyBatchTriageDecisions failed: %v", err)
	}
	if bRes.RemainingPending != 0 || len(bRes.AcceptedIssues) != 1 || len(bRes.ExcludedIssues) != 1 || len(bRes.OverriddenIssues) != 1 {
		t.Fatalf("unexpected batch result: %+v", bRes)
	}
	// An explicit sync should resolve newly linked uncached PRs.
	foundTiflash := false
	targets, err = st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	for _, mt := range targets {
		if mt.Repo == "pingcap/tiflash" && mt.Number == 11200 {
			foundTiflash = true
		}
	}
	if !foundTiflash {
		t.Fatalf("expected pingcap/tiflash#11200 in sync targets, got %+v", targets)
	}

	dsAfter, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	if dsAfter.SyncMeta.MaxIssueUpdatedUTC != "2026-11-12T13:30:00Z" || dsAfter.SyncMeta.SnapshotUTC != "2026-11-12T11:03:47Z" {
		t.Fatalf("expected MaxIssueUpdatedUTC=2026-11-12T13:30:00Z and SnapshotUTC preserved at 2026-11-12T11:03:47Z, got max=%s snap=%s", dsAfter.SyncMeta.MaxIssueUpdatedUTC, dsAfter.SyncMeta.SnapshotUTC)
	}
	var rec73200, rec73063 model.IssueRecord
	for _, r := range dsAfter.Records {
		if r.Number == 73200 {
			rec73200 = r
		}
		if r.Number == 73063 {
			rec73063 = r
		}
	}
	if len(rec73200.Assignees) != 2 || rec73200.Assignees[0] != "winoros" {
		t.Fatalf("expected assignees preserved on #73200, got %v", rec73200.Assignees)
	}
	if rec73200.ManualOverride {
		t.Fatalf("expected initial AI triage (#73200) ManualOverride=false, got true")
	}
	if !rec73063.ManualOverride {
		t.Fatalf("expected overridden #73063 ManualOverride=true, got false")
	}

	// Subsequent human edit on #73200 sets ManualOverride=true, and DeleteIssueOverride reverts to AI baseline
	cal, err := st.SaveIssueOverride(73200, model.TriagePatchRequest{
		Cat:       "MEDIUM",
		Severity:  "MAJOR",
		Subsystem: "statistics",
		Note:      &model.LocalizedNote{ZhCN: "人工微调", En: "Human tweak"},
	})
	if err != nil || !cal.ManualOverride || cal.Cat != "MEDIUM" {
		t.Fatalf("expected calibrated #73200, got %+v (err=%v)", cal, err)
	}
	rev, err := st.DeleteIssueOverride(73200)
	if err != nil || rev.ManualOverride || rev.Cat != "EASY" || rev.Severity != "CRITICAL" {
		t.Fatalf("expected #73200 reverted to initial AI baseline EASY/CRITICAL, got %+v (err=%v)", rev, err)
	}
}

func TestApplyIssueSyncThreeTierPRsArchiveAndRetriage(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "hub.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seed := model.Dataset{
		SyncMeta: model.SyncMeta{
			SchemaVersion:      2,
			SnapshotUTC:        "2026-11-12T10:00:00Z",
			MaxIssueUpdatedUTC: "2026-11-12T09:50:00Z",
		},
		Records: []model.IssueRecord{
			{
				Number:      65653,
				Title:       "outer-join null-reject issue",
				Author:      "user1",
				Created:     "2026-06-01",
				CreatedAt:   "2026-06-01T00:00:00Z",
				UpdatedAt:   "2026-11-10T00:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				GHSeverity:  "major",
				Subsystem:   "logical_rewrite",
				PRs: []model.LinkedPRRef{
					// Historical static link with empty updated_at -> should become source='manual'
					{Num: 67129, Repo: "pingcap/tidb", Label: "#67129", URL: "https://github.com/pingcap/tidb/pull/67129", State: "MERGED", Title: "planner: fix", UpdatedAt: ""},
					// Normal timeline link -> should be removed if unlinked later
					{Num: 68000, Repo: "pingcap/tidb", Label: "#68000", URL: "https://github.com/pingcap/tidb/pull/68000", State: "OPEN", Title: "wip", UpdatedAt: "2026-11-10T00:00:00Z"},
				},
			},
			{
				Number:      72999,
				Title:       "supplemental non-opt dashboard issue",
				Author:      "user2",
				Created:     "2026-11-01",
				CreatedAt:   "2026-11-01T00:00:00Z",
				UpdatedAt:   "2026-11-10T00:00:00Z",
				Labels:      []string{"component/mview", "type/bug"},
				IsCanonical: false,
				Cat:         "FILTER_NON_OPT_OR_FIXED",
				Severity:    "N/A",
				GHSeverity:  "minor",
				Subsystem:   "logical_rewrite",
				PRs: []model.LinkedPRRef{
					{Num: 73500, Repo: "pingcap/tidb", Label: "#73500", URL: "https://github.com/pingcap/tidb/pull/73500", State: "OPEN", Title: "ui: dashboard fix", UpdatedAt: "2026-11-10T00:00:00Z"},
				},
			},
		},
	}
	if err := st.IngestSyncedDataset(seed, nil); err != nil {
		t.Fatalf("ingest seed: %v", err)
	}

	// Day-1 zero false positives on retriage
	dsInit, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsInit.SyncMeta.RetriageIssueNums) != 0 {
		t.Fatalf("expected 0 Day-1 retriage issues, got %v", dsInit.SyncMeta.RetriageIssueNums)
	}

	// First resolution includes every associated incomplete PR, even filtered or merged ones.
	openTargets, err := st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(openTargets) != 3 || openTargets[0].Number != 73500 || openTargets[1].Number != 68000 || openTargets[2].Number != 67129 {
		t.Fatalf("expected all three incomplete associated PRs, got %+v", openTargets)
	}

	// Sync #1:
	// - #65653 timeline unlinks #68000 (so timeline is empty), Stream B adds #69000 ('pr_ref'), and gains report/customer!
	// - #72999 closes (archived into archived_issues)!
	dsAfter1, err := st.ApplyIssueSync(issuesync_SyncPlanForTest())
	if err != nil {
		t.Fatalf("ApplyIssueSync #1: %v", err)
	}
	if len(dsAfter1.Records) != 1 || dsAfter1.Records[0].Number != 65653 {
		t.Fatalf("expected only #65653 active after #72999 closed, got %d", len(dsAfter1.Records))
	}
	rec65653 := dsAfter1.Records[0]
	// Manual #67129 and Stream B #69000 must both be present, while unlinked timeline #68000 is gone!
	if len(rec65653.PRs) != 2 || rec65653.PRs[0].Num != 69000 || rec65653.PRs[1].Num != 67129 {
		t.Fatalf("expected PRs [#69000 (pr_ref), #67129 (manual)], got %+v", rec65653.PRs)
	}
	if !rec65653.NeedsRetriage {
		t.Fatalf("expected #65653 NeedsRetriage=true after new fix PR + customer_reported, got false")
	}

	// Acknowledge retriage on #65653 -> clears NeedsRetriage while preserving MEDIUM/MAJOR
	acked, err := st.AcknowledgeIssueRetriage(65653, "maintainer")
	if err != nil {
		t.Fatalf("AcknowledgeIssueRetriage: %v", err)
	}
	if acked.NeedsRetriage || acked.Cat != "MEDIUM" {
		t.Fatalf("expected NeedsRetriage=false after ack, got %+v", acked)
	}

	// UpsertPRStatuses transitions #69000 to MERGED while #65653 is still OPEN -> triggers fix_pr_merged_still_open!
	if err := st.UpsertPRStatuses([]model.PRStatus{
		{
			Repo:      "pingcap/tidb",
			Num:       69000,
			Label:     "#69000",
			URL:       "https://github.com/pingcap/tidb/pull/69000",
			Title:     "planner: fix 65653",
			State:     "MERGED",
			UpdatedAt: "2026-11-13T01:00:00Z",
		},
	}, time.Time{}); err != nil {
		t.Fatalf("UpsertPRStatuses: %v", err)
	}
	retriageCands, err := st.ListRetriageCandidates(true, nil)
	if err != nil || len(retriageCands) != 1 || retriageCands[0].Reasons[0] != model.RetriageFixPRMergedOpen {
		t.Fatalf("expected fix_pr_merged_still_open on #65653, got %+v (err=%v)", retriageCands, err)
	}

	// Canonical issue #65653 cannot be excluded
	if err := st.ExcludeExistingIssue(65653, "try exclude canonical", false); err == nil {
		t.Fatal("expected canonical issue #65653 exclusion to be blocked")
	}

	// Sync #2: Reopen archived #72999 -> restores with original FILTER_NON_OPT_OR_FIXED triage, and can then be excluded & restored
	dsAfter2, err := st.ApplyIssueSync(issuesync_ReopenPlanForTest())
	if err != nil {
		t.Fatalf("ApplyIssueSync #2 (reopen): %v", err)
	}
	if len(dsAfter2.Records) != 2 {
		t.Fatalf("expected #72999 restored on reopen, got %d records", len(dsAfter2.Records))
	}
	if err := st.ExcludeExistingIssue(72999, "Dashboard UI issue", false); err != nil {
		t.Fatalf("expected non-canonical #72999 exclude to succeed, got %v", err)
	}
	exList, err := st.ListExcludedIssues()
	if err != nil || len(exList) != 1 || exList[0].Number != 72999 || len(exList[0].ExcludedLabels) == 0 {
		t.Fatalf("expected #72999 in ListExcludedIssues with snapshot labels, got %+v (err=%v)", exList, err)
	}
	if err := st.RestoreExcludedIssue(72999); err != nil {
		t.Fatalf("RestoreExcludedIssue: %v", err)
	}
}

func issuesync_SyncPlanForTest() issuesync.SyncPlan {
	return issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2026-11-13T00:00:00Z",
		UpdatedIssues: []issuesync.LiveIssueUpdate{
			{
				Number:      65653,
				Title:       "outer-join null-reject issue",
				Author:      "user1",
				Assignees:   []string{"winoros"},
				Created:     "2026-06-01",
				CreatedAt:   "2026-06-01T00:00:00Z",
				UpdatedAt:   "2026-11-12T23:50:00Z",
				Labels:      []string{"sig/planner", "type/bug", "report/customer"},
				IsCanonical: true,
				IsCustomer:  true,
				GHSeverity:  "major",
				TimelinePRs: nil, // #68000 unlinked!
			},
		},
		StreamBPRRefs: map[int][]model.LinkedPRRef{
			65653: {
				{Num: 69000, Repo: "pingcap/tidb", Label: "#69000", URL: "https://github.com/pingcap/tidb/pull/69000", State: "OPEN", Title: "planner: fix 65653", UpdatedAt: "2026-11-12T23:55:00Z"},
			},
		},
		ClosedRemovals: []model.DeltaClosedIssue{
			{Number: 72999, PrevCat: "FILTER_NON_OPT_OR_FIXED", PrevSeverity: "N/A", Reason: "Closed (COMPLETED)"},
		},
	}
}

func issuesync_ReopenPlanForTest() issuesync.SyncPlan {
	return issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2026-11-13T02:00:00Z",
		RestoredIssues: []issuesync.LiveIssueUpdate{
			{
				Number:      72999,
				Title:       "supplemental non-opt dashboard issue",
				Author:      "user2",
				Created:     "2026-11-01",
				CreatedAt:   "2026-11-01T00:00:00Z",
				UpdatedAt:   "2026-11-13T01:50:00Z",
				Labels:      []string{"component/mview", "type/bug"},
				IsCanonical: false,
				IsCustomer:  false,
				GHSeverity:  "minor",
			},
		},
	}
}

func TestWatermarkIntegrityOverrideSemanticsAndAtomicBatch(t *testing.T) {
	tmp := t.TempDir()
	st, err := Open(filepath.Join(tmp, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seed := model.Dataset{
		SyncMeta: model.SyncMeta{
			SchemaVersion:      2,
			SnapshotUTC:        "2020-05-01T10:00:00Z",
			MaxIssueUpdatedUTC: "2020-05-01T09:50:00Z",
		},
		Records: []model.IssueRecord{
			{
				Number:           75001,
				Title:            "planner: correlated subquery wrong result",
				Author:           "reporter",
				Created:          "2020-05-01",
				CreatedAt:        "2020-05-01T08:00:00Z",
				UpdatedAt:        "2020-05-01T09:50:00Z",
				Labels:           []string{"sig/planner", "type/bug"},
				IsCanonical:      true,
				IsBorderlineKept: true,
				ForceKeep:        true,
				Cat:              "MEDIUM",
				Severity:         "MAJOR",
				GHSeverity:       "major",
				Subsystem:        "logical_rewrite",
				Note:             &model.LocalizedNote{ZhCN: "原始备注", En: "Original note"},
				PRs: []model.LinkedPRRef{
					{Num: 75101, Repo: "pingcap/tidb", Label: "#75101", URL: "https://github.com/pingcap/tidb/pull/75101", State: "OPEN", Title: "planner: timeline pr", UpdatedAt: "2020-05-01T09:45:00Z"},
				},
			},
			{
				Number:      75002,
				Title:       "non-opt dashboard issue",
				Author:      "reporter2",
				Created:     "2020-05-01",
				CreatedAt:   "2020-05-01T08:10:00Z",
				UpdatedAt:   "2020-05-01T09:40:00Z",
				Labels:      []string{"component/mview", "type/bug"},
				IsCanonical: false,
				Cat:         "FILTER_NON_OPT_OR_FIXED",
				Severity:    "N/A",
				GHSeverity:  "minor",
				Subsystem:   "logical_rewrite",
				PRs: []model.LinkedPRRef{
					{Num: 75999, Repo: "pingcap/tidb", Label: "#75999", URL: "https://github.com/pingcap/tidb/pull/75999", State: "OPEN", Title: "ui: fix", UpdatedAt: "2020-05-01T09:40:00Z"},
				},
			},
		},
	}
	if err := st.IngestSyncedDataset(seed, nil); err != nil {
		t.Fatalf("ingest seed: %v", err)
	}

	// 1. Partial JSON override omitting note, is_borderline_kept, and force_keep preserves all three!
	var partialPatch model.TriagePatchRequest
	if err := json.Unmarshal([]byte(`{"cat":"HARD","severity":"CRITICAL","subsystem":"logical_rewrite"}`), &partialPatch); err != nil {
		t.Fatal(err)
	}
	rec, err := st.SaveIssueOverride(75001, partialPatch)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Cat != "HARD" || !rec.IsBorderlineKept || !rec.ForceKeep || rec.Note == nil || rec.Note.En != "Original note" {
		t.Fatalf("expected partial JSON override to preserve borderline, force_keep, and note, got %+v", rec)
	}

	// 2. Watermark integrity (#1): SaveIssueOverride ran at wall-clock ~2026, while seed timestamps are in 2020!
	//    A subsequent sync bringing a maintainer comment at 2020-05-01T10:30:00Z (well BEFORE wall-clock time!)
	//    MUST still trigger substantive_new_comments AND SnapshotUTC before sync must still be 2020-05-01T10:00:00Z!
	preState, err := st.LoadSyncState()
	if err != nil {
		t.Fatal(err)
	}
	if preState.PrevMeta.SnapshotUTC != "2020-05-01T10:00:00Z" {
		t.Fatalf("expected SnapshotUTC unpoisoned at 2020-05-01T10:00:00Z, got %s", preState.PrevMeta.SnapshotUTC)
	}

	dsSync1, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T11:00:00Z",
		UpdatedIssues: []issuesync.LiveIssueUpdate{
			{
				Number:      75001,
				Title:       "planner: correlated subquery wrong result",
				Author:      "reporter",
				Created:     "2020-05-01",
				CreatedAt:   "2020-05-01T08:00:00Z",
				UpdatedAt:   "2020-05-01T10:30:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				GHSeverity:  "major",
				TimelinePRs: []model.LinkedPRRef{
					{Num: 75101, Repo: "pingcap/tidb", Label: "#75101", URL: "https://github.com/pingcap/tidb/pull/75101", State: "OPEN", Title: "planner: timeline pr", UpdatedAt: "2020-05-01T10:25:00Z", Source: model.LinkSourceTimeline},
				},
				HasComments: true,
				RecentComments: []model.PendingComment{
					{Author: "maintainer", CreatedAt: "2020-05-01T10:30:00Z", Body: "Root cause is in `decorrelateSolver` when outer join has null-rejected predicate."},
				},
			},
		},
		// Stream B also sees #75101 AND adds pr_ref #75102 -> #75101 must remain 'timeline' (not downgraded to 'pr_ref')!
		StreamBPRRefs: map[int][]model.LinkedPRRef{
			75001: {
				{Num: 75101, Repo: "pingcap/tidb", Label: "#75101", URL: "https://github.com/pingcap/tidb/pull/75101", State: "OPEN", Title: "planner: timeline pr", UpdatedAt: "2020-05-01T10:25:00Z", Source: model.LinkSourcePRRef},
				{Num: 75102, Repo: "pingcap/tidb", Label: "#75102", URL: "https://github.com/pingcap/tidb/pull/75102", State: "OPEN", Title: "planner: pr_ref pr", UpdatedAt: "2020-05-01T10:28:00Z", Source: model.LinkSourcePRRef},
			},
		},
	})
	if err != nil {
		t.Fatalf("ApplyIssueSync 1: %v", err)
	}
	var r75001 model.IssueRecord
	for _, r := range dsSync1.Records {
		if r.Number == 75001 {
			r75001 = r
		}
	}
	hasCommentTrigger := false
	for _, rc := range r75001.RetriageReasons {
		if rc == model.RetriageSubstantiveComments {
			hasCommentTrigger = true
		}
	}
	if !hasCommentTrigger {
		t.Fatalf("expected substantive_new_comments trigger (proving comment_watermark_utc was not poisoned by wall-clock time), got %v", r75001.RetriageReasons)
	}

	// Ack retriage while override is active, then reset override -> DeleteIssueOverride restores pre-override auto_baseline_json,
	// which predates #75102 and the maintainer comment, so NeedsRetriage re-triggers!
	if _, err := st.AcknowledgeIssueRetriage(75001, "maintainer"); err != nil {
		t.Fatal(err)
	}
	afterReset, err := st.DeleteIssueOverride(75001)
	if err != nil {
		t.Fatal(err)
	}
	if !afterReset.NeedsRetriage || afterReset.Cat != "MEDIUM" {
		t.Fatalf("expected DeleteIssueOverride to restore pre-override baseline and re-flag retriage, got %+v", afterReset)
	}

	// 3. Stream B unlink (#3): PR #75102 edits out its issue ref -> UnlinkedStreamBPRs removes #75102 while #75101 ('timeline') survives!
	dsSync2, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T12:00:00Z",
		UnlinkedStreamBPRs: map[int][]string{
			75001: {"pingcap/tidb#75101", "pingcap/tidb#75102"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyIssueSync 2 (unlink): %v", err)
	}
	for _, r := range dsSync2.Records {
		if r.Number == 75001 {
			if len(r.PRs) != 1 || r.PRs[0].Num != 75101 || r.PRs[0].Source != model.LinkSourceTimeline {
				t.Fatalf("expected #75101 ('timeline') preserved and #75102 ('pr_ref') deleted, got %+v", r.PRs)
			}
		}
	}

	// 4. Atomic batch rollback + ordering (#8):
	//    a) Batch attempting to override #75001 AND exclude canonical #75001 must roll back #75001 override!
	_, err = st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		Overrides: map[string]model.TriagePatchRequest{
			"75001": {Cat: "EASY", Severity: "CRITICAL", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "回滚", En: "Rollback"}},
		},
		ExcludeIssues: map[string]string{
			"75001": "cannot exclude canonical",
		},
	})
	if err == nil {
		t.Fatal("expected error when batch attempts to exclude canonical issue #75001")
	}
	dsCheckRollback, _ := st.BuildDataset(true)
	for _, r := range dsCheckRollback.Records {
		if r.Number == 75001 && r.ManualOverride {
			t.Fatal("expected atomic batch rollback, but #75001 override persisted")
		}
	}

	//    b) ManualPRLinks + AckRetriage in same batch: ManualPRLinks runs BEFORE AckRetriage so #75001 ends with NeedsRetriage=false!
	bRes, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		ManualPRLinks: map[string][]string{
			"75001": {"75103"},
		},
		AckRetriage: model.FlexIntList{75001},
	})
	if err != nil {
		t.Fatalf("ApplyBatchTriageDecisions (manual link + ack): %v", err)
	}
	if bRes.ManualPRsLinked != 1 || bRes.RemainingRetriage != 0 {
		t.Fatalf("expected ManualPRsLinked=1 and RemainingRetriage=0, got %+v", bRes)
	}

	// 5. Reopen baseline preservation (#5) & ClassifyTargetedPRSources (#9b):
	//    ClassifyTargetedPRSources for #75999 (linked only to FILTER_NON_OPT_OR_FIXED #75002) tags it ["issue_linked"], NOT ["manual_cli"]!
	classifiedTargets, err := st.ClassifyTargetedPRSources([]prstatus.PRTarget{{Repo: "pingcap/tidb", Number: 75999}})
	if err != nil {
		t.Fatal(err)
	}
	if len(classifiedTargets) != 1 || len(classifiedTargets[0].TrackSources) != 1 || classifiedTargets[0].TrackSources[0] != "issue_linked" {
		t.Fatalf("expected #75999 TrackSources=[issue_linked], got %+v", classifiedTargets)
	}

	//    Close #75001 then reopen after #75101 merged -> pre-close baseline is preserved so merged PR triggers fix_pr_merged_still_open!
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T13:00:00Z",
		ClosedRemovals: []model.DeltaClosedIssue{
			{Number: 75001, PrevCat: "MEDIUM", PrevSeverity: "MAJOR", Reason: "Closed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	dsReopened, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T14:00:00Z",
		RestoredIssues: []issuesync.LiveIssueUpdate{
			{
				Number:      75001,
				Title:       "planner: correlated subquery wrong result",
				Author:      "reporter",
				Created:     "2020-05-01",
				CreatedAt:   "2020-05-01T08:00:00Z",
				UpdatedAt:   "2020-05-01T13:55:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				GHSeverity:  "major",
				TimelinePRs: []model.LinkedPRRef{
					{Num: 75101, Repo: "pingcap/tidb", Label: "#75101", URL: "https://github.com/pingcap/tidb/pull/75101", State: "MERGED", Title: "planner: first attempt", UpdatedAt: "2020-05-01T12:55:00Z", Source: model.LinkSourceTimeline},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsReopened.Records {
		if r.Number == 75001 && !r.NeedsRetriage {
			t.Fatalf("expected reopened #75001 with merged PR #75101 to trigger NeedsRetriage=true, got %+v", r)
		}
	}

	//    Exclude & Restore #75002 queues RecheckNums in LoadSyncState()
	if err := st.ExcludeExistingIssue(75002, "mview UI", false); err != nil {
		t.Fatal(err)
	}
	if err := st.RestoreExcludedIssue(75002); err != nil {
		t.Fatal(err)
	}
	postRestoreState, err := st.LoadSyncState()
	if err != nil {
		t.Fatal(err)
	}
	if len(postRestoreState.RecheckNums) != 1 || postRestoreState.RecheckNums[0] != 75002 {
		t.Fatalf("expected RecheckNums=[75002] after RestoreExcludedIssue, got %v", postRestoreState.RecheckNums)
	}

	// 6. Round-2 regressions:
	//    a) Single-locale note is preserved as-is in BuildDataset and still surfaced in ListBackfillNoteCandidates!
	if err := st.ResolvePendingCandidate(75901, model.PendingResolveRequest{Action: "accept"}); err == nil {
		t.Fatal("expected error for non-existent pending #75901")
	}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T15:00:00Z",
		UpsertPending: []model.PendingCandidate{
			{
				Number:    75010,
				Title:     "planner: partial note & concurrent accept test",
				Body:      "stack in pkg/planner/core/rule_predicate_push_down.go",
				Author:    "dev",
				CreatedAt: "2020-05-01T14:50:00Z",
				UpdatedAt: "2020-05-01T14:55:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolvePendingCandidate(75010, model.PendingResolveRequest{
		Action:    "accept",
		Cat:       "EASY",
		Severity:  "MAJOR",
		Subsystem: "logical_rewrite",
		Note:      &model.LocalizedNote{ZhCN: "", En: "Only English note filled"},
	}); err != nil {
		t.Fatal(err)
	}
	backfillList, err := st.ListBackfillNoteCandidates(10, "")
	if err != nil {
		t.Fatal(err)
	}
	foundPartialInBackfill := false
	for _, bf := range backfillList {
		if bf.Number == 75010 {
			foundPartialInBackfill = true
			if bf.Note == nil || bf.Note.ZhCN != "" || bf.Note.En != "Only English note filled" {
				t.Fatalf("expected single-locale note preserved without silent copy, got %+v", bf.Note)
			}
		}
	}
	if !foundPartialInBackfill {
		t.Fatal("expected issue #75010 with missing zh-CN note to appear in ListBackfillNoteCandidates")
	}

	//    b) Stale UpsertPending from concurrent PlanSync must NOT resurrect an already-accepted issue into pending_candidates!
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T15:05:00Z",
		UpsertPending: []model.PendingCandidate{
			{Number: 75010, Title: "stale concurrent upsert", UpdatedAt: "2020-05-01T14:55:00Z", Labels: []string{"sig/planner", "type/bug"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	pendAfterStale, _ := st.ListPendingCandidates()
	for _, pc := range pendAfterStale {
		if pc.Number == 75010 {
			t.Fatal("expected already-accepted #75010 not to be resurrected into pending_candidates")
		}
	}

	//    c) ManualPRLinks alone (without AckRetriage) must NOT self-flag new_fix_pr on #75010!
	bResManualOnly, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		ManualPRLinks: map[string][]string{
			"75010": {"75201"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyBatchTriageDecisions manual-only: %v", err)
	}
	dsAfterManual, _ := st.BuildDataset(true)
	for _, r := range dsAfterManual.Records {
		if r.Number == 75010 && r.NeedsRetriage {
			t.Fatalf("expected manual PR link #75201 on #75010 NOT to self-trigger retriage, got %v (res=%+v)", r.RetriageReasons, bResManualOnly)
		}
	}

	//    d) Archived issue with ENHANCEMENT override retains override Cat in LoadSyncState()
	if _, err := st.SaveIssueOverride(75010, model.TriagePatchRequest{
		Cat:       "ENHANCEMENT",
		Severity:  "N/A",
		Subsystem: "logical_rewrite",
		Note:      &model.LocalizedNote{ZhCN: "特性演进", En: "Enhancement"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{
		ModeUsed:    "incremental",
		SnapshotUTC: "2020-05-01T15:10:00Z",
		ClosedRemovals: []model.DeltaClosedIssue{
			{Number: 75010, PrevCat: "ENHANCEMENT", PrevSeverity: "N/A", Reason: "Closed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	archSyncState, err := st.LoadSyncState()
	if err != nil {
		t.Fatal(err)
	}
	if archSyncState.ArchivedIssues[75010].Cat != "ENHANCEMENT" {
		t.Fatalf("expected ArchivedIssues[75010].Cat == ENHANCEMENT from override, got %+v", archSyncState.ArchivedIssues[75010])
	}
}

func TestBootstrapFallbackBackupAndConflictSafeBatchApply(t *testing.T) {
	tmp := t.TempDir()
	fallbackJSON := []byte(`{
  "sync_meta": {
    "schema_version": 2,
    "snapshot_utc": "2026-11-12T10:00:00Z",
    "max_issue_updated_utc": "2026-11-12T09:50:00Z"
  },
  "records": [
    {
      "number": 76001,
      "title": "planner: issue calibrated on Web UI",
      "author": "u1",
      "created": "2026-11-12",
      "created_at": "2026-11-12T08:00:00Z",
      "updated_at": "2026-11-12T09:00:00Z",
      "labels": ["sig/planner", "type/bug"],
      "is_canonical": true,
      "cat": "MEDIUM",
      "severity": "MAJOR",
      "gh_severity": "major",
      "subsystem": "logical_rewrite",
      "note": {"zh-CN": "初始中文", "en": "Initial EN"}
    },
    {
      "number": 76002,
      "title": "planner: issue untouched on Web UI",
      "author": "u2",
      "created": "2026-11-12",
      "created_at": "2026-11-12T08:05:00Z",
      "updated_at": "2026-11-12T09:05:00Z",
      "labels": ["sig/planner", "type/bug"],
      "is_canonical": true,
      "cat": "MEDIUM",
      "severity": "MODERATE",
      "gh_severity": "moderate",
      "subsystem": "statistics",
      "note": {"zh-CN": "初始统计中文", "en": "Initial stats EN"}
    }
  ]
}`)

	st, err := Open(filepath.Join(tmp, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 1. BootstrapIfEmptyWithFallback falls back cleanly when seed path is missing
	seeded, err := st.BootstrapIfEmptyWithFallback(filepath.Join(tmp, "missing_seed.json"), fallbackJSON)
	if err != nil || !seeded {
		t.Fatalf("expected BootstrapIfEmptyWithFallback seeded=true, got seeded=%v err=%v", seeded, err)
	}

	// Queue two pending issues (#76101 and #76102)
	ds0, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	var triagedAt76001, triagedAt76002 string
	for _, r := range ds0.Records {
		if r.Number == 76001 {
			triagedAt76001 = r.TriagedAt
		}
		if r.Number == 76002 {
			triagedAt76002 = r.TriagedAt
		}
	}
	if err := st.IngestSyncedDataset(ds0, []model.PendingCandidate{
		{
			Number:    76101,
			Title:     "planner: resolved concurrently on Web UI",
			Author:    "alice",
			CreatedAt: "2026-11-12T10:10:00Z",
			UpdatedAt: "2026-11-12T10:15:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
		{
			Number:    76102,
			Title:     "planner: still pending for Machine B",
			Author:    "bob",
			CreatedAt: "2026-11-12T10:20:00Z",
			UpdatedAt: "2026-11-12T10:25:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
		{
			Number:    76103,
			Title:     "mview: non-canonical pending accepted on Web UI while Machine B excludes",
			Author:    "dave",
			CreatedAt: "2026-11-12T10:30:00Z",
			UpdatedAt: "2026-11-12T10:35:00Z",
			Labels:    []string{"type/bug"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	exportedAt := model.NowRFC3339Nano()
	time.Sleep(2 * time.Millisecond)

	// 2. Simulate concurrent Web UI activity AFTER Machine B exported pending:
	//    - Human accepts #76101 and #76103 on Web UI
	//    - Human calibrates #76001 on Web UI to HARD / CRITICAL
	if err := st.ResolvePendingCandidate(76101, model.PendingResolveRequest{
		Action:    "accept",
		Cat:       "HARD",
		Severity:  "CRITICAL",
		Subsystem: "logical_rewrite",
		Note:      &model.LocalizedNote{ZhCN: "网页人工入库", En: "Accepted via Web UI"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolvePendingCandidate(76103, model.PendingResolveRequest{
		Action:    "accept",
		Cat:       "MEDIUM",
		Severity:  "MAJOR",
		Subsystem: "logical_rewrite",
		Note:      &model.LocalizedNote{ZhCN: "网页人工保留非canonical", En: "Kept supplemental issue via Web UI"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveIssueOverride(76001, model.TriagePatchRequest{
		Cat:       "HARD",
		Severity:  "CRITICAL",
		Subsystem: "logical_rewrite",
		Note:      &model.LocalizedNote{ZhCN: "网页人工校准", En: "Calibrated via Web UI"},
	}); err != nil {
		t.Fatal(err)
	}

	batch := model.BatchTriageDecisions{
		BaseExportedAt: exportedAt,
		NewIssues: map[string]model.TriagePatchRequest{
			"76101": {
				Cat:       "EASY",
				Severity:  "MINOR",
				Subsystem: "statistics",
				Note:      &model.LocalizedNote{ZhCN: "机器B旧结果", En: "Stale Machine B result"},
			},
			"76102": {
				Cat:       "EASY",
				Severity:  "MAJOR",
				Subsystem: "statistics",
				Note:      &model.LocalizedNote{ZhCN: "机器B新入库", En: "Fresh Machine B accept"},
			},
		},
		ExcludeIssues: map[string]string{
			"76103": "Stale Machine B exclude that must NOT remove human-accepted #76103",
		},
		Overrides: map[string]model.TriagePatchRequest{
			"76001": {
				Cat:               "EASY",
				Severity:          "MINOR",
				Subsystem:         "logical_rewrite",
				Note:              &model.LocalizedNote{ZhCN: "机器B过期覆盖", En: "Stale Machine B override"},
				IfUnmodifiedSince: triagedAt76001,
			},
			"76002": {
				Cat:               "EASY",
				Severity:          "MAJOR",
				Subsystem:         "statistics",
				Note:              &model.LocalizedNote{ZhCN: "机器B正常覆盖", En: "Valid Machine B override"},
				IfUnmodifiedSince: triagedAt76002,
			},
		},
		ManualPRLinks: map[string][]string{
			"76101": {"79999"}, // must be skipped along with conflict-skipped #76101!
		},
	}

	// Strict mode (SkipConflicts=false) rejects stale batch (including BaseExportedAt conflicts)
	if _, err := st.ValidateBatchDecisionsWithOptions(batch, BatchApplyOptions{SkipConflicts: false}); err == nil {
		t.Fatal("expected strict ValidateBatchDecisionsWithOptions to fail on already-resolved pending #76101")
	}
	baseOnlyStale := model.BatchTriageDecisions{
		BaseExportedAt: exportedAt,
		ExcludeIssues:  map[string]string{"76103": "stale exclude"},
	}
	if _, err := st.ValidateBatchDecisionsWithOptions(baseOnlyStale, BatchApplyOptions{SkipConflicts: false}); err == nil {
		t.Fatal("expected strict ValidateBatchDecisionsWithOptions to fail on BaseExportedAt exclude conflict")
	}

	// Conflict-skip dry-run reports 4 conflicts (#76101 new_issues, #76103 exclude_issues, #76001 overrides, #76101 companion manual_pr_links) without mutating DB
	dryConflicts, err := st.ValidateBatchDecisionsWithOptions(batch, BatchApplyOptions{SkipConflicts: true})
	if err != nil {
		t.Fatalf("expected SkipConflicts validation to succeed, got %v", err)
	}
	if len(dryConflicts) != 4 {
		t.Fatalf("expected 4 skipped conflicts in dry-run, got %+v", dryConflicts)
	}

	// Genuine typo issue in new_issues still fails even with SkipConflicts=true
	typoBatch := model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"99999999": {Cat: "EASY", Severity: "MAJOR", Subsystem: "statistics"},
		},
	}
	if _, err := st.ValidateBatchDecisionsWithOptions(typoBatch, BatchApplyOptions{SkipConflicts: true}); err == nil {
		t.Fatal("expected unknown issue #99999999 to fail validation even with SkipConflicts=true")
	}

	// Live apply with SkipConflicts=true commits #76102 + #76002 while preserving Web UI #76101, #76103, and #76001!
	res, err := st.ApplyBatchTriageDecisionsWithOptions(batch, BatchApplyOptions{SkipConflicts: true})
	if err != nil {
		t.Fatalf("ApplyBatchTriageDecisionsWithOptions failed: %v", err)
	}
	if len(res.AcceptedIssues) != 1 || res.AcceptedIssues[0] != 76102 {
		t.Fatalf("expected AcceptedIssues=[76102], got %v", res.AcceptedIssues)
	}
	if len(res.ExcludedIssues) != 0 {
		t.Fatalf("expected ExcludedIssues=[] (stale #76103 skipped), got %v", res.ExcludedIssues)
	}
	if res.ManualPRsLinked != 0 {
		t.Fatalf("expected companion manual_pr_links on skipped #76101 to be skipped, got %d", res.ManualPRsLinked)
	}
	if len(res.OverriddenIssues) != 1 || res.OverriddenIssues[0] != 76002 {
		t.Fatalf("expected OverriddenIssues=[76002], got %v", res.OverriddenIssues)
	}
	if len(res.SkippedConflicts) != 4 {
		t.Fatalf("expected 4 SkippedConflicts, got %+v", res.SkippedConflicts)
	}

	dsFinal, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	byNum := make(map[int]model.IssueRecord)
	for _, r := range dsFinal.Records {
		byNum[r.Number] = r
	}
	if byNum[76101].Cat != "HARD" || byNum[76101].Note.En != "Accepted via Web UI" || len(byNum[76101].PRs) != 0 {
		t.Fatalf("expected Web UI accept on #76101 preserved without stale manual PR, got %+v", byNum[76101])
	}
	if byNum[76103].Cat != "MEDIUM" || byNum[76103].Note.En != "Kept supplemental issue via Web UI" {
		t.Fatalf("expected Web UI accept on #76103 preserved (not excluded), got %+v", byNum[76103])
	}
	if byNum[76001].Cat != "HARD" || byNum[76001].Note.En != "Calibrated via Web UI" {
		t.Fatalf("expected Web UI override on #76001 preserved, got %+v", byNum[76001])
	}
	if byNum[76002].Cat != "EASY" || byNum[76002].Note.En != "Valid Machine B override" {
		t.Fatalf("expected Machine B override on #76002 applied, got %+v", byNum[76002])
	}
	if byNum[76102].Cat != "EASY" {
		t.Fatalf("expected Machine B new issue #76102 accepted, got %+v", byNum[76102])
	}

	// DeleteIssueOverride advances triaged_at so stale overrides before reset are skipped
	beforeReset := model.NowRFC3339Nano()
	time.Sleep(2 * time.Millisecond)
	if _, err := st.DeleteIssueOverride(76002); err != nil {
		t.Fatalf("DeleteIssueOverride(#76002) failed: %v", err)
	}
	staleAfterReset, err := st.ValidateBatchDecisionsWithOptions(model.BatchTriageDecisions{
		BaseExportedAt: beforeReset,
		Overrides: map[string]model.TriagePatchRequest{
			"76002": {Cat: "HARD", Severity: "MAJOR", Subsystem: "statistics"},
		},
	}, BatchApplyOptions{SkipConflicts: true})
	if err != nil || len(staleAfterReset) != 1 {
		t.Fatalf("expected DeleteIssueOverride to advance triaged_at and skip stale override, got conflicts=%+v err=%v", staleAfterReset, err)
	}

	// 3. Online SQLite BackupTo rejects self-overwrite and produces a standalone valid snapshot
	if err := st.BackupTo(filepath.Join(tmp, "main.db")); err == nil {
		t.Fatal("expected BackupTo onto live DB path to fail")
	}
	backupPath := filepath.Join(tmp, "snapshots", "backup.db")
	if err := st.BackupTo(backupPath); err != nil {
		t.Fatalf("BackupTo failed: %v", err)
	}
	bakStore, err := Open(backupPath)
	if err != nil {
		t.Fatalf("Open(backupPath) failed: %v", err)
	}
	defer bakStore.Close()
	bakDS, err := bakStore.BuildDataset(true)
	if err != nil {
		t.Fatalf("bakStore.BuildDataset failed: %v", err)
	}
	if len(bakDS.Records) != len(dsFinal.Records) {
		t.Fatalf("expected %d records in backup DB, got %d", len(dsFinal.Records), len(bakDS.Records))
	}

	// 4. BootstrapIfEmptyWithFallback("", nil) leaves empty DB empty without error
	emptySt, err := Open(filepath.Join(tmp, "empty_no_seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer emptySt.Close()
	if seededEmpty, err := emptySt.BootstrapIfEmptyWithFallback("", nil); err != nil || seededEmpty {
		t.Fatalf("expected BootstrapIfEmptyWithFallback('', nil) -> (false, nil), got (%v, %v)", seededEmpty, err)
	}
}

func TestRound2SpecialDBPathSyncRacesReconcileAndTypoDetection(t *testing.T) {
	// 1. Open handles special URI characters (#, ?, %, spaces) in path and BackupTo works cleanly
	specialDir := filepath.Join(t.TempDir(), "dir #1?%20 space")
	specialDB := filepath.Join(specialDir, "hub#main?test.db")
	st, err := Open(specialDB)
	if err != nil {
		t.Fatalf("Open(specialDB) failed: %v", err)
	}
	defer st.Close()

	seed := model.Dataset{
		SyncMeta: model.SyncMeta{
			SchemaVersion:      2,
			SnapshotUTC:        "2026-11-12T10:00:00Z",
			MaxIssueUpdatedUTC: "2026-11-12T09:50:00Z",
			DeltaSincePrevSnapshot: &model.SnapshotDelta{
				PrevSnapshotDate:       "2026-11-11",
				NewOpenCandidatesAdded: []model.DeltaAddedIssue{},
			},
		},
		Enums: model.DatasetEnums{
			Categories: model.ValidCategories,
			Severities: model.ValidSeverities,
			Subsystems: model.ValidSubsystems,
		},
		Records: []model.IssueRecord{
			{
				Number:           78001,
				Title:            "planner: borderline issue with initial open PR",
				Author:           "alice",
				Created:          "2026-11-01",
				CreatedAt:        "2026-11-01T00:00:00Z",
				UpdatedAt:        "2026-11-12T09:50:00Z",
				Labels:           []string{"sig/planner", "type/bug"},
				IsCanonical:      true,
				Cat:              "MEDIUM",
				Severity:         "MAJOR",
				GHSeverity:       "major",
				Subsystem:        "logical_rewrite",
				IsBorderlineKept: true,
				ForceKeep:        true,
				PRs:              []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 78901, State: model.PRStateUnknown}},
			},
			{
				Number:      78200,
				Title:       "supplemental issue excluded then re-excluded concurrently",
				Author:      "bob",
				Created:     "2026-11-02",
				CreatedAt:   "2026-11-02T00:00:00Z",
				UpdatedAt:   "2026-11-12T09:40:00Z",
				Labels:      []string{"component/mview", "type/bug"},
				IsCanonical: false,
				Cat:         "FILTER_NON_OPT_OR_FIXED",
				Severity:    "N/A",
				GHSeverity:  "minor",
				Subsystem:   "logical_rewrite",
			},
		},
	}
	if err := st.IngestSyncedDataset(seed, []model.PendingCandidate{
		{
			Number:    78101,
			Title:     "pending issue that closes on GitHub while human accepts it",
			Author:    "carol",
			CreatedAt: "2026-11-12T10:01:00Z",
			UpdatedAt: "2026-11-12T10:02:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
		{
			Number:    78102,
			Title:     "pending issue that gains PR on GitHub while human accepts it",
			Author:    "dave",
			CreatedAt: "2026-11-12T10:03:00Z",
			UpdatedAt: "2026-11-12T10:04:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
		{
			Number:    78103,
			Title:     "pending issue accepted locally during sync",
			Author:    "erin",
			CreatedAt: "2026-11-12T10:05:00Z",
			UpdatedAt: "2026-11-12T10:06:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
		{
			Number:    78104,
			Title:     "pending issue closed on GitHub before Machine B submits batch",
			Author:    "frank",
			CreatedAt: "2026-11-12T10:07:00Z",
			UpdatedAt: "2026-11-12T10:08:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
	}); err != nil {
		t.Fatalf("IngestSyncedDataset: %v", err)
	}

	specialBackup := filepath.Join(specialDir, "backup#copy?.db")
	if err := st.BackupTo(specialBackup); err != nil {
		t.Fatalf("BackupTo(specialBackup) failed: %v", err)
	}

	// Initial exclusion of #78200 BEFORE sync PlanStartedUTC
	if err := st.ExcludeExistingIssue(78200, "initial exclude", false); err != nil {
		t.Fatalf("ExcludeExistingIssue(78200): %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	planStarted := model.NowRFC3339Nano()
	time.Sleep(2 * time.Millisecond)

	// Simulate concurrent human actions AFTER PlanStartedUTC but BEFORE ApplyIssueSync commits:
	// - Accept #78101, #78102, and #78103
	for _, num := range []int{78101, 78102, 78103} {
		if err := st.ResolvePendingCandidate(num, model.PendingResolveRequest{
			Action:    "accept",
			Cat:       "MEDIUM",
			Severity:  "MAJOR",
			Subsystem: "statistics",
			Note:      &model.LocalizedNote{ZhCN: "并发入库", En: "Concurrent accept"},
		}); err != nil {
			t.Fatalf("ResolvePendingCandidate(%d): %v", num, err)
		}
	}
	// - Re-exclude #78200 AFTER PlanStartedUTC
	if err := st.ExcludeExistingIssue(78200, "fresher human exclude", false); err != nil {
		t.Fatalf("re-ExcludeExistingIssue(78200): %v", err)
	}

	// 2. ApplyIssueSync commits a SyncPlan computed from pre-accept state:
	//    - #78101 closed on GitHub (ClosedPendingRemovals) -> must archive concurrently accepted #78101!
	//    - #78102 & #78103 updated in UpsertPending (#78102 gained timeline PR #78999) -> must stay in issues (NOT resurrected in pending) AND #78102 gets PR #78999!
	//    - #78104 closed on GitHub -> removed from pending and recorded in removed_pending_nums!
	//    - #78200 in ReadmittedNums -> blocked by fresher excluded_at > PlanStartedUTC!
	plan := issuesync.SyncPlan{
		ModeUsed:       "incremental",
		PlanStartedUTC: planStarted,
		SnapshotUTC:    "2026-11-12T12:00:00Z",
		ClosedPendingRemovals: []model.DeltaClosedIssue{
			{Number: 78101, ClosedAt: "2026-11-12T11:55:00Z", Reason: "closed on GitHub"},
			{Number: 78104, ClosedAt: "2026-11-12T11:55:00Z", Reason: "closed on GitHub"},
		},
		DeletePendingNums: []int{78101, 78104},
		ReadmittedNums:    []int{78200},
		UpsertPending: []model.PendingCandidate{
			{
				Number:    78102,
				Title:     "pending issue that gains PR on GitHub while human accepts it (updated title)",
				Author:    "dave",
				CreatedAt: "2026-11-12T10:03:00Z",
				UpdatedAt: "2026-11-12T11:55:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
				PRs:       []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 78999, Label: "#78999", URL: "https://github.com/pingcap/tidb/pull/78999", State: "OPEN", UpdatedAt: "2026-11-12T11:55:00Z"}},
			},
			{
				Number:    78103,
				Title:     "pending issue accepted locally during sync",
				Author:    "erin",
				CreatedAt: "2026-11-12T10:05:00Z",
				UpdatedAt: "2026-11-12T10:06:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
			},
			{
				Number:    78200,
				Title:     "stale readmission candidate",
				Author:    "bob",
				CreatedAt: "2026-11-02T00:00:00Z",
				UpdatedAt: "2026-11-12T11:50:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
			},
		},
	}
	if _, err := st.ApplyIssueSync(plan); err != nil {
		t.Fatalf("ApplyIssueSync failed: %v", err)
	}

	pendAfter, err := st.ListPendingCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(pendAfter) != 0 {
		t.Fatalf("expected 0 pending candidates (no resurrection of #78102/#78103 and no stale readmission of #78200), got %+v", pendAfter)
	}

	dsAfterSync, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	recMap := make(map[int]model.IssueRecord)
	for _, r := range dsAfterSync.Records {
		recMap[r.Number] = r
	}
	if _, stillActive := recMap[78101]; stillActive {
		t.Fatalf("expected concurrently accepted #78101 to be archived by ClosedPendingRemovals")
	}
	if r78102, ok := recMap[78102]; !ok || len(r78102.PRs) != 1 || r78102.PRs[0].Num != 78999 {
		t.Fatalf("expected concurrently accepted #78102 to receive live PR #78999 from UpsertPending, got %+v", recMap[78102])
	}
	foundDelta78103 := false
	if dsAfterSync.SyncMeta.DeltaSincePrevSnapshot != nil {
		for _, d := range dsAfterSync.SyncMeta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded {
			if d.Number == 78103 {
				foundDelta78103 = true
			}
		}
	}
	if !foundDelta78103 {
		t.Fatalf("expected concurrent local accept #78103 preserved in SyncMeta.DeltaSincePrevSnapshot, got %+v", dsAfterSync.SyncMeta.DeltaSincePrevSnapshot)
	}

	// 3. Batch validation with BaseExportedAt != "" and SkipConflicts=true:
	//    - Closed/removed pending #78104 IS skipped as a conflict (via removed_pending_nums)
	//    - Genuine typo #88888888 in new_issues or exclude_issues FAILS with ErrIssueNotFound!
	_, cfl78104, err := st.PlanBatchDecisionsWithOptions(model.BatchTriageDecisions{
		BaseExportedAt: planStarted,
		NewIssues: map[string]model.TriagePatchRequest{
			"78104": {Cat: "EASY", Severity: "MINOR", Subsystem: "statistics"},
		},
	}, BatchApplyOptions{SkipConflicts: true})
	if err != nil || len(cfl78104) != 1 {
		t.Fatalf("expected closed pending #78104 to be skipped via removed_pending_nums, got cfl=%+v err=%v", cfl78104, err)
	}
	if _, _, err := st.PlanBatchDecisionsWithOptions(model.BatchTriageDecisions{
		BaseExportedAt: planStarted,
		NewIssues: map[string]model.TriagePatchRequest{
			"88888888": {Cat: "EASY", Severity: "MINOR", Subsystem: "statistics"},
		},
	}, BatchApplyOptions{SkipConflicts: true}); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("expected typo #88888888 in new_issues to fail with ErrIssueNotFound even when BaseExportedAt is set, got %v", err)
	}
	if _, _, err := st.PlanBatchDecisionsWithOptions(model.BatchTriageDecisions{
		BaseExportedAt: planStarted,
		ExcludeIssues:  map[string]string{"88888888": "typo"},
	}, BatchApplyOptions{SkipConflicts: true}); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("expected typo #88888888 in exclude_issues to fail with ErrIssueNotFound even when BaseExportedAt is set, got %v", err)
	}

	// 4. JSON override omitting booleans preserves IsBorderlineKept & ForceKeep,
	//    ApplyIssueOverride enforces IfUnmodifiedSince (ErrOverrideConflict),
	//    and UpsertPRStatuses suppresses pre-ack merge on unverified UNKNOWN PR #78901!
	r78001 := recMap[78001]
	var tplPatch model.TriagePatchRequest
	rawTplJSON, _ := json.Marshal(map[string]any{
		"cat":                 "HARD",
		"severity":            "CRITICAL",
		"subsystem":           "logical_rewrite",
		"if_unmodified_since": r78001.TriagedAt,
		"note": map[string]string{
			"zh-CN": "模板覆盖保留边界与强留标记",
			"en":    "Template override preserves borderline and force_keep flags",
		},
	})
	if err := json.Unmarshal(rawTplJSON, &tplPatch); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyIssueOverride(78001, tplPatch); err != nil {
		t.Fatalf("ApplyIssueOverride(78001) failed: %v", err)
	}
	// Stale IfUnmodifiedSince must fail with ErrOverrideConflict
	var stalePatch model.TriagePatchRequest
	_ = json.Unmarshal([]byte(`{"cat":"EASY","severity":"MINOR","subsystem":"statistics","if_unmodified_since":"2020-01-01T00:00:00.000000000Z"}`), &stalePatch)
	if err := st.ApplyIssueOverride(78001, stalePatch); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("expected ErrOverrideConflict on stale IfUnmodifiedSince, got %v", err)
	}

	// Simulate post-override PR sync discovering #78901 was actually MERGED BEFORE baseline (MergedAt=09:30:00Z <= 10:00:00Z)!
	if err := st.UpsertPRStatuses([]model.PRStatus{{
		Repo:      "pingcap/tidb",
		Num:       78901,
		Title:     "planner: already merged fix",
		State:     "MERGED",
		MergedAt:  "2026-11-12T09:30:00Z",
		UpdatedAt: "2026-11-12T12:00:00Z",
	}}, time.Now().UTC()); err != nil {
		t.Fatalf("UpsertPRStatuses(78901=MERGED): %v", err)
	}
	// UpsertPRStatuses atomically reconciles unverified #78901 from open->merged in both baseline and auto_baseline!
	dsReconciled, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsReconciled.Records {
		if r.Number == 78001 {
			if !r.IsBorderlineKept || !r.ForceKeep {
				t.Fatalf("expected NewTemplateOverridePatch to preserve IsBorderlineKept=true & ForceKeep=true, got %+v", r)
			}
			if r.NeedsRetriage {
				t.Fatalf("expected atomic UpsertPRStatuses reconciliation to clear spurious fix_pr_merged_still_open on #78001, got reasons=%v", r.RetriageReasons)
			}
		}
	}
	// Even after ResetIssueOverride restores auto_baseline_json, #78901 stays reconciled (no spurious retriage)!
	if err := st.ResetIssueOverride(78001); err != nil {
		t.Fatalf("ResetIssueOverride(78001): %v", err)
	}
	dsAfterReset, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsAfterReset.Records {
		if r.Number == 78001 && r.NeedsRetriage {
			t.Fatalf("expected auto_baseline_json on #78001 to also have reconciled PR #78901 after ResetIssueOverride, got reasons=%v", r.RetriageReasons)
		}
	}
}

func TestConvergenceInvariantsMonotonicFreshnessRevisionAndChunking(t *testing.T) {
	tmp := t.TempDir()

	// 1. file: URI Open resolves physical DB path via PRAGMA database_list and blocks BackupTo self-overwrite
	uriDBPath := filepath.Join(tmp, "uri_target.db")
	stURI, err := Open("file:" + filepath.ToSlash(uriDBPath))
	if err != nil {
		t.Fatalf("Open(file: URI) failed: %v", err)
	}
	defer stURI.Close()
	if strings.TrimSpace(stURI.DBPath()) == "" {
		t.Fatalf("expected non-empty DBPath() for file: URI SQLite database")
	}
	if err := stURI.BackupTo(uriDBPath); err == nil || !strings.Contains(err.Error(), "cannot be the active SQLite database") {
		t.Fatalf("expected BackupTo self-overwrite rejection on file: URI DB, got %v", err)
	}

	// 2. Monotonic PR freshness: out-of-order older or same-second OPEN snapshots never clobber fresher MERGED
	dbPath := filepath.Join(tmp, "monotonic.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records: []model.IssueRecord{{
			Number:    81001,
			Title:     "planner: issue with fix PR",
			Author:    "alice",
			Created:   "2026-11-12",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:00:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			Cat:       "MEDIUM",
			Severity:  "MAJOR",
			Subsystem: "logical_rewrite",
			Note:      &model.LocalizedNote{ZhCN: "单调新鲜度测试", En: "Monotonic freshness test"},
			PRs: []model.LinkedPRRef{{
				Repo:      "pingcap/tidb",
				Num:       81901,
				Label:     "#81901",
				URL:       "https://github.com/pingcap/tidb/pull/81901",
				State:     "OPEN",
				Title:     "planner: fix 81001",
				UpdatedAt: "2026-11-12T10:00:00Z",
			}},
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Targeted sync commits MERGED at t=10:10:00Z
	if err := st.UpsertPRStatuses([]model.PRStatus{{
		Repo:         "pingcap/tidb",
		Num:          81901,
		Title:        "planner: fix 81001",
		State:        "MERGED",
		UpdatedAt:    "2026-11-12T10:10:00Z",
		TrackSources: []string{"issue_linked"},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcknowledgeIssueRetriage(81001, "maintainer"); err != nil {
		t.Fatal(err)
	}

	// Concurrent slower full PR sync submits stale OPEN (t=10:05:00Z) and same-second OPEN (t=10:10:00Z)
	for _, staleUpd := range []string{"2026-11-12T10:05:00Z", "2026-11-12T10:10:00Z"} {
		if err := st.UpsertPRStatuses([]model.PRStatus{{
			Repo:         "pingcap/tidb",
			Num:          81901,
			Title:        "planner: stale open snapshot",
			State:        "OPEN",
			UpdatedAt:    staleUpd,
			TrackSources: []string{"sig_planner"},
		}}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	// Stale ApplyIssueSync also tries to regress #81901 via UpdatedIssues & DirectPRUpdates
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{
		SnapshotUTC: "2026-11-12T10:11:00Z",
		UpdatedIssues: []issuesync.LiveIssueUpdate{{
			Number:    81001,
			Title:     "planner: issue with fix PR",
			Author:    "alice",
			Created:   "2026-11-12",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:09:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			TimelinePRs: []model.LinkedPRRef{{
				Repo:      "pingcap/tidb",
				Num:       81901,
				Label:     "#81901",
				URL:       "https://github.com/pingcap/tidb/pull/81901",
				State:     "OPEN",
				UpdatedAt: "2026-11-12T10:05:00Z",
			}},
		}},
		DirectPRUpdates: map[string]model.LinkedPRRef{
			"pingcap/tidb#81901": {
				Repo:      "pingcap/tidb",
				Num:       81901,
				State:     "OPEN",
				UpdatedAt: "2026-11-12T10:05:00Z",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var cachedState, cachedStatusJSON string
	if err := st.db.QueryRow(`SELECT state, status_json FROM pull_requests WHERE repo = 'pingcap/tidb' AND number = 81901`).Scan(&cachedState, &cachedStatusJSON); err != nil {
		t.Fatal(err)
	}
	if cachedState != "MERGED" {
		t.Fatalf("expected PR #81901 to remain MERGED against stale snapshots, got %s", cachedState)
	}
	var cachedSt model.PRStatus
	_ = json.Unmarshal([]byte(cachedStatusJSON), &cachedSt)
	if len(cachedSt.TrackSources) != 2 {
		t.Fatalf("expected TrackSources merged even when stale snapshot skipped, got %v", cachedSt.TrackSources)
	}

	dsMono, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsMono.Records {
		if r.Number == 81001 {
			if len(r.PRs) != 1 || r.PRs[0].State != "MERGED" {
				t.Fatalf("expected issue #81001 PR link to remain MERGED, got %+v", r.PRs)
			}
			if r.NeedsRetriage {
				t.Fatalf("expected no spurious retriage regression on #81001, got %v", r.RetriageReasons)
			}
		}
	}

	// 3. pending_candidates.revision CAS protects same-timestamp PR discovery from stale UpdatePendingCandidates
	if _, _, err := st.ApplyIssueSyncWithOutcome(issuesync.SyncPlan{
		SnapshotUTC: "2026-11-12T11:00:00Z",
		UpsertPending: []model.PendingCandidate{{
			Number:    82001,
			Title:     "planner: pending candidate",
			Author:    "bob",
			CreatedAt: "2026-11-12T11:00:00Z",
			UpdatedAt: "2026-11-12T11:00:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	pendBefore, err := st.ListPendingCandidates()
	if err != nil || len(pendBefore) != 1 || pendBefore[0].Revision != 1 {
		t.Fatalf("expected 1 pending candidate at revision 1, got %+v err=%v", pendBefore, err)
	}
	staleEnrich := pendBefore[0]

	// Stream B discovers PR #82901 without changing issue updated_at -> revision bumps to 2
	if _, _, err := st.ApplyIssueSyncWithOutcome(issuesync.SyncPlan{
		SnapshotUTC: "2026-11-12T11:05:00Z",
		UpsertPending: []model.PendingCandidate{{
			Number:    82001,
			Title:     "planner: pending candidate",
			Author:    "bob",
			CreatedAt: "2026-11-12T11:00:00Z",
			UpdatedAt: "2026-11-12T11:00:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			PRs:       []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 82901, State: "OPEN"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePendingCandidates([]model.PendingCandidate{staleEnrich}); err != nil {
		t.Fatal(err)
	}
	pendAfter, err := st.ListPendingCandidates()
	if err != nil || len(pendAfter) != 1 || len(pendAfter[0].PRs) != 1 || pendAfter[0].PRs[0].Num != 82901 {
		t.Fatalf("expected revision CAS to protect newly discovered PR #82901 from stale UpdatePendingCandidates, got %+v err=%v", pendAfter, err)
	}

	// 4. ApplyIssueSyncWithOutcome + FormatSyncSummary reports concurrent ClosedPendingRemovals & PendingLiveUpdates graduations
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"82001": {
				Cat:       "MEDIUM",
				Severity:  "MAJOR",
				Subsystem: "logical_rewrite",
				Note:      &model.LocalizedNote{ZhCN: "并发接受后关闭归档测试", En: "Concurrent accept then closed removal test"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	planConc := issuesync.SyncPlan{
		ModeUsed:       "incremental",
		SnapshotUTC:    "2026-11-12T11:10:00Z",
		PlanStartedUTC: "2026-11-12T11:08:00Z",
		ClosedPendingRemovals: []model.DeltaClosedIssue{{
			Number: 82001,
			Reason: "Closed on GitHub",
		}},
	}
	dsConc, appliedConc, err := st.ApplyIssueSyncWithOutcome(planConc)
	if err != nil {
		t.Fatal(err)
	}
	planConc.Applied = &appliedConc
	sumTxt := issuesync.FormatSyncSummary(planConc, dsMono, dsConc)
	if !strings.Contains(sumTxt, "Closed Removed: [82001]") {
		t.Fatalf("expected FormatSyncSummary to reflect post-commit ClosedPendingRemovals [82001], got:\n%s", sumTxt)
	}

	// 5. Chunked ListRetriageCandidates handles > sqlInChunkSize (400) issues without dropping bodies or comments
	chunkDB := filepath.Join(tmp, "chunked.db")
	stChunk, err := Open(chunkDB)
	if err != nil {
		t.Fatal(err)
	}
	defer stChunk.Close()
	bulkRecs := make([]model.IssueRecord, 0, 455)
	for i := 1; i <= 455; i++ {
		num := 90000 + i
		bulkRecs = append(bulkRecs, model.IssueRecord{
			Number:    num,
			Title:     fmt.Sprintf("planner: bulk issue %d", num),
			Author:    "alice",
			Created:   "2026-11-12",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:00:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			Cat:       "EASY",
			Severity:  "MINOR",
			Subsystem: "statistics",
			Note:      &model.LocalizedNote{ZhCN: "批量分块查询测试", En: "Chunked query test"},
		})
	}
	if err := stChunk.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records:  bulkRecs,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := stChunk.ApplyIssueSync(issuesync.SyncPlan{
		SnapshotUTC: "2026-11-12T10:05:00Z",
		UpdatedIssues: []issuesync.LiveIssueUpdate{
			{
				Number:         90001,
				Title:          "planner: bulk issue 90001",
				Author:         "alice",
				Created:        "2026-11-12",
				CreatedAt:      "2026-11-12T10:00:00Z",
				UpdatedAt:      "2026-11-12T10:05:00Z",
				Labels:         []string{"sig/planner", "type/bug"},
				HasBody:        true,
				Body:           "body-first-chunk",
				HasComments:    true,
				RecentComments: []model.PendingComment{{Author: "dev1", CreatedAt: "2026-11-12T10:04:00Z", Body: "cmt-first"}},
			},
			{
				Number:         90455,
				Title:          "planner: bulk issue 90455",
				Author:         "alice",
				Created:        "2026-11-12",
				CreatedAt:      "2026-11-12T10:00:00Z",
				UpdatedAt:      "2026-11-12T10:05:00Z",
				Labels:         []string{"sig/planner", "type/bug"},
				HasBody:        true,
				Body:           "body-second-chunk",
				HasComments:    true,
				RecentComments: []model.PendingComment{{Author: "dev2", CreatedAt: "2026-11-12T10:04:00Z", Body: "cmt-second"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cands, err := stChunk.ListRetriageCandidates(false, nil)
	if err != nil || len(cands) != 455 {
		t.Fatalf("expected 455 retriage candidates across chunks, got len=%d err=%v", len(cands), err)
	}
	foundFirst, foundSecond := false, false
	for _, c := range cands {
		if c.Number == 90001 && c.Body == "body-first-chunk" && len(c.RecentComments) == 1 {
			foundFirst = true
		}
		if c.Number == 90455 && c.Body == "body-second-chunk" && len(c.RecentComments) == 1 {
			foundSecond = true
		}
	}
	if !foundFirst || !foundSecond {
		t.Fatalf("expected chunked ListRetriageCandidates to populate bodies & comments across both chunks (first=%v, second=%v)", foundFirst, foundSecond)
	}
}

func TestExplicitFactModelUnknownManualPRSyncWatermarkAndReopen(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "facts.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Seed 3 active issues + 1 issue with an OPEN PR that will close and reopen + 1 pending candidate
	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records: []model.IssueRecord{
			{
				Number:      91001,
				Title:       "planner: manual PR turns out OPEN",
				Author:      "alice",
				Created:     "2026-11-12",
				CreatedAt:   "2026-11-12T10:00:00Z",
				UpdatedAt:   "2026-11-12T10:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				Subsystem:   "logical_rewrite",
				Note:        &model.LocalizedNote{ZhCN: "测试未知手动PR验证为OPEN", En: "Test unknown manual PR verified as OPEN"},
			},
			{
				Number:      91002,
				Title:       "planner: manual PR merges AFTER triage ack",
				Author:      "alice",
				Created:     "2026-11-12",
				CreatedAt:   "2026-11-12T10:00:00Z",
				UpdatedAt:   "2026-11-12T10:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				Subsystem:   "logical_rewrite",
				Note:        &model.LocalizedNote{ZhCN: "测试未知手动PR在分诊后合并触发重审", En: "Test unknown manual PR merged after ack triggers retriage"},
			},
			{
				Number:      91003,
				Title:       "planner: manual PR had ALREADY merged BEFORE triage ack",
				Author:      "alice",
				Created:     "2026-11-12",
				CreatedAt:   "2026-11-12T10:00:00Z",
				UpdatedAt:   "2026-11-12T10:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				Subsystem:   "logical_rewrite",
				Note:        &model.LocalizedNote{ZhCN: "测试未知手动PR早已合并则不自触发重审", En: "Test unknown manual PR merged before ack does not self-flag"},
			},
			{
				Number:      91004,
				Title:       "planner: PR closes and later reopens",
				Author:      "alice",
				Created:     "2026-11-12",
				CreatedAt:   "2026-11-12T10:00:00Z",
				UpdatedAt:   "2026-11-12T10:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				Subsystem:   "logical_rewrite",
				Note:        &model.LocalizedNote{ZhCN: "测试关闭后重开的PR再次触发new_fix_pr", En: "Test closed then reopened PR triggers new_fix_pr"},
				PRs:         []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 95004, State: "OPEN", UpdatedAt: "2026-11-12T10:00:00Z"}},
			},
		},
	}, []model.PendingCandidate{
		{
			Number:    92001,
			Title:     "planner: pending candidate with sync_watermark CAS",
			Author:    "bob",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:05:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Attach uncached manual_pr_links offline -> stored as PRStateUnknown ("UNKNOWN") with non-empty ack_at
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		ManualPRLinks: map[string][]string{
			"91001": {"95001"},
			"91002": {"95002"},
			"91003": {"95003"},
		},
	}); err != nil {
		t.Fatalf("ApplyBatchTriageDecisions(ManualPRLinks): %v", err)
	}

	dsUnknown, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	byNum := make(map[int]model.IssueRecord)
	for _, r := range dsUnknown.Records {
		byNum[r.Number] = r
	}
	if len(byNum[91001].PRs) != 1 || byNum[91001].PRs[0].State != model.PRStateUnknown || byNum[91001].PRs[0].AckAt == "" {
		t.Fatalf("expected uncached manual PR #95001 stored as UNKNOWN with AckAt, got %+v", byNum[91001].PRs)
	}
	ackAt := byNum[91002].PRs[0].AckAt
	ackParsed, err := time.Parse(time.RFC3339Nano, ackAt)
	if err != nil {
		t.Fatalf("parse AckAt %q: %v", ackAt, err)
	}
	beforeAck := ackParsed.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	afterAck := ackParsed.Add(10 * time.Minute).UTC().Format(time.RFC3339)

	// 2. Verify PR states via UpsertPRStatuses:
	//    - #95001 = OPEN -> NO new_fix_pr (AckAt != "")
	//    - #95002 = MERGED at afterAck -> FIRES fix_pr_merged_still_open (MergedAt > baseline/AckAt)
	//    - #95003 = MERGED at beforeAck -> NO fix_pr_merged_still_open (MergedAt <= baseline/AckAt)
	//    - #95004 = CLOSED -> clears ack_at on #95004
	if err := st.UpsertPRStatuses([]model.PRStatus{
		{Repo: "pingcap/tidb", Num: 95001, State: "OPEN", UpdatedAt: afterAck},
		{Repo: "pingcap/tidb", Num: 95002, State: "MERGED", UpdatedAt: afterAck, MergedAt: afterAck},
		{Repo: "pingcap/tidb", Num: 95003, State: "MERGED", UpdatedAt: afterAck, MergedAt: beforeAck},
		{Repo: "pingcap/tidb", Num: 95004, State: "CLOSED", UpdatedAt: afterAck},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Acknowledge any retriage or check #91001, #91002, #91003, and then reopen #95004!
	dsVerified, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	byNum = make(map[int]model.IssueRecord)
	for _, r := range dsVerified.Records {
		byNum[r.Number] = r
	}
	if byNum[91001].NeedsRetriage {
		t.Fatalf("expected #91001 (UNKNOWN -> OPEN) NOT to trigger new_fix_pr, got reasons=%v", byNum[91001].RetriageReasons)
	}
	if !byNum[91002].NeedsRetriage || len(byNum[91002].RetriageReasons) != 1 || byNum[91002].RetriageReasons[0] != "fix_pr_merged_still_open" {
		t.Fatalf("expected #91002 (UNKNOWN -> post-ack MERGED) to trigger fix_pr_merged_still_open, got NeedsRetriage=%v reasons=%v", byNum[91002].NeedsRetriage, byNum[91002].RetriageReasons)
	}
	if byNum[91003].NeedsRetriage {
		t.Fatalf("expected #91003 (UNKNOWN -> pre-ack MERGED) NOT to self-flag retriage, got reasons=%v", byNum[91003].RetriageReasons)
	}

	// Acknowledge #91004 at CLOSED state (so baseOpen is empty), then reopen #95004 -> MUST fire new_fix_pr!
	if _, err := st.AcknowledgeIssueRetriage(91004, "maintainer"); err != nil {
		t.Fatal(err)
	}
	reopenTS := ackParsed.Add(20 * time.Minute).UTC().Format(time.RFC3339)
	if err := st.UpsertPRStatuses([]model.PRStatus{
		{Repo: "pingcap/tidb", Num: 95004, State: "OPEN", UpdatedAt: reopenTS},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	dsReopened, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsReopened.Records {
		if r.Number == 91004 && !r.NeedsRetriage {
			t.Fatalf("expected reopened PR #95004 (OPEN -> CLOSED -> OPEN) to fire new_fix_pr on #91004")
		}
	}

	// 3. Pending SyncWatermark stays stable across enrichment cache writes, advances on core sync change,
	//    and enforces IfUnmodifiedSince on ResolvePendingCandidate!
	pList1, err := st.ListPendingCandidates()
	if err != nil || len(pList1) != 1 || pList1[0].SyncWatermark == "" {
		t.Fatalf("expected pending #92001 with non-empty SyncWatermark, got %+v err=%v", pList1, err)
	}
	wm1 := pList1[0].SyncWatermark

	// Enrichment cache update does NOT bump SyncWatermark
	enriched := pList1[0]
	enriched.SuggestedTriage = &model.SuggestedTriage{Cat: "EASY", Severity: "MINOR", Subsystem: "statistics"}
	if err := st.UpdatePendingCandidates([]model.PendingCandidate{enriched}); err != nil {
		t.Fatal(err)
	}
	pListEnriched, err := st.ListPendingCandidates()
	if err != nil || pListEnriched[0].SyncWatermark != wm1 {
		t.Fatalf("expected UpdatePendingCandidates NOT to change SyncWatermark (%q vs %q)", pListEnriched[0].SyncWatermark, wm1)
	}

	// Core sync update (new PR discovered on #92001) DOES advance SyncWatermark monotonically
	if _, err := st.ApplyIssueSync(issuesync.SyncPlan{
		SnapshotUTC: "2026-11-12T10:20:00Z",
		UpsertPending: []model.PendingCandidate{{
			Number:    92001,
			Title:     "planner: pending candidate with sync_watermark CAS",
			Author:    "bob",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:05:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			PRs:       []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 95999, State: "OPEN"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	pList2, err := st.ListPendingCandidates()
	if err != nil || pList2[0].SyncWatermark <= wm1 {
		t.Fatalf("expected SyncWatermark to advance after PR discovery (%q <= %q)", pList2[0].SyncWatermark, wm1)
	}
	wm2 := pList2[0].SyncWatermark

	// Stale wm1 is rejected with ErrOverrideConflict; fresh wm2 succeeds!
	if err := st.ResolvePendingCandidate(92001, model.PendingResolveRequest{
		Action:            "accept",
		Cat:               "EASY",
		Severity:          "MINOR",
		Subsystem:         "statistics",
		IfUnmodifiedSince: wm1,
		Note:              &model.LocalizedNote{ZhCN: "过期水位线应冲突", En: "Stale watermark must fail"},
	}); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("expected ErrOverrideConflict on stale SyncWatermark, got %v", err)
	}
	if err := st.ResolvePendingCandidate(92001, model.PendingResolveRequest{
		Action:            "accept",
		Cat:               "EASY",
		Severity:          "MINOR",
		Subsystem:         "statistics",
		IfUnmodifiedSince: wm2,
		Note:              &model.LocalizedNote{ZhCN: "最新水位线正常入库", En: "Fresh watermark resolves cleanly"},
	}); err != nil {
		t.Fatalf("expected fresh SyncWatermark to succeed, got %v", err)
	}
}

func TestRound3StoreConvergenceInvariants(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "round3.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Seed active issue #97001 with verified OPEN PR #97901 and #97002 with UNKNOWN manual link #97902
	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records: []model.IssueRecord{
			{
				Number:      97001,
				Title:       "planner: verified OPEN PR merges before calibration",
				Author:      "alice",
				Created:     "2026-11-12",
				CreatedAt:   "2026-11-12T09:00:00Z",
				UpdatedAt:   "2026-11-12T10:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				Subsystem:   "logical_rewrite",
				PRs: []model.LinkedPRRef{{
					Repo:      "pingcap/tidb",
					Num:       97901,
					Label:     "#97901",
					URL:       "https://github.com/pingcap/tidb/pull/97901",
					State:     "OPEN",
					UpdatedAt: "2026-11-12T10:00:00Z",
				}},
			},
			{
				Number:      97002,
				Title:       "planner: issue with pre-cached PR status and lagging link",
				Author:      "bob",
				Created:     "2026-11-12",
				CreatedAt:   "2026-11-12T09:00:00Z",
				UpdatedAt:   "2026-11-12T10:00:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "EASY",
				Severity:    "MODERATE",
				Subsystem:   "statistics",
			},
		},
	}, []model.PendingCandidate{{
		Number:    97101,
		Title:     "planner: pending candidate with open PR",
		Author:    "carol",
		CreatedAt: "2026-11-12T10:00:00Z",
		UpdatedAt: "2026-11-12T10:00:00Z",
		Labels:    []string{"sig/planner", "type/bug"},
		PRs:       []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 97910, State: "OPEN", UpdatedAt: "2026-11-12T10:00:00Z"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// 1. Finding #1: Human calibrates #97001 while local cache still says PR #97901 is OPEN,
	//    even though #97901 actually merged on GitHub 5 minutes BEFORE human calibration (MergedAt < nowISO).
	//    Next PR sync delivering MERGED MUST still trigger fix_pr_merged_still_open!
	mergedBeforeCalib := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	if err := st.ApplyIssueOverride(97001, model.TriagePatchRequest{
		Cat:       "HARD",
		Severity:  "CRITICAL",
		Subsystem: "logical_rewrite",
	}); err != nil {
		t.Fatal(err)
	}
	dsBeforeSync, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	var preTriggerWatermark string
	for _, r := range dsBeforeSync.Records {
		if r.Number == 97001 {
			preTriggerWatermark = r.TriagedAt
		}
	}
	if preTriggerWatermark == "" {
		t.Fatalf("expected non-empty TriagedAt after override")
	}

	time.Sleep(2 * time.Millisecond)
	if err := st.UpsertPRStatuses([]model.PRStatus{{
		Repo:         "pingcap/tidb",
		Num:          97901,
		Title:        "planner: fix 97001",
		State:        "MERGED",
		UpdatedAt:    mergedBeforeCalib,
		MergedAt:     mergedBeforeCalib,
		TrackSources: []string{"issue_linked"},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	dsAfterMerge, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	var postTriggerWatermark string
	foundMergeTrigger := false
	for _, r := range dsAfterMerge.Records {
		if r.Number == 97001 {
			postTriggerWatermark = r.TriagedAt
			for _, reason := range r.RetriageReasons {
				if reason == "fix_pr_merged_still_open" {
					foundMergeTrigger = true
				}
			}
		}
	}
	if !foundMergeTrigger {
		t.Fatalf("expected verified OPEN -> MERGED to fire fix_pr_merged_still_open even when MergedAt (%s) < calibration timestamp", mergedBeforeCalib)
	}

	// 2. Finding #8a & #8c: Background retriage trigger bumped retriage_updated_at,
	//    so stale preTriggerWatermark MUST be rejected on AcknowledgeIssueRetriageWithGuard & ResetIssueOverrideWithGuard!
	if postTriggerWatermark <= preTriggerWatermark {
		t.Fatalf("expected retriage_updated_at to advance TriagedAt (%q <= %q)", postTriggerWatermark, preTriggerWatermark)
	}
	if _, err := st.AcknowledgeIssueRetriageWithGuard(97001, "maint", preTriggerWatermark); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("expected stale AcknowledgeIssueRetriageWithGuard to fail with ErrOverrideConflict, got %v", err)
	}
	if err := st.ResetIssueOverrideWithGuard(97001, preTriggerWatermark); !errors.Is(err, ErrOverrideConflict) {
		t.Fatalf("expected stale ResetIssueOverrideWithGuard to fail with ErrOverrideConflict, got %v", err)
	}

	// 3. Finding #9: Orthogonal `issue_notes`:
	//    - Backfill bilingual notes via Notes
	//    - Single-locale update preserves the other locale
	//    - Notes update does NOT cause false conflict on AcknowledgeIssueRetriageWithGuard(postTriggerWatermark)
	//    - ResetIssueOverride deletes classification override (HARD -> MEDIUM) while PRESERVING bilingual notes!
	time.Sleep(2 * time.Millisecond)
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		Notes: map[string]model.NotePatchRequest{
			"97001": {
				ZhCN: "初始中文根因：谓词下推遗漏空值拒绝检查导致结果集错误",
				En:   "Initial English note: predicate pushdown misses null-rejecting check causing wrong result",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Single-locale zh-CN update preserves English note
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		Notes: map[string]model.NotePatchRequest{
			"97001": {
				ZhCN: "更新后中文根因：`pushDownPredicate` 遗漏空值拒绝检查导致外连接结果错误",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Acknowledge retriage using postTriggerWatermark (taken BEFORE the note updates!) -> MUST SUCCEED (zero false conflict)!
	if _, err := st.AcknowledgeIssueRetriageWithGuard(97001, "maint", postTriggerWatermark); err != nil {
		t.Fatalf("expected note-only updates NOT to conflict with AcknowledgeIssueRetriageWithGuard, got %v", err)
	}
	// Reset classification override -> reverts Cat from HARD back to auto MEDIUM, while keeping bilingual notes!
	if err := st.ResetIssueOverride(97001); err != nil {
		t.Fatal(err)
	}
	dsAfterReset, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsAfterReset.Records {
		if r.Number == 97001 {
			if r.Cat != "MEDIUM" || r.ManualOverride {
				t.Fatalf("expected ResetIssueOverride to revert Cat to MEDIUM and ManualOverride=false, got Cat=%s ManualOverride=%v", r.Cat, r.ManualOverride)
			}
			if r.Note == nil || !strings.Contains(r.Note.ZhCN, "pushDownPredicate") || !strings.Contains(r.Note.En, "Initial English note") {
				t.Fatalf("expected ResetIssueOverride and single-locale note patch to preserve both locales, got %+v", r.Note)
			}
		}
	}

	// 4. Gap #1: UpsertPRStatuses preserves TrackSources union ("manual_cli") on subset upsert,
	//    and hydrates UNKNOWN issue_pr_links even when pull_requests already has the fresh snapshot!
	if err := st.UpsertPRStatuses([]model.PRStatus{{
		Repo:         "pingcap/tidb",
		Num:          97902,
		Title:        "planner: manual tracked PR",
		State:        "OPEN",
		UpdatedAt:    "2026-11-12T12:00:00Z",
		TrackSources: []string{"manual_cli"},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Attach manual PR link #97902 while offline (inserts or links against #97002), then simulate UNKNOWN link + subset upsert
	if _, err := st.ApplyBatchTriageDecisions(model.BatchTriageDecisions{
		ManualPRLinks: map[string][]string{"97002": {"97902"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.db.Exec(`UPDATE issue_pr_links SET state = 'UNKNOWN', updated_at = '' WHERE issue_num = 97002 AND pr_num = 97902`)
	// Upsert older/same timestamp with subset TrackSources ["issue_linked"]
	if err := st.UpsertPRStatuses([]model.PRStatus{{
		Repo:         "pingcap/tidb",
		Num:          97902,
		Title:        "planner: manual tracked PR",
		State:        "OPEN",
		UpdatedAt:    "2026-11-12T11:00:00Z",
		TrackSources: []string{"issue_linked"},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	openTargets, err := st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	sawManualCLI := false
	for _, tgt := range openTargets {
		if tgt.Number == 97902 {
			for _, src := range tgt.TrackSources {
				if src == "manual_cli" {
					sawManualCLI = true
				}
			}
		}
	}
	if !sawManualCLI {
		t.Fatalf("expected manual_cli TrackSource preserved across subset UpsertPRStatuses, got %+v", openTargets)
	}
	var linkState string
	if err := st.db.QueryRow(`SELECT state FROM issue_pr_links WHERE issue_num = 97002 AND pr_num = 97902`).Scan(&linkState); err != nil || linkState != "OPEN" {
		t.Fatalf("expected lagging UNKNOWN issue_pr_link hydrated to OPEN even when pull_requests skipped stale write, got %q err=%v", linkState, err)
	}

	// 5. Finding #4 & Gap #2: PersistEnrichedPendingCandidates advances SyncWatermark when PR UpdatedAt/State changes
	pBefore, err := st.ListPendingCandidates()
	if err != nil || len(pBefore) != 1 {
		t.Fatalf("expected 1 pending candidate #97101, got %+v err=%v", pBefore, err)
	}
	wmBefore := pBefore[0].SyncWatermark
	updatedCand := pBefore[0]
	updatedCand.PRs[0].UpdatedAt = "2026-11-12T12:30:00Z" // new commit pushed to OPEN PR
	survived, err := st.PersistEnrichedPendingCandidates([]model.PendingCandidate{updatedCand})
	if err != nil || len(survived) != 1 || survived[0].SyncWatermark <= wmBefore {
		t.Fatalf("expected PersistEnrichedPendingCandidates to advance SyncWatermark on new PR commit (%+v vs %q), err=%v", survived, wmBefore, err)
	}

	// 6. Finding #6 & #12: RejectDBTargetCollision blocks file: URI & -wal collisions; OpenSnapshotForDryRun leaves live DB untouched
	fileURI := "file:" + dbPath + "?cache=shared"
	if err := RejectDBTargetCollision(dbPath, fileURI); err == nil {
		t.Fatalf("expected RejectDBTargetCollision to reject dest colliding with file: URI activeDBPath")
	}
	if err := RejectDBTargetCollision(fileURI, dbPath); err == nil {
		t.Fatalf("expected RejectDBTargetCollision to reject file: URI destPath colliding with live DB")
	}
	if err := RejectDBTargetCollision(dbPath+"-wal", fileURI); err == nil {
		t.Fatalf("expected RejectDBTargetCollision to reject -wal companion file of live DB")
	}
	snapStore, cleanupSnap, err := OpenSnapshotForDryRun(fileURI, "", nil)
	if err != nil {
		t.Fatalf("OpenSnapshotForDryRun(%s) failed: %v", fileURI, err)
	}
	// Mutate inside snapshot store and verify live store does NOT see it!
	if err := snapStore.ApplyIssueOverride(97002, model.TriagePatchRequest{
		Cat:       "HARD",
		Severity:  "CRITICAL",
		Subsystem: "logical_rewrite",
	}); err != nil {
		cleanupSnap()
		t.Fatal(err)
	}
	cleanupSnap()
	dsLive, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dsLive.Records {
		if r.Number == 97002 && (r.Cat != "EASY" || r.ManualOverride) {
			t.Fatalf("expected live DB #97002 to remain EASY without override after mutating OpenSnapshotForDryRun copy, got %+v", r)
		}
	}
}
