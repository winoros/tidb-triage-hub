package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
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
