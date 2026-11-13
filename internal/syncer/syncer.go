package syncer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

type Syncer struct {
	mu     sync.Mutex
	store  *store.Store
	runner ghgql.Runner
}

// New creates a pure-Go Syncer using the default gh GraphQL CLI runner.
func New(st *store.Store) *Syncer {
	return &Syncer{
		store:  st,
		runner: ghgql.DefaultRunner(),
	}
}

func NewWithRunner(st *store.Store, runner ghgql.Runner) *Syncer {
	if runner == nil {
		runner = ghgql.DefaultRunner()
	}
	return &Syncer{
		store:  st,
		runner: runner,
	}
}

func (sy *Syncer) Runner() ghgql.Runner {
	return sy.runner
}

func (sy *Syncer) SyncPRStatuses(ctx context.Context) (prstatus.FetchStats, error) {
	return sy.SyncPRStatusesWithQuota(ctx, 100)
}

func (sy *Syncer) SyncPRStatusesWithQuota(ctx context.Context, minQuota int) (prstatus.FetchStats, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	if minQuota > 0 {
		if _, err := ghgql.CheckQuota(ctx, sy.runner, minQuota); err != nil {
			return prstatus.FetchStats{}, err
		}
	}
	return sy.syncPRStatusesLocked(ctx)
}

func (sy *Syncer) SyncSpecificPRs(ctx context.Context, targets []prstatus.PRTarget, minQuota int) (prstatus.FetchStats, error) {
	// Intentionally do not acquire sy.mu here: targeted PR syncs are fast (single-batch GraphQL)
	// and Store.mu serializes SQLite writes safely, so Web UI triage actions don't block for minutes
	// behind a background SyncIssuesWithOptions run.
	if minQuota > 0 {
		if _, err := ghgql.CheckQuota(ctx, sy.runner, minQuota); err != nil {
			return prstatus.FetchStats{}, err
		}
	}
	return sy.syncSpecificPRs(ctx, targets)
}

func (sy *Syncer) syncSpecificPRs(ctx context.Context, targets []prstatus.PRTarget) (prstatus.FetchStats, error) {
	started := time.Now()
	enriched, err := sy.store.ClassifyTargetedPRSources(targets)
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	statuses, stats, err := prstatus.FetchAndClassifyBatchWithRunner(ctx, sy.runner, enriched, 20)
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	finished := time.Now()
	if err := sy.store.UpsertPRStatuses(statuses, time.Time{}); err != nil {
		return prstatus.FetchStats{}, err
	}
	sy.store.RecordSyncRun("pr_status_targeted", started, finished, stats)
	return stats, nil
}

func (sy *Syncer) syncPRStatusesLocked(ctx context.Context) (prstatus.FetchStats, error) {
	started := time.Now()
	targets, err := sy.store.ListOpenPRTargets()
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	statuses, stats, err := prstatus.FetchAndClassifyBatchWithRunner(ctx, sy.runner, targets, 20)
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	finished := time.Now()
	if err := sy.store.UpsertPRStatuses(statuses, finished); err != nil {
		return prstatus.FetchStats{}, err
	}
	sy.store.RecordSyncRun("pr_status", started, finished, stats)
	return stats, nil
}

type IssueSyncOptions struct {
	Full                  bool
	SkipPRs               bool
	DryRun                bool
	Since                 string
	OverlapMinutes        int
	MaxIncrementalDays    int
	UntriagedLookbackDays int
	MinQuota              int
}

type IssueSyncResult struct {
	ModeUsed        string              `json:"mode_used"`
	DryRun          bool                `json:"dry_run,omitempty"`
	ScriptOutput    string              `json:"script_output"`
	IssueStats      ghgql.CallStats     `json:"issue_stats"`
	PRStats         prstatus.FetchStats `json:"pr_stats"`
	PendingCount    int                 `json:"pending_count"`
	PendingNumbers  []int               `json:"pending_numbers,omitempty"`
	RetriageCount   int                 `json:"retriage_count"`
	RetriageNumbers []int               `json:"retriage_numbers,omitempty"`
	EnrichWarning   string              `json:"enrich_warning,omitempty"`
}

func (sy *Syncer) SyncIssuesDualStream(ctx context.Context, full bool) (IssueSyncResult, error) {
	return sy.SyncIssuesWithOptions(ctx, IssueSyncOptions{
		Full:     full,
		SkipPRs:  false,
		MinQuota: -1,
	})
}

func (sy *Syncer) SyncIssuesWithOptions(ctx context.Context, opts IssueSyncOptions) (IssueSyncResult, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()

	started := time.Now()
	prevDS, err := sy.store.BuildDataset(true)
	if err != nil {
		return IssueSyncResult{}, err
	}
	syncState, err := sy.store.LoadSyncState()
	if err != nil {
		return IssueSyncResult{}, err
	}

	engineOpts := issuesync.Options{
		Full:                  opts.Full,
		Since:                 opts.Since,
		OverlapMinutes:        opts.OverlapMinutes,
		MaxIncrementalDays:    opts.MaxIncrementalDays,
		UntriagedLookbackDays: opts.UntriagedLookbackDays,
	}
	willFull := issuesync.WillPromoteToFull(syncState.PrevMeta, engineOpts)
	minQuota := opts.MinQuota
	if minQuota < 0 {
		if willFull {
			minQuota = 350
		} else {
			minQuota = 150
		}
	} else if minQuota > 0 && willFull && !opts.Full && minQuota < 350 {
		minQuota = 350
	}
	if minQuota > 0 {
		if _, err := ghgql.CheckQuota(ctx, sy.runner, minQuota); err != nil {
			return IssueSyncResult{}, err
		}
	}

	tracker := ghgql.NewCostTracker()
	engineOpts.Tracker = tracker
	engine := issuesync.NewEngine(sy.runner)
	plan, err := engine.PlanSync(ctx, syncState, engineOpts)
	if err != nil {
		return IssueSyncResult{}, err
	}

	var enrichWarn string
	if len(plan.UpsertPending) > 0 {
		enriched, enrichErr := triage.EnrichAndAnalyzePending(ctx, sy.runner, tracker, plan.UpsertPending, true)
		plan.UpsertPending = enriched
		if enrichErr != nil {
			enrichWarn = enrichErr.Error()
		}
	}
	plan.Stats = tracker.Snapshot()

	if opts.DryRun {
		pendNums := make([]int, 0, len(plan.UpsertPending))
		for _, pc := range plan.UpsertPending {
			pendNums = append(pendNums, pc.Number)
		}
		summary := issuesync.FormatSyncSummary(plan, prevDS, prevDS)
		return IssueSyncResult{
			ModeUsed:       plan.ModeUsed,
			DryRun:         true,
			ScriptOutput:   summary,
			IssueStats:     plan.Stats,
			PendingCount:   len(pendNums),
			PendingNumbers: pendNums,
			EnrichWarning:  enrichWarn,
		}, nil
	}

	nextDS, err := sy.store.ApplyIssueSync(plan)
	if err != nil {
		return IssueSyncResult{}, err
	}

	summary := issuesync.FormatSyncSummary(plan, prevDS, nextDS)

	var prStats prstatus.FetchStats
	if !opts.SkipPRs {
		prStats, err = sy.syncPRStatusesLocked(ctx)
		if err != nil {
			return IssueSyncResult{
				ModeUsed:     plan.ModeUsed,
				ScriptOutput: summary,
				IssueStats:   plan.Stats,
			}, err
		}
		nextDS, _ = sy.store.BuildDataset(true)
	}

	pending, _ := sy.store.ListPendingCandidates()
	pendNums := make([]int, 0, len(pending))
	for _, pc := range pending {
		pendNums = append(pendNums, pc.Number)
	}

	res := IssueSyncResult{
		ModeUsed:        plan.ModeUsed,
		ScriptOutput:    summary,
		IssueStats:      plan.Stats,
		PRStats:         prStats,
		PendingCount:    len(pendNums),
		PendingNumbers:  pendNums,
		RetriageCount:   len(nextDS.SyncMeta.RetriageIssueNums),
		RetriageNumbers: nextDS.SyncMeta.RetriageIssueNums,
		EnrichWarning:   enrichWarn,
	}
	sy.store.RecordSyncRun("issues_dual_stream", started, time.Now(), res)
	return res, nil
}

type TriageApplyResult struct {
	store.BatchTriageResult
	SyncedNewPRs int                  `json:"synced_new_prs"`
	PRStats      *prstatus.FetchStats `json:"pr_stats,omitempty"`
}

func collectManualLinkTargets(manualLinks map[string][]string) []prstatus.PRTarget {
	if len(manualLinks) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []prstatus.PRTarget
	for _, specs := range manualLinks {
		for _, sp := range specs {
			targets, err := prstatus.ParsePRSpecs(sp, store.DefaultRepo)
			if err != nil {
				continue
			}
			for _, t := range targets {
				k := model.PRKey(t.Repo, t.Number)
				if !seen[k] {
					seen[k] = true
					t.TrackSources = []string{"issue_linked"}
					out = append(out, t)
				}
			}
		}
	}
	return out
}

func (sy *Syncer) ApplyTriageDecisions(ctx context.Context, dec model.BatchTriageDecisions, syncNewPRs bool, minQuota int) (TriageApplyResult, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()

	started := time.Now()
	var combinedStats prstatus.FetchStats
	hasStats := false
	quotaChecked := false

	// Pre-fetch manual PR specs BEFORE applying decisions so real state/title/is_cherry_pick
	// are available during baseline snapshotting & retriage evaluation (including FILTER_NON_OPT_OR_FIXED).
	if syncNewPRs && len(dec.ManualPRLinks) > 0 {
		if err := sy.store.ValidateBatchDecisionsReadOnly(dec); err != nil {
			return TriageApplyResult{}, err
		}
		manualTargets := collectManualLinkTargets(dec.ManualPRLinks)
		if len(manualTargets) > 0 {
			if minQuota > 0 {
				if _, err := ghgql.CheckQuota(ctx, sy.runner, minQuota); err != nil {
					return TriageApplyResult{}, fmt.Errorf("pre-flight quota check for %d manual PR link(s) failed: %w", len(manualTargets), err)
				}
				quotaChecked = true
			}
			stats, err := sy.syncSpecificPRs(ctx, manualTargets)
			if err != nil {
				return TriageApplyResult{}, fmt.Errorf("pre-fetching %d manual PR link(s) failed: %w", len(manualTargets), err)
			}
			missingPRs, err := sy.store.HasPRStatuses(manualTargets)
			if err != nil {
				return TriageApplyResult{}, err
			}
			if len(missingPRs) > 0 {
				return TriageApplyResult{}, fmt.Errorf("manual_pr_links PR not found on GitHub: %s", strings.Join(missingPRs, ", "))
			}
			combinedStats = stats
			hasStats = true
		}
	}

	bRes, err := sy.store.ApplyBatchTriageDecisions(dec)
	if err != nil {
		return TriageApplyResult{}, err
	}
	out := TriageApplyResult{BatchTriageResult: bRes}
	if syncNewPRs && len(bRes.MissingPRTargets) > 0 {
		if minQuota > 0 && !quotaChecked {
			if _, err := ghgql.CheckQuota(ctx, sy.runner, minQuota); err != nil {
				return out, fmt.Errorf("triage decisions saved, but pre-flight quota check for %d newly linked PR(s) failed: %w", len(bRes.MissingPRTargets), err)
			}
		}
		stats, err := sy.syncSpecificPRs(ctx, bRes.MissingPRTargets)
		if err != nil {
			return out, fmt.Errorf("triage decisions saved, but syncing %d newly linked PR(s) failed: %w", len(bRes.MissingPRTargets), err)
		}
		if len(bRes.AcceptedIssues) > 0 {
			_ = sy.store.RefreshAcceptedIssueBaselines(bRes.AcceptedIssues)
			if cnt, cntErr := sy.store.CountRetriageNeeded(); cntErr == nil {
				out.RemainingRetriage = cnt
			}
		}
		combinedStats.GraphQLCalls += stats.GraphQLCalls
		combinedStats.GraphQLCost += stats.GraphQLCost
		combinedStats.TotalFetched += stats.TotalFetched
		combinedStats.OpenCount += stats.OpenCount
		combinedStats.ClosedCount += stats.ClosedCount
		combinedStats.DurationMs += stats.DurationMs
		if stats.RateLimitLimit > 0 {
			combinedStats.RateLimitLimit = stats.RateLimitLimit
			combinedStats.RateLimitRemaining = stats.RateLimitRemaining
			combinedStats.RateLimitResetUTC8 = stats.RateLimitResetUTC8
		}
		hasStats = true
	}
	if hasStats {
		out.SyncedNewPRs = combinedStats.TotalFetched
		out.PRStats = &combinedStats
	}
	sy.store.RecordSyncRun("triage_apply", started, time.Now(), out)
	return out, nil
}
