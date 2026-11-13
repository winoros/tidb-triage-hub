package store

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	// MissingPRTargets should surface #73064 (from seed without pr_status) and newly linked pingcap/tiflash#11200
	foundTiflash := false
	for _, mt := range bRes.MissingPRTargets {
		if mt.Repo == "pingcap/tiflash" && mt.Number == 11200 {
			foundTiflash = true
		}
	}
	if !foundTiflash {
		t.Fatalf("expected pingcap/tiflash#11200 in MissingPRTargets, got %+v", bRes.MissingPRTargets)
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

	// Verify FILTER_NON_OPT_OR_FIXED PR #73500 is isolated from ListOpenPRTargets, while #68000 is included
	openTargets, err := st.ListOpenPRTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(openTargets) != 1 || openTargets[0].Number != 68000 {
		t.Fatalf("expected only #68000 in ListOpenPRTargets (isolating #73500), got %+v", openTargets)
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
