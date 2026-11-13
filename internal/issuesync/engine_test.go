package issuesync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

func TestSeedDatasetZeroDropsRetention(t *testing.T) {
	raw, err := os.ReadFile("../../seed/initial_dataset.json")
	if err != nil {
		t.Fatalf("read seed dataset: %v", err)
	}
	var ds model.Dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatalf("unmarshal seed dataset: %v", err)
	}
	if len(ds.Records) == 0 {
		t.Fatal("expected non-empty seed dataset")
	}

	for _, rec := range ds.Records {
		keep, reason := ShouldRetainTrackedIssue(rec.Labels, rec.ForceKeep, rec.Cat)
		if !keep {
			t.Errorf("seed issue #%d (cat=%s, labels=%v) unexpectedly dropped: %s", rec.Number, rec.Cat, rec.Labels, reason)
		}
	}

	// Verify graduation still triggers when relabeled to enhancement or non-planner SIG
	if keep, _ := ShouldRetainExistingIssue([]string{"sig/planner", "type/enhancement"}, false); keep {
		t.Error("expected sig/planner + type/enhancement to graduate when forceKeep=false")
	}
	if keep, _ := ShouldRetainExistingIssue([]string{"sig/planner", "type/enhancement"}, true); !keep {
		t.Error("expected forceKeep=true to override graduation")
	}
	if keep, _ := ShouldRetainExistingIssue([]string{"sig/execution", "type/bug"}, false); keep {
		t.Error("expected sig/execution + type/bug without sig/planner to graduate")
	}
}

func TestMatchDiscoveryScopes(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

	// Tier-1 recalls even old 2023 issues regardless of 90d lookback
	s1 := MatchDiscoveryScopes(DiscoveryCandidateInput{
		Title:     "old canonical planner bug",
		CreatedAt: "2023-05-01T00:00:00Z",
		Labels:    []string{"sig/planner", "type/bug"},
		Now:       now,
	})
	if len(s1) != 1 || s1[0] != "canonical_planner_bug" {
		t.Fatalf("expected canonical_planner_bug, got %v", s1)
	}

	// Tier-2 blocks non-optimizer noise (expression vectorized cast, tikv-client, coderabbitai, flaky)
	noiseCases := []DiscoveryCandidateInput{
		{
			Title:     "expression: cast decimal to json panics in vecEval",
			CreatedAt: "2026-11-01T00:00:00Z",
			Author:    "alice",
			Labels:    []string{"type/bug", "component/expression"},
			Now:       now,
		},
		{
			Title:     "tikv-client: region cache backoff too slow",
			CreatedAt: "2026-11-01T00:00:00Z",
			Author:    "bob",
			Labels:    []string{"type/bug", "component/tikv-client"},
			Now:       now,
		},
		{
			Title:     "planner: refactor plan cache helper interface",
			CreatedAt: "2026-11-01T00:00:00Z",
			Author:    "coderabbitai",
			Labels:    []string{},
			Now:       now,
		},
		{
			Title:     "flaky test: TestIndexMergeReader",
			CreatedAt: "2026-11-01T00:00:00Z",
			Author:    "ci-user",
			Labels:    []string{"type/bug"},
			Now:       now,
		},
	}
	for i, tc := range noiseCases {
		if got := MatchDiscoveryScopes(tc); len(got) != 0 {
			t.Errorf("noise case %d (%q) should be rejected, got %v", i, tc.Title, got)
		}
	}

	// Ghost author (deleted human account) MUST NOT be blocked as a bot
	ghostScopes := MatchDiscoveryScopes(DiscoveryCandidateInput{
		Title:     "Prepared plan cache returns wrong result with INL_JOIN hint",
		CreatedAt: "2026-11-05T00:00:00Z",
		Author:    "ghost",
		Labels:    []string{"type/bug"},
		Now:       now,
	})
	if len(ghostScopes) == 0 {
		t.Fatal("expected ghost author with strong optimizer title to be discovered")
	}

	// Body S2 signal discovers untriaged bug with optimizer stack trace
	bodyScopes := MatchDiscoveryScopes(DiscoveryCandidateInput{
		Title:     "SELECT query panics with runtime error",
		Body:      "goroutine 1: github.com/pingcap/tidb/pkg/planner/core.(*PlanBuilder).buildSelect()",
		CreatedAt: "2026-11-10T00:00:00Z",
		Author:    "user1",
		Labels:    []string{"type/bug"},
		Now:       now,
	})
	if len(bodyScopes) != 1 || bodyScopes[0] != "affinity_opt_body" {
		t.Fatalf("expected [affinity_opt_body], got %v", bodyScopes)
	}

	// Tier-2 older than 90d lookback is skipped
	oldTier2 := MatchDiscoveryScopes(DiscoveryCandidateInput{
		Title:     "Prepared plan cache returns wrong result",
		CreatedAt: "2026-01-01T00:00:00Z",
		Author:    "user1",
		Labels:    []string{"type/bug"},
		Now:       now,
	})
	if len(oldTier2) != 0 {
		t.Fatalf("expected Tier-2 outside 90d window to be skipped, got %v", oldTier2)
	}
}

func TestShouldReadmitExcluded(t *testing.T) {
	// Excluded while already carrying sig/planner (e.g. non-bug discussion): must NOT loop
	if got := ShouldReadmitExcluded([]string{"sig/planner"}, []string{"sig/planner"}); len(got) != 0 {
		t.Fatalf("expected no readmission when sig/planner was already present, got %v", got)
	}
	// If it later gains type/bug, it becomes canonical -> MUST readmit
	if got := ShouldReadmitExcluded([]string{"sig/planner", "type/bug"}, []string{"sig/planner"}); len(got) == 0 {
		t.Fatal("expected readmission when excluded sig/planner issue gains type/bug")
	}
	// Gaining component/statistics -> readmit
	if got := ShouldReadmitExcluded([]string{"type/bug", "component/statistics"}, []string{"type/bug"}); len(got) != 1 || got[0] != "readmitted:+component/statistics" {
		t.Fatalf("expected readmitted:+component/statistics, got %v", got)
	}
	// Gaining component/mview alone -> NOT readmitted (noisy UI/DDL component)
	if got := ShouldReadmitExcluded([]string{"type/bug", "component/mview"}, []string{"type/bug"}); len(got) != 0 {
		t.Fatalf("expected component/mview not to trigger readmission, got %v", got)
	}
}

func TestExtractTiDBIssueRefsFromText(t *testing.T) {
	tidbText := `Fixes #73063, related to (#73064) and pingcap/tidb#65653.
Ignore tikv/tikv#18000, tiflash#11155, path/#12345, and https://github.com/tikv/tikv/issues/19999.
Also see https://github.com/pingcap/tidb/issues/70001.`
	got := ExtractTiDBIssueRefsFromText("pingcap/tidb", tidbText)
	want := []int{65653, 70001, 73063, 73064}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pingcap/tidb refs = %v, want %v", got, want)
	}

	// Cross-repo PR (pingcap/tiflash) must NOT treat bare #11155 as TiDB issue
	tiflashText := `Closes #11155, ref pingcap/tidb#73063`
	gotCross := ExtractTiDBIssueRefsFromText("pingcap/tiflash", tiflashText)
	if fmt.Sprint(gotCross) != "[73063]" {
		t.Fatalf("pingcap/tiflash refs = %v, want [73063]", gotCross)
	}
}

type mockSearchRunner struct {
	mu      sync.Mutex
	queries []string
	handler func(q string, cursor string) ([]byte, error)
}

func (m *mockSearchRunner) Query(_ context.Context, _ string, vars map[string]string) ([]byte, error) {
	q := vars["q"]
	cur := vars["cursor"]
	m.mu.Lock()
	m.queries = append(m.queries, q+"|"+cur)
	m.mu.Unlock()
	return m.handler(q, cur)
}

func TestSearchWithBisection(t *testing.T) {
	ctx := context.Background()
	lo := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2026, 11, 10, 0, 1, 0, 0, time.UTC) // 60s window

	// Case 1: <= threshold reuses page 1 without duplicate query, then fetches page 2
	r1 := &mockSearchRunner{
		handler: func(q string, cursor string) ([]byte, error) {
			if cursor == "" {
				return []byte(`{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999},"search":{"issueCount":2,"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"number":101,"updatedAt":"2026-11-10T00:00:10Z"}]}}}`), nil
			}
			return []byte(`{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4998},"search":{"issueCount":2,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"number":102,"updatedAt":"2026-11-10T00:00:20Z"}]}}}`), nil
		},
	}
	tracker := ghgql.NewCostTracker()
	nodes, err := SearchWithBisection(ctx, r1, tracker, SearchIssuesGQL, "repo:pingcap/tidb is:issue", lo, hi, 800, decodeIssueSearchNode)
	if err != nil {
		t.Fatalf("bisect <=800: %v", err)
	}
	if len(nodes) != 2 || len(r1.queries) != 2 {
		t.Fatalf("expected 2 nodes and exactly 2 queries (no duplicate page 1), got %d nodes, %d queries", len(nodes), len(r1.queries))
	}

	// Case 2: > threshold splits [00:00:00..00:01:00] into [00:00:00..00:00:30] and [00:00:31..00:01:00]
	r2 := &mockSearchRunner{
		handler: func(q string, _ string) ([]byte, error) {
			if strings.Contains(q, "2026-11-10T00:00:00Z..2026-11-10T00:01:00Z") {
				return []byte(`{"data":{"search":{"issueCount":950,"pageInfo":{"hasNextPage":true,"endCursor":"x"},"nodes":[]}}}`), nil
			}
			if strings.Contains(q, "2026-11-10T00:00:00Z..2026-11-10T00:00:30Z") {
				return []byte(`{"data":{"search":{"issueCount":2,"pageInfo":{"hasNextPage":false},"nodes":[{"number":201,"updatedAt":"2026-11-10T00:00:05Z"},{"number":202,"updatedAt":"2026-11-10T00:00:10Z"}]}}}`), nil
			}
			if strings.Contains(q, "2026-11-10T00:00:31Z..2026-11-10T00:01:00Z") {
				// #202 updated again in second half -> dedup keeps newer timestamp
				return []byte(`{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false},"nodes":[{"number":202,"updatedAt":"2026-11-10T00:00:45Z"},{"number":203,"updatedAt":"2026-11-10T00:00:50Z"}]}}}`), nil
			}
			return nil, fmt.Errorf("unexpected query %s", q)
		},
	}
	nodes2, err := SearchWithBisection(ctx, r2, nil, SearchIssuesGQL, "repo:pingcap/tidb is:issue", lo, hi, 800, decodeIssueSearchNode)
	if err != nil {
		t.Fatalf("bisect >800: %v", err)
	}
	if len(nodes2) != 3 {
		t.Fatalf("expected 3 deduplicated nodes, got %d", len(nodes2))
	}
	if nodes2[1].Number != 202 || nodes2[1].UpdatedAt != "2026-11-10T00:00:45Z" {
		t.Fatalf("expected #202 latest updatedAt 00:00:45Z, got %+v", nodes2[1])
	}

	// Case 3: 1-second >800 burst splits orthogonally by created:<2025-01-01 and created:>=2025-01-01
	r3 := &mockSearchRunner{
		handler: func(q string, _ string) ([]byte, error) {
			if strings.Contains(q, "created:<2025-01-01") {
				return []byte(`{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false},"nodes":[{"number":301,"updatedAt":"2026-11-10T00:00:00Z"}]}}}`), nil
			}
			if strings.Contains(q, "created:>=2025-01-01") {
				return []byte(`{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false},"nodes":[{"number":302,"updatedAt":"2026-11-10T00:00:00Z"}]}}}`), nil
			}
			return []byte(`{"data":{"search":{"issueCount":900,"pageInfo":{"hasNextPage":true},"nodes":[]}}}`), nil
		},
	}
	nodes3, err := SearchWithBisection(ctx, r3, nil, SearchIssuesGQL, "repo:pingcap/tidb is:issue", lo, lo, 800, decodeIssueSearchNode)
	if err != nil {
		t.Fatalf("bisect 1s burst: %v", err)
	}
	if len(nodes3) != 2 {
		t.Fatalf("expected 2 nodes from orthogonal 1s burst split, got %d", len(nodes3))
	}
}

func TestRangerDiscoveryEnhancementRetentionAndUnknownBaselineExclusion(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

	// 1. pkg/util/ranger stack trace triggers Tier-2 optimizer body discovery
	scopes := MatchDiscoveryScopes(DiscoveryCandidateInput{
		Title:     "index scan returns wrong rows on prefix key",
		Body:      "panic in github.com/pingcap/tidb/pkg/util/ranger.(*dNF).detachDNFCondAndBuildRangeForIndex()",
		CreatedAt: "2026-11-10T00:00:00Z",
		Author:    "reporter1",
		Labels:    []string{"type/bug"},
		Now:       now,
	})
	if len(scopes) != 1 || scopes[0] != "affinity_opt_body" {
		t.Fatalf("expected pkg/util/ranger to trigger [affinity_opt_body], got %v", scopes)
	}

	// 2. ENHANCEMENT-triaged issue stays retained when maintainer aligns GitHub label to type/enhancement
	if keep, reason := ShouldRetainTrackedIssue([]string{"sig/planner", "type/enhancement"}, false, "ENHANCEMENT"); !keep {
		t.Fatalf("expected ENHANCEMENT issue to be retained on sig/planner + type/enhancement, got dropped (%s)", reason)
	}
	// Real bug issue still graduates when relabeled to type/enhancement without force_keep
	if keep, _ := ShouldRetainTrackedIssue([]string{"sig/planner", "type/enhancement"}, false, "MEDIUM"); keep {
		t.Fatal("expected MEDIUM bug relabeled to type/enhancement to graduate when forceKeep=false")
	}

	// 3. Unknown baseline labels (legacy seed_removed '[]') must NOT false-readmit on component/statistics,
	//    unless promoted to canonical sig/planner + type/bug
	if got := EvaluateExcludedReadmission([]string{"type/bug", "component/statistics"}, ExcludedIssueState{Labels: nil, LabelsKnown: false}); len(got) != 0 {
		t.Fatalf("expected unknown-baseline excluded issue not to false-readmit on non-canonical label, got %v", got)
	}
	if got := EvaluateExcludedReadmission([]string{"sig/planner", "type/bug"}, ExcludedIssueState{Labels: nil, LabelsKnown: false}); len(got) == 0 {
		t.Fatal("expected unknown-baseline excluded issue to readmit when promoted to canonical sig/planner + type/bug")
	}
	// Hard-negative sig/execution blocks readmission even if component/statistics was added
	if got := EvaluateExcludedReadmission([]string{"type/bug", "sig/execution", "component/statistics"}, ExcludedIssueState{Labels: []string{"type/bug"}, LabelsKnown: true}); len(got) != 0 {
		t.Fatalf("expected hard-negative sig/execution to block readmission, got %v", got)
	}
}

func TestPlanSyncExcludedPrecedencePendingReuseAndRecheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	var batchQueries []string
	runner := &mockSearchRunner{
		handler: func(q string, _ string) ([]byte, error) {
			switch {
			case strings.Contains(q, "is:pr"):
				// Stream B: PR #88001 used to reference #74001 (tracked) and #74002 (pending),
				// now only references #74002 (edited out #74001!).
				return []byte(`{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false},"nodes":[
					{"number":88001,"title":"planner: fix range","body":"Fixes #74002","state":"OPEN","isDraft":false,"baseRefName":"master","url":"https://github.com/pingcap/tidb/pull/88001","updatedAt":"2026-11-13T11:55:00Z","repository":{"nameWithOwner":"pingcap/tidb"},"labels":{"nodes":[{"name":"sig/planner"}]}}
				]}}}`), nil
			case strings.Contains(q, "is:issue"):
				// Stream A returns:
				// - #74003: present in BOTH ExcludedIssues AND ArchivedIssues -> Excluded MUST win!
				return []byte(`{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false},"nodes":[
					{"number":74003,"title":"mview dashboard UI","state":"OPEN","createdAt":"2026-11-01T00:00:00Z","updatedAt":"2026-11-13T11:50:00Z","author":{"login":"bob"},"labels":{"nodes":[{"name":"component/mview"},{"name":"type/bug"}]}}
				]}}}`), nil
			default:
				batchQueries = append(batchQueries, "batch")
				return []byte(`{"data":{"repository":{
					"i74001":{"number":74001,"title":"tracked planner bug","body":"desc","state":"OPEN","createdAt":"2026-11-01T00:00:00Z","updatedAt":"2026-11-12T00:00:00Z","url":"https://github.com/pingcap/tidb/issues/74001","author":{"login":"alice"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"sig/planner"},{"name":"type/bug"}]},"comments":{"nodes":[]},"timelineItems":{"nodes":[]}},
					"i74002":{"number":74002,"title":"pending planner bug","body":"desc","state":"OPEN","createdAt":"2026-11-10T00:00:00Z","updatedAt":"2026-11-12T00:00:00Z","url":"https://github.com/pingcap/tidb/issues/74002","author":{"login":"carol"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"sig/planner"},{"name":"type/bug"}]},"comments":{"nodes":[]},"timelineItems":{"nodes":[]}},
					"i74004":{"number":74004,"title":"restored manual issue","body":"desc","state":"OPEN","createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-06T00:00:00Z","url":"https://github.com/pingcap/tidb/issues/74004","author":{"login":"dave"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"type/bug"}]},"comments":{"nodes":[]},"timelineItems":{"nodes":[]}}
				}}}`), nil
			}
		},
	}

	state := SyncState{
		PrevMeta: model.SyncMeta{
			SnapshotUTC:        "2026-11-13T11:00:00Z",
			MaxIssueUpdatedUTC: "2026-11-13T10:55:00Z",
		},
		TrackedIssues: map[int]TrackedIssueState{
			74001: {
				Number:    74001,
				UpdatedAt: "2026-11-12T00:00:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
				Cat:       "MEDIUM",
				PRs: []TrackedPRLink{
					{Repo: "pingcap/tidb", Num: 88001, State: "OPEN", Source: model.LinkSourcePRRef},
				},
			},
		},
		ArchivedIssues: map[int]ArchivedIssueState{
			74003: {Number: 74003, Cat: "FILTER_NON_OPT_OR_FIXED"},
		},
		ExcludedIssues: map[int]ExcludedIssueState{
			74003: {Labels: []string{"component/mview", "type/bug"}, LabelsKnown: true},
		},
		PendingScopes: map[int][]string{
			74002: {"canonical_planner_bug"},
			74005: {"canonical_planner_bug"}, // untouched pending candidate -> skipped in incremental mode!
		},
		PendingCandidates: map[int]model.PendingCandidate{
			74002: {
				Number:        74002,
				Title:         "pending planner bug",
				UpdatedAt:     "2026-11-12T00:00:00Z",
				Labels:        []string{"sig/planner", "type/bug"},
				MatchedScopes: []string{"canonical_planner_bug"},
				PRs: []model.LinkedPRRef{
					{Repo: "pingcap/tidb", Num: 88001, Label: "#88001", State: "OPEN", Source: model.LinkSourcePRRef},
					{Repo: "pingcap/tidb", Num: 88099, Label: "#88099", State: "OPEN", Source: model.LinkSourcePRRef},
				},
			},
			74005: {
				Number:        74005,
				Title:         "untouched pending candidate",
				UpdatedAt:     "2026-11-11T00:00:00Z",
				Labels:        []string{"sig/planner", "type/bug"},
				MatchedScopes: []string{"canonical_planner_bug"},
			},
		},
		RecheckNums: []int{74004},
	}

	eng := NewEngine(runner)
	plan, err := eng.PlanSync(ctx, state, Options{Now: now})
	if err != nil {
		t.Fatalf("PlanSync: %v", err)
	}

	// 1. #74003 must NOT be restored from ArchivedIssues because ExcludedIssues takes precedence
	if len(plan.RestoredIssues) != 0 {
		t.Fatalf("expected 0 restored issues (#74003 is excluded), got %+v", plan.RestoredIssues)
	}
	// 2. #88001 edited out its reference to #74001 -> UnlinkedStreamBPRs records it
	if len(plan.UnlinkedStreamBPRs[74001]) != 1 || plan.UnlinkedStreamBPRs[74001][0] != "pingcap/tidb#88001" {
		t.Fatalf("expected #88001 unlinked from #74001, got %+v", plan.UnlinkedStreamBPRs)
	}
	// 3. Re-fetched #74002 preserved its older Stream B PR #88099 alongside #88001, untouched #74005 was neither re-fetched nor deleted, and #74004 was queued via RecheckNums
	found74004, found74002Preserved := false, false
	for _, pc := range plan.UpsertPending {
		if pc.Number == 74004 && len(pc.MatchedScopes) == 1 && pc.MatchedScopes[0] == "restored_manual" {
			found74004 = true
		}
		if pc.Number == 74002 && len(pc.PRs) == 2 {
			found74002Preserved = true
		}
		if pc.Number == 74005 {
			t.Fatalf("expected untouched pending #74005 not to be re-fetched/upserted, got %+v", pc)
		}
	}
	for _, delNum := range plan.DeletePendingNums {
		if delNum == 74005 {
			t.Fatal("expected untouched pending #74005 not to be deleted")
		}
	}
	if !found74004 {
		t.Fatalf("expected RecheckNums #74004 to be queued with restored_manual scope, got %+v", plan.UpsertPending)
	}
	if !found74002Preserved {
		t.Fatalf("expected #74002 to preserve older Stream B PR #88099 alongside #88001, got %+v", plan.UpsertPending)
	}
	if len(plan.ClearedRecheckNums) != 1 || plan.ClearedRecheckNums[0] != 74004 {
		t.Fatalf("expected ClearedRecheckNums=[74004], got %v", plan.ClearedRecheckNums)
	}
}

func TestRound2TruncatedPRCommentsManualRestoreArchivedAndNotFound(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	// 1. Multi-sig / multi-component Tier-1 scopes and readmission
	multiScopes := MatchDiscoveryScopes(DiscoveryCandidateInput{
		Title:     "planner: wrong result with MPP join",
		CreatedAt: "2026-11-10T00:00:00Z",
		Labels:    []string{"sig/planner", "sig/execution", "component/statistics", "planner/cascades"},
		Now:       now,
	})
	if !strings.Contains(strings.Join(multiScopes, ","), "planner_cascades") {
		t.Fatalf("expected planner/cascades to be captured alongside sig/planner, got %v", multiScopes)
	}
	if got := EvaluateExcludedReadmission(
		[]string{"component/expression", "component/statistics", "type/bug"},
		ExcludedIssueState{Labels: []string{"component/expression", "type/bug"}, LabelsKnown: true},
	); len(got) == 0 {
		t.Fatal("expected excluded multi-component issue gaining component/statistics to be readmitted for review")
	}
	if got := EvaluateExcludedReadmission(
		[]string{"sig/planner", "sig/execution", "type/bug"},
		ExcludedIssueState{Labels: []string{"sig/execution", "type/bug"}, LabelsKnown: true},
	); len(got) == 0 {
		t.Fatal("expected excluded sig/execution issue gaining sig/planner (multi-sig canonical) to be readmitted")
	}

	// 2. PlanSync:
	//    - PR #89001 has truncated comments (totalCount=80 > 30) so it MUST NOT falsely unlink #76001's pr_ref!
	//    - Tracked #76002 returns null (NOT_FOUND / transferred) -> removed via ClosedRemovals!
	//    - Archived + RecheckNums #76003 (relabeled to non-retained sig/execution) -> queued via restored_manual!
	//    - Pending #76004 relabeled to type/enhancement -> dropped from pending via DeletePendingNums!
	runner := &mockSearchRunner{
		handler: func(q string, _ string) ([]byte, error) {
			switch {
			case strings.Contains(q, "is:pr"):
				return []byte(`{"data":{"search":{"issueCount":1,"pageInfo":{"hasNextPage":false},"nodes":[
					{"number":89001,"title":"planner: refactor","body":"no ref in body","state":"OPEN","isDraft":false,"baseRefName":"master","url":"https://github.com/pingcap/tidb/pull/89001","updatedAt":"2026-11-13T11:55:00Z","repository":{"nameWithOwner":"pingcap/tidb"},"labels":{"nodes":[{"name":"sig/planner"}]},"comments":{"totalCount":80,"nodes":[]}}
				]}}}`), nil
			case strings.Contains(q, "is:issue"):
				return []byte(`{"data":{"search":{"issueCount":2,"pageInfo":{"hasNextPage":false},"nodes":[
					{"number":76001,"title":"tracked bug","state":"OPEN","createdAt":"2026-11-01T00:00:00Z","updatedAt":"2026-11-13T11:50:00Z","author":{"login":"alice"},"labels":{"nodes":[{"name":"sig/planner"},{"name":"type/bug"}]}},
					{"number":76004,"title":"pending turned enhancement","state":"OPEN","createdAt":"2026-11-10T00:00:00Z","updatedAt":"2026-11-13T11:52:00Z","author":{"login":"bob"},"labels":{"nodes":[{"name":"sig/planner"},{"name":"type/enhancement"}]}}
				]}}}`), nil
			default:
				return []byte(`{"data":{"repository":{
					"i76001":{"number":76001,"title":"tracked bug","body":"desc","state":"OPEN","createdAt":"2026-11-01T00:00:00Z","updatedAt":"2026-11-13T11:50:00Z","url":"https://github.com/pingcap/tidb/issues/76001","author":{"login":"alice"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"sig/planner"},{"name":"type/bug"}]},"comments":{"nodes":[]},"timelineItems":{"totalCount":0,"nodes":[]}},
					"i76002":null,
					"i76003":{"number":76003,"title":"archived execution issue","body":"desc","state":"OPEN","createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-06T00:00:00Z","url":"https://github.com/pingcap/tidb/issues/76003","author":{"login":"carol"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"sig/execution"},{"name":"type/bug"}]},"comments":{"nodes":[]},"timelineItems":{"totalCount":0,"nodes":[]}}
				}},"errors":[{"type":"NOT_FOUND","message":"Could not resolve i76002"}]}`), nil
			}
		},
	}

	state := SyncState{
		PrevMeta: model.SyncMeta{
			SnapshotUTC:        "2026-11-13T11:00:00Z",
			MaxIssueUpdatedUTC: "2026-11-13T10:55:00Z",
		},
		TrackedIssues: map[int]TrackedIssueState{
			76001: {
				Number:    76001,
				UpdatedAt: "2026-11-12T00:00:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
				Cat:       "MEDIUM",
				PRs: []TrackedPRLink{
					{Repo: "pingcap/tidb", Num: 89001, State: "OPEN", Source: model.LinkSourcePRRef},
				},
			},
			76002: {
				Number:    76002,
				UpdatedAt: "2026-11-12T00:00:00Z",
				Labels:    []string{"sig/planner", "type/bug"},
				Cat:       "EASY",
			},
		},
		ArchivedIssues: map[int]ArchivedIssueState{
			76003: {Number: 76003, Cat: "MEDIUM"},
		},
		PendingScopes: map[int][]string{
			76004: {"affinity_opt_title"},
		},
		PendingCandidates: map[int]model.PendingCandidate{
			76004: {
				Number:        76004,
				Title:         "pending turned enhancement",
				UpdatedAt:     "2026-11-12T00:00:00Z",
				Labels:        []string{"sig/planner"},
				MatchedScopes: []string{"affinity_opt_title"},
			},
		},
		RecheckNums: []int{76002, 76003},
	}

	eng := NewEngine(runner)
	plan, err := eng.PlanSync(ctx, state, Options{Now: now})
	if err != nil {
		t.Fatalf("PlanSync: %v", err)
	}

	if len(plan.UnlinkedStreamBPRs[76001]) != 0 {
		t.Fatalf("expected truncated PR #89001 NOT to unlink #76001, got %v", plan.UnlinkedStreamBPRs[76001])
	}
	if len(plan.ClosedRemovals) != 1 || plan.ClosedRemovals[0].Number != 76002 {
		t.Fatalf("expected NOT_FOUND tracked issue #76002 in ClosedRemovals, got %+v", plan.ClosedRemovals)
	}
	found76003 := false
	for _, pc := range plan.UpsertPending {
		if pc.Number == 76003 {
			found76003 = true
		}
	}
	if !found76003 {
		t.Fatalf("expected manually restored archived non-retained #76003 in UpsertPending, got %+v", plan.UpsertPending)
	}
	foundDel76004 := false
	for _, dn := range plan.DeletePendingNums {
		if dn == 76004 {
			foundDel76004 = true
		}
	}
	if !foundDel76004 {
		t.Fatalf("expected pending #76004 relabeled to type/enhancement in DeletePendingNums, got %v", plan.DeletePendingNums)
	}
}
