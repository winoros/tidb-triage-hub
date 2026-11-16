package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

type stubBundleRunner struct {
	failPRFiles bool
}

func (r *stubBundleRunner) Query(_ context.Context, gql string, _ map[string]string) ([]byte, error) {
	if strings.Contains(gql, "files(first:") {
		if r.failPRFiles {
			return nil, fmt.Errorf("simulated GitHub PR files enrichment outage")
		}
		return json.Marshal(map[string]any{
			"data": map[string]any{
				"rateLimit": map[string]any{"limit": 5000, "cost": 1, "remaining": 4990, "resetAt": "2026-11-16T12:00:00Z"},
				"repository": map[string]any{
					"p96901": map[string]any{
						"number":      96901,
						"title":       "planner: fix issue 96001",
						"url":         "https://github.com/pingcap/tidb/pull/96901",
						"state":       "MERGED",
						"mergedAt":    "2026-11-16T01:00:00Z",
						"baseRefName": "master",
						"body":        "close #96001",
						"files": map[string]any{
							"nodes": []any{
								map[string]any{"path": "pkg/planner/core/rule_predicate_push_down.go", "additions": 18, "deletions": 4},
							},
						},
					},
				},
			},
		})
	}
	return json.Marshal(map[string]any{
		"data": map[string]any{
			"rateLimit": map[string]any{"limit": 5000, "cost": 0, "remaining": 4990, "resetAt": "2026-11-16T12:00:00Z"},
		},
	})
}

func TestBuildPendingBundleDegradedEnrichmentGuardAndRetriageLivePRSync(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "bundle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cachedSug := &model.SuggestedTriage{
		RecommendedAction: "accept",
		Cat:               "HARD",
		Severity:          "CRITICAL",
		Subsystem:         "cost_cardinality",
		Confidence:        "high",
		Signals:           []string{"cached:rich-pr-signal"},
	}
	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records: []model.IssueRecord{{
			Number:      96001,
			Title:       "planner: open issue whose PR merged on GitHub before retriage bundle",
			Author:      "alice",
			Created:     "2026-11-12",
			CreatedAt:   "2026-11-12T10:00:00Z",
			UpdatedAt:   "2026-11-12T10:00:00Z",
			Labels:      []string{"sig/planner", "type/bug"},
			IsCanonical: true,
			Cat:         "MEDIUM",
			Severity:    "MAJOR",
			Subsystem:   "logical_rewrite",
			Note:        &model.LocalizedNote{ZhCN: "测试重审导出同步最新PR合并状态", En: "Test retriage bundle syncs live MERGED PR state"},
			PRs:         []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 96901, State: "OPEN"}},
		}},
	}, []model.PendingCandidate{{
		Number:          96101,
		Title:           "planner: pending candidate with pre-cached enrichment",
		Author:          "bob",
		CreatedAt:       "2026-11-12T10:00:00Z",
		UpdatedAt:       "2026-11-12T10:05:00Z",
		Labels:          []string{"sig/planner", "type/bug"},
		PRs:             []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 96902, State: "OPEN"}},
		SuggestedTriage: cachedSug,
	}}); err != nil {
		t.Fatal(err)
	}

	// 1. When EnrichPRs fails (failPRFiles=true), cached SuggestedTriage in SQLite MUST NOT be overwritten with degraded fallback
	syFail := NewWithRunner(st, &stubBundleRunner{failPRFiles: true})
	bundleFail, _, err := syFail.BuildPendingBundle(context.Background(), PendingBundleOptions{
		OnlyPending: true,
		EnrichPRs:   true,
		Compact:     true,
	})
	if err != nil {
		t.Fatalf("BuildPendingBundle on enrichment failure should degrade gracefully, got %v", err)
	}
	if warn, _ := bundleFail["enrichment_warning"].(string); warn == "" {
		t.Fatalf("expected enrichment_warning when PR enrichment fails, got %+v", bundleFail["enrichment_warning"])
	}
	storedAfterFail, err := st.ListPendingCandidates()
	if err != nil || len(storedAfterFail) != 1 || storedAfterFail[0].SuggestedTriage == nil || storedAfterFail[0].SuggestedTriage.Cat != "HARD" {
		t.Fatalf("expected cached SuggestedTriage (Cat=HARD) preserved in SQLite when EnrichPRs fails, got %+v err=%v", storedAfterFail, err)
	}

	// 2. On --re-triage 96001 with working enrichment, live PR #96901=MERGED is synced into retriage_candidates[].prs BEFORE SuggestTriage!
	syOK := NewWithRunner(st, &stubBundleRunner{failPRFiles: false})
	bundleOK, _, err := syOK.BuildPendingBundle(context.Background(), PendingBundleOptions{
		ReTriageNums: []int{96001},
		EnrichPRs:    true,
		Compact:      true,
	})
	if err != nil {
		t.Fatalf("BuildPendingBundle(--re-triage 96001) failed: %v", err)
	}
	rawRC, _ := json.Marshal(bundleOK["retriage_candidates"])
	var rCands []model.RetriageCandidate
	if err := json.Unmarshal(rawRC, &rCands); err != nil || len(rCands) != 1 {
		t.Fatalf("expected 1 retriage candidate, got len=%d err=%v", len(rCands), err)
	}
	if len(rCands[0].PRs) != 1 || rCands[0].PRs[0].State != "MERGED" {
		t.Fatalf("expected retriage candidate PR #96901 synced to MERGED, got %+v", rCands[0].PRs)
	}
	if rCands[0].SuggestedTriage == nil || rCands[0].SuggestedTriage.Cat != "FILTER_NON_OPT_OR_FIXED" {
		t.Fatalf("expected SuggestTriage to see live MERGED PR and suggest FILTER_NON_OPT_OR_FIXED, got %+v", rCands[0].SuggestedTriage)
	}

	// 3. Cancelled context returns context.Canceled cleanly
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := syOK.BuildPendingBundle(canceledCtx, PendingBundleOptions{EnrichPRs: false}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on cancelled ctx, got %v", err)
	}
}

type stubTargetedSyncRunner struct {
	cancelOnPRFetch func()
}

func (r *stubTargetedSyncRunner) Query(_ context.Context, gql string, _ map[string]string) ([]byte, error) {
	if strings.Contains(gql, "...PRStatusFields") {
		if r.cancelOnPRFetch != nil {
			r.cancelOnPRFetch()
		}
		repoMap := map[string]any{}
		if strings.Contains(gql, "number: 98001") {
			repoMap["p98001"] = map[string]any{
				"number":      98001,
				"title":       "planner: manual cli tracked PR",
				"url":         "https://github.com/pingcap/tidb/pull/98001",
				"state":       "OPEN",
				"isDraft":     false,
				"mergeable":   "MERGEABLE",
				"createdAt":   "2026-11-16T01:00:00Z",
				"updatedAt":   "2026-11-16T02:00:00Z",
				"baseRefName": "master",
				"author":      map[string]any{"login": "alice"},
			}
		}
		if strings.Contains(gql, "number: 98999") {
			repoMap["p98999"] = nil // nonexistent GitHub PR
		}
		return json.Marshal(map[string]any{
			"data": map[string]any{
				"rateLimit":  map[string]any{"limit": 5000, "cost": 1, "remaining": 4990, "resetAt": "2026-11-16T12:00:00Z"},
				"repository": repoMap,
			},
		})
	}
	return json.Marshal(map[string]any{
		"data": map[string]any{
			"rateLimit": map[string]any{"limit": 5000, "cost": 0, "remaining": 4990, "resetAt": "2026-11-16T12:00:00Z"},
		},
	})
}

func TestExecuteSyncPRsManualSourceMissingPRAndPreCommitCancel(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "sync_targeted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-16T00:00:00Z"},
		Records: []model.IssueRecord{{
			Number:      98100,
			Title:       "planner: test cancel before SQLite commit",
			Author:      "alice",
			Created:     "2026-11-16",
			CreatedAt:   "2026-11-16T00:00:00Z",
			UpdatedAt:   "2026-11-16T00:00:00Z",
			Labels:      []string{"sig/planner", "type/bug"},
			IsCanonical: true,
			Cat:         "EASY",
			Severity:    "MINOR",
			Subsystem:   "statistics",
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	sy := NewWithRunner(st, &stubTargetedSyncRunner{})

	// 1. Finding #5: ExecuteSyncPRs stamps "manual_cli" so ListOpenPRTargets retains #98001
	if _, err := sy.ExecuteSyncPRs(context.Background(), "98001", 0); err != nil {
		t.Fatalf("ExecuteSyncPRs(98001) failed: %v", err)
	}
	targets, err := st.ListOpenPRTargets()
	if err != nil || len(targets) != 1 || targets[0].Number != 98001 {
		t.Fatalf("expected #98001 retained in ListOpenPRTargets via manual_cli TrackSource, got %+v err=%v", targets, err)
	}

	// 2. Finding #5: Nonexistent PR #98999 returns ErrMissingGitHubPR instead of silent success
	if _, err := sy.ExecuteSyncPRs(context.Background(), "98999", 0); !errors.Is(err, ErrMissingGitHubPR) {
		t.Fatalf("expected ErrMissingGitHubPR for nonexistent PR #98999, got %v", err)
	}

	// 3. Finding #7: Cancelling ctx during pre-write PR fetch aborts BEFORE committing batch triage to SQLite!
	ctx, cancel := context.WithCancel(context.Background())
	syCancel := NewWithRunner(st, &stubTargetedSyncRunner{cancelOnPRFetch: cancel})
	_, err = syCancel.ApplyTriageDecisionsWithOptions(ctx, model.BatchTriageDecisions{
		Overrides: map[string]model.TriagePatchRequest{
			"98100": {
				Cat:       "HARD",
				Severity:  "CRITICAL",
				Subsystem: "logical_rewrite",
			},
		},
		ManualPRLinks: map[string][]string{"98100": {"98002"}},
	}, store.BatchApplyOptions{}, true, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled when cancelled before SQLite commit, got %v", err)
	}
	dsAfter, err := st.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	if dsAfter.Records[0].Cat != "EASY" || dsAfter.Records[0].ManualOverride {
		t.Fatalf("expected cancelled ApplyTriageDecisions NOT to commit override to SQLite, got %+v", dsAfter.Records[0])
	}
}
