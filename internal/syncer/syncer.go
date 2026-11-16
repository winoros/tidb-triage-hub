package syncer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

var (
	ErrInvalidSyncInput = errors.New("invalid sync input")
	ErrMissingGitHubPR  = errors.New("manual_pr_links PR not found on GitHub")
)

type Syncer struct {
	mu                 sync.Mutex
	store              *store.Store
	runner             ghgql.Runner
	lastFullPRSyncUnix atomic.Int64
}

// New creates a pure-Go Syncer using the default GitHub GraphQL runner.
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
	if err := ctx.Err(); err != nil {
		return prstatus.FetchStats{}, err
	}
	if minQuota > 0 {
		if _, err := ghgql.CheckQuota(ctx, sy.runner, minQuota); err != nil {
			return prstatus.FetchStats{}, err
		}
	}
	return sy.syncPRStatusesLocked(ctx)
}

// SyncPRStatusesIfStale runs a full open-PR refresh only if at least minAge has elapsed
// since the last completed full PR status refresh (including one run inside SyncIssuesWithOptions).
func (sy *Syncer) SyncPRStatusesIfStale(ctx context.Context, minAge time.Duration) (prstatus.FetchStats, bool, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return prstatus.FetchStats{}, false, err
	}
	if minAge > 0 {
		if last := sy.lastFullPRSyncUnix.Load(); last > 0 && time.Since(time.Unix(0, last)) < minAge {
			return prstatus.FetchStats{}, false, nil
		}
	}
	if _, err := ghgql.CheckQuota(ctx, sy.runner, 100); err != nil {
		return prstatus.FetchStats{}, false, err
	}
	stats, err := sy.syncPRStatusesLocked(ctx)
	if err != nil {
		return prstatus.FetchStats{}, false, err
	}
	return stats, true, nil
}

// ExecuteSyncPRs runs either a targeted PR refresh (when prSpec is non-empty) or a full open-PR refresh,
// applying standard default quota thresholds when minQuota < 0.
func (sy *Syncer) ExecuteSyncPRs(ctx context.Context, prSpec string, minQuota int) (prstatus.FetchStats, error) {
	if strings.TrimSpace(prSpec) != "" {
		targets, err := prstatus.ParsePRSpecs(prSpec, store.DefaultRepo)
		if err != nil {
			return prstatus.FetchStats{}, fmt.Errorf("%w: invalid pr spec: %v", ErrInvalidSyncInput, err)
		}
		for i := range targets {
			targets[i].TrackSources = []string{"manual_cli"}
		}
		mq := minQuota
		if mq < 0 {
			mq = 20
		}
		stats, err := sy.SyncSpecificPRs(ctx, targets, mq)
		if err != nil {
			return prstatus.FetchStats{}, err
		}
		missingPRs, err := sy.store.HasPRStatuses(targets)
		if err != nil {
			return prstatus.FetchStats{}, err
		}
		if len(missingPRs) > 0 {
			return prstatus.FetchStats{}, fmt.Errorf("%w: %s", ErrMissingGitHubPR, strings.Join(missingPRs, ", "))
		}
		return stats, nil
	}
	mq := minQuota
	if mq < 0 {
		mq = 100
	}
	return sy.SyncPRStatusesWithQuota(ctx, mq)
}

func (sy *Syncer) SyncSpecificPRs(ctx context.Context, targets []prstatus.PRTarget, minQuota int) (prstatus.FetchStats, error) {
	// Intentionally do not acquire sy.mu here: targeted PR syncs are fast (single-batch GraphQL)
	// and Store.mu serializes SQLite writes safely, so Web UI triage actions don't block for minutes
	// behind a background SyncIssuesWithOptions run.
	if err := ctx.Err(); err != nil {
		return prstatus.FetchStats{}, err
	}
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
	sy.lastFullPRSyncUnix.Store(finished.UnixNano())
	sy.store.RecordSyncRun("pr_status", started, finished, stats)
	return stats, nil
}

type IssueSyncOptions struct {
	Full                  bool   `json:"full"`
	SkipPRs               bool   `json:"skip_prs"`
	DryRun                bool   `json:"dry_run"`
	Since                 string `json:"since,omitempty"`
	OverlapMinutes        int    `json:"overlap_minutes"`
	MaxIncrementalDays    int    `json:"max_incremental_days"`
	UntriagedLookbackDays int    `json:"untriaged_lookback_days"`
	MinQuota              int    `json:"min_quota"`
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

func buildPrevSummaryDataset(syncState issuesync.SyncState, retriageNums []int) model.Dataset {
	meta := syncState.PrevMeta
	canCnt := 0
	realBugs := 0
	for _, tr := range syncState.TrackedIssues {
		if tr.IsCanonical {
			canCnt++
		}
		if model.IsRealBugCategory(tr.Cat) {
			realBugs++
		}
	}
	meta.TotalCandidates = len(syncState.TrackedIssues)
	meta.CanonicalCandidates = canCnt
	meta.RealBugs = realBugs
	meta.RetriageIssueNums = retriageNums
	return model.Dataset{SyncMeta: meta}
}

func (sy *Syncer) SyncIssuesWithOptions(ctx context.Context, opts IssueSyncOptions) (IssueSyncResult, error) {
	if s := strings.TrimSpace(opts.Since); s != "" {
		if _, ok := model.NormalizeRFC3339NanoFixed(s); !ok {
			return IssueSyncResult{}, fmt.Errorf("%w: invalid since timestamp %q (expected RFC3339)", ErrInvalidSyncInput, s)
		}
	}

	sy.mu.Lock()
	defer sy.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return IssueSyncResult{}, err
	}

	started := time.Now()
	planStartedUTC := model.NowRFC3339Nano()
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
	plan.PlanStartedUTC = planStartedUTC

	var enrichWarn string
	if len(plan.UpsertPending) > 0 {
		enriched, enrichErr := triage.EnrichAndAnalyzePending(ctx, sy.runner, tracker, plan.UpsertPending, true)
		if err := ctx.Err(); err != nil {
			return IssueSyncResult{}, err
		}
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
		curRetriage, _ := sy.store.ListRetriageNumbers()
		prevSummaryDS := buildPrevSummaryDataset(syncState, curRetriage)
		summary := issuesync.FormatSyncSummary(plan, prevSummaryDS, prevSummaryDS)
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

	prevSummaryDS := buildPrevSummaryDataset(syncState, nil)
	nextMeta, applied, err := sy.store.ApplyIssueSyncSummary(plan)
	if err != nil {
		return IssueSyncResult{}, err
	}
	plan.Applied = &applied

	summary := issuesync.FormatSyncSummary(plan, prevSummaryDS, model.Dataset{SyncMeta: nextMeta})
	pendNums := nextMeta.PendingCandidateNums
	retriageNums := nextMeta.RetriageIssueNums

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
		if pNums, pErr := sy.store.ListPendingCandidateNumbers(); pErr == nil {
			pendNums = pNums
		}
		if nums, rErr := sy.store.ListRetriageNumbers(); rErr == nil {
			retriageNums = nums
		}
	}

	res := IssueSyncResult{
		ModeUsed:        plan.ModeUsed,
		ScriptOutput:    summary,
		IssueStats:      plan.Stats,
		PRStats:         prStats,
		PendingCount:    len(pendNums),
		PendingNumbers:  pendNums,
		RetriageCount:   len(retriageNums),
		RetriageNumbers: retriageNums,
		EnrichWarning:   enrichWarn,
	}
	sy.store.RecordSyncRun("issues_dual_stream", started, time.Now(), res)
	return res, nil
}

type TriageApplyResult struct {
	store.BatchTriageResult
	SyncedNewPRs       int                        `json:"synced_new_prs"`
	PRStats            *prstatus.FetchStats       `json:"pr_stats,omitempty"`
	PRSyncWarning      string                     `json:"pr_sync_warning,omitempty"`
	ValidationWarnings []triage.ValidationWarning `json:"validation_warnings,omitempty"`
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

func mergePRFetchStats(dst *prstatus.FetchStats, src prstatus.FetchStats) {
	dst.GraphQLCalls += src.GraphQLCalls
	dst.GraphQLCost += src.GraphQLCost
	dst.TotalFetched += src.TotalFetched
	dst.OpenCount += src.OpenCount
	dst.ClosedCount += src.ClosedCount
	dst.DurationMs += src.DurationMs
	if src.RateLimitLimit > 0 {
		dst.RateLimitLimit = src.RateLimitLimit
		dst.RateLimitRemaining = src.RateLimitRemaining
		dst.RateLimitResetUTC8 = src.RateLimitResetUTC8
	}
}

func collectDecisionIssueNums[V any](m map[string]V) []int {
	if len(m) == 0 {
		return nil
	}
	var nums []int
	seen := make(map[int]bool, len(m))
	for k := range m {
		s := strings.TrimPrefix(strings.TrimSpace(k), "#")
		if n, err := strconv.Atoi(s); err == nil && n > 0 && !seen[n] {
			seen[n] = true
			nums = append(nums, n)
		}
	}
	return nums
}

func (sy *Syncer) ApplyTriageDecisions(ctx context.Context, dec model.BatchTriageDecisions, syncNewPRs bool, minQuota int) (TriageApplyResult, error) {
	return sy.ApplyTriageDecisionsWithOptions(ctx, dec, store.BatchApplyOptions{SkipConflicts: false}, syncNewPRs, minQuota)
}

func (sy *Syncer) ApplyTriageDecisionsWithOptions(ctx context.Context, dec model.BatchTriageDecisions, applyOpts store.BatchApplyOptions, syncNewPRs bool, minQuota int) (TriageApplyResult, error) {
	// Intentionally do not acquire sy.mu here: Store.mu serializes SQLite batch writes safely in <5ms,
	// and ApplyIssueSync re-reads live tables inside its write transaction, so batch triage calls
	// (including remote Machine B `triage`) never stall behind multi-minute background syncs.
	if err := ctx.Err(); err != nil {
		return TriageApplyResult{}, err
	}

	started := time.Now()
	var combinedStats prstatus.FetchStats
	hasStats := false
	quotaChecked := false
	prSyncWarn := ""

	// Pre-fetch uncached PRs BEFORE mutating SQLite so baseline snapshotting sees true GitHub PR states
	// immediately on the first transaction, avoiding any post-commit transient window.
	if syncNewPRs && (len(dec.ManualPRLinks) > 0 || len(dec.NewIssues) > 0 || len(dec.Overrides) > 0) {
		planned, _, err := sy.store.PlanBatchDecisionsWithOptions(dec, applyOpts)
		if err != nil {
			return TriageApplyResult{}, err
		}
		syncedKeys := make(map[string]bool)
		manualTargets := collectManualLinkTargets(planned.ManualPRLinks)
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
				return TriageApplyResult{}, fmt.Errorf("%w: %s", ErrMissingGitHubPR, strings.Join(missingPRs, ", "))
			}
			for _, t := range manualTargets {
				syncedKeys[model.PRKey(t.Repo, t.Number)] = true
			}
			mergePRFetchStats(&combinedStats, stats)
			hasStats = true
		}

		var preTargets []prstatus.PRTarget
		addPreTargets := func(list []prstatus.PRTarget) {
			for _, t := range list {
				k := model.PRKey(t.Repo, t.Number)
				if !syncedKeys[k] {
					syncedKeys[k] = true
					preTargets = append(preTargets, t)
				}
			}
		}
		if newNums := collectDecisionIssueNums(planned.NewIssues); len(newNums) > 0 {
			if list, err := sy.store.ListPendingMissingPRTargets(newNums...); err == nil {
				addPreTargets(list)
			}
		}
		if ovrNums := collectDecisionIssueNums(planned.Overrides); len(ovrNums) > 0 {
			if list, err := sy.store.ListIssueMissingPRTargets(ovrNums...); err == nil {
				addPreTargets(list)
			}
		}
		if len(preTargets) > 0 {
			if minQuota > 0 && !quotaChecked {
				if _, qErr := ghgql.CheckQuota(ctx, sy.runner, minQuota); qErr != nil {
					if err := ctx.Err(); err != nil {
						return TriageApplyResult{}, err
					}
					prSyncWarn = fmt.Sprintf("triage decisions saved, but pre-flight quota check for %d newly linked PR(s) failed: %v", len(preTargets), qErr)
				} else {
					quotaChecked = true
				}
			}
			if prSyncWarn == "" {
				stats, sErr := sy.syncSpecificPRs(ctx, preTargets)
				if sErr != nil {
					if err := ctx.Err(); err != nil {
						return TriageApplyResult{}, err
					}
					prSyncWarn = fmt.Sprintf("triage decisions saved, but syncing %d newly linked PR(s) failed: %v", len(preTargets), sErr)
				} else {
					mergePRFetchStats(&combinedStats, stats)
					hasStats = true
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return TriageApplyResult{}, err
	}
	bRes, err := sy.store.ApplyBatchTriageDecisionsWithOptions(dec, applyOpts)
	if err != nil {
		return TriageApplyResult{}, err
	}
	out := TriageApplyResult{
		BatchTriageResult: bRes,
		PRSyncWarning:     prSyncWarn,
	}
	if syncNewPRs && out.PRSyncWarning == "" && len(bRes.MissingPRTargets) > 0 {
		if minQuota > 0 && !quotaChecked {
			if _, qErr := ghgql.CheckQuota(ctx, sy.runner, minQuota); qErr != nil {
				out.PRSyncWarning = fmt.Sprintf("triage decisions saved, but pre-flight quota check for %d newly linked PR(s) failed: %v", len(bRes.MissingPRTargets), qErr)
			}
		}
		if out.PRSyncWarning == "" {
			stats, sErr := sy.syncSpecificPRs(ctx, bRes.MissingPRTargets)
			if sErr != nil {
				out.PRSyncWarning = fmt.Sprintf("triage decisions saved, but syncing %d newly linked PR(s) failed: %v", len(bRes.MissingPRTargets), sErr)
			} else {
				if cnt, cntErr := sy.store.CountRetriageNeeded(); cntErr == nil {
					out.RemainingRetriage = cnt
				}
				mergePRFetchStats(&combinedStats, stats)
				hasStats = true
			}
		}
	}
	if hasStats {
		out.SyncedNewPRs = combinedStats.TotalFetched
		out.PRStats = &combinedStats
	}
	sy.store.RecordSyncRun("triage_apply", started, time.Now(), out)
	return out, nil
}
