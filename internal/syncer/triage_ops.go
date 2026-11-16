package syncer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

var ErrBatchInputInvalid = errors.New("invalid batch triage decisions")

type PendingBundleOptions struct {
	OnlyNum       int
	Numbers       []int
	ScopeFilter   string
	OnlyPending   bool
	OnlyRetriage  bool
	ReTriageNums  []int
	BackfillNotes int
	CatFilter     string
	Compact       bool
	EnrichPRs     bool
}

type BatchTriageExecuteRequest struct {
	Decisions      model.BatchTriageDecisions `json:"decisions"`
	SkipTodo       bool                       `json:"skip_todo,omitempty"`
	AllowEmptyNote bool                       `json:"allow_empty_note,omitempty"`
	StrictNotes    bool                       `json:"strict_notes,omitempty"`
	SkipConflicts  bool                       `json:"skip_conflicts,omitempty"`
	DryRun         bool                       `json:"dry_run,omitempty"`
	SyncNewPRs     *bool                      `json:"sync_new_prs,omitempty"`
	MinQuota       *int                       `json:"min_quota,omitempty"`
}

func (sy *Syncer) BuildStatusReport(ctx context.Context, dbDisplay string, checkGH bool) (map[string]any, error) {
	return sy.BuildStatusReportWithOptions(ctx, dbDisplay, checkGH, true)
}

func (sy *Syncer) BuildStatusReportWithOptions(ctx context.Context, dbDisplay string, checkGH bool, includeWorkbench bool) (map[string]any, error) {
	meta, err := sy.store.LoadStatusSummary(includeWorkbench)
	if err != nil {
		return nil, fmt.Errorf("load status summary: %w", err)
	}

	dbMap := map[string]any{
		"db_path":                         dbDisplay,
		"total_candidates":                meta.TotalCandidates,
		"canonical_candidates":            meta.CanonicalCandidates,
		"real_bugs":                       meta.RealBugs,
		"open_pr_count":                   meta.OpenPRCount,
		"mainline_open_pr_count":          meta.MainlineOpenPRCount,
		"issue_snapshot_utc8":             meta.SnapshotUTC8,
		"issue_watermark_utc8":            meta.MaxIssueUpdatedUTC8,
		"pr_snapshot_utc8":                meta.PRSnapshotUTC8,
		"incremental_gh_search_qualifier": meta.IncrementalGHSearchQualifier,
	}

	runs := []store.SyncRunRecord{}
	if includeWorkbench {
		pendNums := meta.PendingCandidateNums
		if pendNums == nil {
			pendNums = []int{}
		}
		retriageNums := meta.RetriageIssueNums
		if retriageNums == nil {
			retriageNums = []int{}
		}
		dbMap["pending_count"] = len(pendNums)
		dbMap["pending_numbers"] = pendNums
		dbMap["retriage_count"] = len(retriageNums)
		dbMap["retriage_numbers"] = retriageNums
		dbMap["excluded_count"] = len(meta.ExcludedIssueNums)
		if r, rErr := sy.store.RecentSyncRuns(5); rErr == nil && r != nil {
			runs = r
		}
	} else {
		dbMap["db_path"] = "redacted"
	}

	out := map[string]any{
		"database":         dbMap,
		"recent_sync_runs": runs,
	}

	if checkGH {
		ghCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		rl, err := ghgql.CheckQuota(ghCtx, sy.Runner(), 0)
		if err != nil {
			out["gh_check_error"] = err.Error()
		} else {
			out["gh_rate_limit"] = rl
		}
	}
	return out, nil
}

func (sy *Syncer) BuildPendingBundle(ctx context.Context, opts PendingBundleOptions) (map[string]any, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// Mode A: Backfill notes for existing issues lacking root-cause notes
	if opts.BackfillNotes > 0 {
		recs, err := sy.store.ListBackfillNoteCandidates(opts.BackfillNotes, opts.CatFilter)
		if err != nil {
			return nil, 0, fmt.Errorf("list backfill candidates: %w", err)
		}
		var prDetailMap map[string]model.PendingPRDetail
		var bfEnrichErr error
		if opts.EnrichPRs && len(recs) <= 30 {
			var allPRs []model.LinkedPRRef
			for _, r := range recs {
				allPRs = append(allPRs, triage.FilterRelevantKernelPRs(r.PRs)...)
			}
			if len(allPRs) > 0 {
				prDetailMap, bfEnrichErr = triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, allPRs)
				if err := ctx.Err(); err != nil {
					return nil, 0, err
				}
				if len(prDetailMap) > 0 {
					_ = sy.store.SyncFetchedPRDetails(prDetailMap)
					if refreshed, rErr := sy.store.ListBackfillNoteCandidates(opts.BackfillNotes, opts.CatFilter); rErr == nil {
						recs = refreshed
					}
				}
			}
		}
		exportedAt := model.NowRFC3339Nano()

		type backfillItem struct {
			Number      int                     `json:"number"`
			URL         string                  `json:"url"`
			Title       string                  `json:"title"`
			IsCanonical bool                    `json:"is_canonical"`
			IsCustomer  bool                    `json:"is_customer"`
			Cat         string                  `json:"cat"`
			Severity    string                  `json:"severity"`
			Subsystem   string                  `json:"subsystem"`
			Labels      []string                `json:"labels"`
			PRs         []model.LinkedPRRef     `json:"prs"`
			PRDetails   []model.PendingPRDetail `json:"pr_details,omitempty"`
		}
		items := make([]backfillItem, 0, len(recs))
		tmplNotes := make(map[string]model.NotePatchRequest, len(recs))
		for _, r := range recs {
			syncedPRs := r.PRs
			var dets []model.PendingPRDetail
			if len(prDetailMap) > 0 {
				syncedPRs, dets = triage.SyncPRRefsWithDetails(r.PRs, prDetailMap)
			}
			items = append(items, backfillItem{
				Number:      r.Number,
				URL:         fmt.Sprintf("https://github.com/pingcap/tidb/issues/%d", r.Number),
				Title:       r.Title,
				IsCanonical: r.IsCanonical,
				IsCustomer:  r.IsCustomer,
				Cat:         r.Cat,
				Severity:    r.Severity,
				Subsystem:   r.Subsystem,
				Labels:      r.Labels,
				PRs:         syncedPRs,
				PRDetails:   dets,
			})
			tmplNotes[strconv.Itoa(r.Number)] = model.NotePatchRequest{
				ZhCN:              "TODO: 填写中文根因与影响简述",
				En:                "TODO: Fill in concise English root-cause & impact note",
				IfUnmodifiedSince: r.TriagedAt,
			}
		}

		bundle := map[string]any{
			"mode":            "backfill_notes",
			"exported_at_utc": exportedAt,
			"backfill_count":  len(items),
			"instructions":    "Inspect each issue & linked PR diff (`gh issue view <num> --repo pingcap/tidb --comments`, `gh pr diff <pr> --repo pingcap/tidb`), fill in `notes` (preserving `base_exported_at` and `if_unmodified_since`), and apply via `triage-hub triage --stdin --skip-todo`.",
			"triage_rubric":   triage.DefaultRubric(opts.Compact),
			"backfill_issues": items,
			"decisions_template": model.BatchTriageDecisions{
				BaseExportedAt: exportedAt,
				Notes:          tmplNotes,
			},
		}
		if bfEnrichErr != nil {
			bundle["enrichment_warning"] = fmt.Sprintf("PR file enrichment degraded: %v", bfEnrichErr)
		}
		return bundle, len(items), nil
	}

	wantPendingNums := make(map[int]bool)
	if opts.OnlyNum > 0 {
		wantPendingNums[opts.OnlyNum] = true
	}
	for _, n := range opts.Numbers {
		if n > 0 {
			wantPendingNums[n] = true
		}
	}

	specificRetriageNums := opts.ReTriageNums
	skipPending := opts.OnlyRetriage || (len(specificRetriageNums) > 0 && len(wantPendingNums) == 0)
	skipRetriage := opts.OnlyPending || (len(wantPendingNums) > 0 && len(specificRetriageNums) == 0)

	filterPendingSlice := func(list []model.PendingCandidate) []model.PendingCandidate {
		if len(wantPendingNums) == 0 && strings.TrimSpace(opts.ScopeFilter) == "" {
			return list
		}
		sf := strings.ToLower(strings.TrimSpace(opts.ScopeFilter))
		filtered := make([]model.PendingCandidate, 0, len(list))
		for _, pc := range list {
			if len(wantPendingNums) > 0 && !wantPendingNums[pc.Number] {
				continue
			}
			if sf != "" {
				matched := false
				for _, sc := range pc.MatchedScopes {
					if strings.Contains(strings.ToLower(sc), sf) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			filtered = append(filtered, pc)
		}
		return filtered
	}

	var pending []model.PendingCandidate
	if !skipPending {
		var err error
		pending, err = sy.store.ListPendingCandidates()
		if err != nil {
			return nil, 0, fmt.Errorf("list pending candidates: %w", err)
		}
		pending = filterPendingSlice(pending)
	}

	var retriageList []model.RetriageCandidate
	loadRetriage := func() ([]model.RetriageCandidate, error) {
		if len(specificRetriageNums) > 0 {
			return sy.store.ListRetriageCandidates(false, specificRetriageNums)
		}
		return sy.store.ListRetriageCandidates(true, nil)
	}
	if !skipRetriage {
		var err error
		retriageList, err = loadRetriage()
		if err != nil {
			return nil, 0, fmt.Errorf("list retriage candidates: %w", err)
		}
	}

	var enrichWarnings []string
	dirtyIdx := make([]int, 0)
	for i := range pending {
		if pending[i].SuggestedTriage == nil || (opts.EnrichPRs && triage.CandidateNeedsPREnrichment(pending[i])) {
			dirtyIdx = append(dirtyIdx, i)
		}
	}
	if len(dirtyIdx) > 0 {
		toEnrich := make([]model.PendingCandidate, len(dirtyIdx))
		for k, idx := range dirtyIdx {
			toEnrich[k] = pending[idx]
		}
		enriched, enrichErr := triage.EnrichAndAnalyzePending(ctx, sy.Runner(), nil, toEnrich, opts.EnrichPRs)
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if enrichErr != nil {
			enrichWarnings = append(enrichWarnings, enrichErr.Error())
		}
		var toPersist []model.PendingCandidate
		for k, idx := range dirtyIdx {
			prevSug := pending[idx].SuggestedTriage
			wasUnanalyzed := prevSug == nil
			if enrichErr != nil && !wasUnanalyzed && triage.CandidateNeedsPREnrichment(enriched[k]) {
				enriched[k].SuggestedTriage = prevSug
			}
			pending[idx] = enriched[k]
			if enrichErr == nil || (!opts.EnrichPRs && wasUnanalyzed) || !triage.CandidateNeedsPREnrichment(enriched[k]) {
				toPersist = append(toPersist, enriched[k])
			}
		}
		if len(toPersist) > 0 {
			if persisted, pErr := sy.store.PersistEnrichedPendingCandidates(toPersist); pErr == nil {
				persistedMap := make(map[int]model.PendingCandidate, len(persisted))
				for _, pc := range persisted {
					persistedMap[pc.Number] = pc
				}
				triedPersist := make(map[int]bool, len(toPersist))
				for _, pc := range toPersist {
					triedPersist[pc.Number] = true
				}
				nextPending := make([]model.PendingCandidate, 0, len(pending))
				for _, pc := range pending {
					if triedPersist[pc.Number] {
						if livePC, ok := persistedMap[pc.Number]; ok {
							nextPending = append(nextPending, livePC)
						}
						continue
					}
					nextPending = append(nextPending, pc)
				}
				pending = nextPending
			}
		} else if liveNums, nErr := sy.store.ListPendingCandidateNumbers(); nErr == nil {
			liveSet := make(map[int]bool, len(liveNums))
			for _, n := range liveNums {
				liveSet[n] = true
			}
			nextPending := make([]model.PendingCandidate, 0, len(pending))
			for _, pc := range pending {
				if liveSet[pc.Number] {
					nextPending = append(nextPending, pc)
				}
			}
			pending = nextPending
		}
	}

	if len(retriageList) > 0 {
		var allPRs []model.LinkedPRRef
		if opts.EnrichPRs {
			for _, rc := range retriageList {
				allPRs = append(allPRs, triage.FilterRelevantKernelPRs(rc.PRs)...)
			}
		}
		var detMap map[string]model.PendingPRDetail
		if len(allPRs) > 0 {
			var rtEnrichErr error
			detMap, rtEnrichErr = triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, allPRs)
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			if rtEnrichErr != nil {
				enrichWarnings = append(enrichWarnings, rtEnrichErr.Error())
			}
			if len(detMap) > 0 {
				_ = sy.store.SyncFetchedPRDetails(detMap)
				if refreshed, rErr := loadRetriage(); rErr == nil {
					retriageList = refreshed
				}
			}
		}
		for i := range retriageList {
			if len(detMap) > 0 {
				retriageList[i].PRs, retriageList[i].PRDetails = triage.SyncPRRefsWithDetails(retriageList[i].PRs, detMap)
			}
			retriageList[i].Clues = triage.ExtractClues(
				retriageList[i].Title, retriageList[i].Body, retriageList[i].Labels,
				retriageList[i].RecentComments, retriageList[i].PRDetails,
			)
			retriageList[i].SuggestedTriage = triage.SuggestTriage(
				retriageList[i].Title, retriageList[i].Labels, retriageList[i].PRs,
				retriageList[i].PRDetails, retriageList[i].Clues,
			)
		}
	}

	exportedAt := model.NowRFC3339Nano()
	if pending == nil {
		pending = []model.PendingCandidate{}
	}
	if retriageList == nil {
		retriageList = []model.RetriageCandidate{}
	}

	tmplNew := make(map[string]model.TriagePatchRequest, len(pending))
	suggestedExcludesHint := make(map[string]string)
	for _, pc := range pending {
		cat := "TODO: REVIEW"
		sev := "TODO: REVIEW"
		subsys := "TODO: REVIEW"
		borderline := false
		if pc.SuggestedTriage != nil {
			borderline = pc.SuggestedTriage.IsBorderlineKept
			if pc.SuggestedTriage.RecommendedAction == "exclude" && pc.SuggestedTriage.ExcludeReason != "" {
				suggestedExcludesHint[strconv.Itoa(pc.Number)] = pc.SuggestedTriage.ExcludeReason
			}
			if pc.SuggestedTriage.Confidence != "low" && pc.SuggestedTriage.RecommendedAction != "review_carefully" {
				cat = pc.SuggestedTriage.Cat
				sev = pc.SuggestedTriage.Severity
				subsys = pc.SuggestedTriage.Subsystem
			} else if pc.SuggestedTriage.Subsystem != "" && (len(pc.SuggestedTriage.Signals) == 0 || pc.SuggestedTriage.Signals[0] != "default:logical_rewrite") {
				subsys = pc.SuggestedTriage.Subsystem
			}
		}
		tmplNew[strconv.Itoa(pc.Number)] = model.TriagePatchRequest{
			Cat:               cat,
			Severity:          sev,
			Subsystem:         subsys,
			IsBorderlineKept:  borderline,
			IfUnmodifiedSince: pc.SyncWatermark,
			Note: &model.LocalizedNote{
				ZhCN: "TODO: 填写中文根因与影响简述",
				En:   "TODO: Fill in concise English root-cause & impact note",
			},
		}
	}

	tmplOvr := make(map[string]model.TriagePatchRequest)

	bundle := map[string]any{
		"exported_at_utc": exportedAt,
		"pending_count":   len(pending),
		"retriage_count":  len(retriageList),
		"instructions":    "Independently judge `pending_candidates` (SQL realism/reachability, PR diff, difficulty, and severity — do NOT blindly accept `decisions_template` pre-fills) and `retriage_candidates` against `triage_rubric` (including `retriage_guide`). Preserve `base_exported_at` and `if_unmodified_since` so concurrent edits/syncs are safely detected. For each `retriage_candidates` item, explicitly add it to ONE of `overrides` (classification recalibration; copy `triaged_at` to `if_unmodified_since`), `ack_retriage` (classification unchanged, advance baseline), `notes` (note-only update without resetting baseline), or `exclude_issues` (supplemental non-optimizer only). Submit via `triage-hub triage --stdin` (`--skip-todo`, `--dry-run`). Canonical (`sig/planner` + `type/bug`) issues must NEVER be put in `exclude_issues`.",
		"valid_enums": map[string]any{
			"cat":       model.ValidCategories,
			"severity":  model.ValidSeverities,
			"subsystem": model.ValidSubsystems,
		},
		"triage_rubric": triage.DefaultRubric(opts.Compact),
		"decisions_template": model.BatchTriageDecisions{
			BaseExportedAt: exportedAt,
			NewIssues:      tmplNew,
			ExcludeIssues:  map[string]string{},
			Overrides:      tmplOvr,
			AckRetriage:    model.FlexIntList{},
		},
		"pending_candidates":  pending,
		"retriage_candidates": retriageList,
	}
	if len(suggestedExcludesHint) > 0 {
		bundle["suggested_excludes_hint"] = suggestedExcludesHint
	}
	if len(enrichWarnings) > 0 {
		bundle["enrichment_warning"] = fmt.Sprintf("PR file enrichment degraded: %s", strings.Join(enrichWarnings, "; "))
	}

	return bundle, len(pending) + len(retriageList), nil
}

// HasActionableDecisions reports whether a prepared BatchTriageDecisions contains at least one mutation.
func HasActionableDecisions(dec model.BatchTriageDecisions) bool {
	return len(dec.NewIssues) > 0 ||
		len(dec.ExcludeIssues) > 0 ||
		len(dec.Overrides) > 0 ||
		len(dec.Notes) > 0 ||
		len(dec.AckRetriage) > 0 ||
		len(dec.ManualPRLinks) > 0
}

// FormatDryRunBatchResult formats the canonical dry-run output map from post-conflict-filtered `planned` decisions.
func FormatDryRunBatchResult(
	planned model.BatchTriageDecisions,
	dbValidated bool,
	warnings []triage.ValidationWarning,
	conflicts []model.TriageConflict,
) map[string]any {
	out := map[string]any{
		"dry_run":             true,
		"db_validated":        dbValidated,
		"new_issues_count":    len(planned.NewIssues),
		"exclude_count":       len(planned.ExcludeIssues),
		"overrides_count":     len(planned.Overrides),
		"notes_count":         len(planned.Notes),
		"ack_retriage_count":  len(planned.AckRetriage),
		"manual_pr_links":     len(planned.ManualPRLinks),
		"validation_warnings": warnings,
		"prepared_decisions":  planned,
	}
	if len(conflicts) > 0 {
		out["skipped_conflicts"] = conflicts
	}
	return out
}

// ExecuteBatchTriage validates, optionally dry-runs, or applies batch triage decisions.
func (sy *Syncer) ExecuteBatchTriage(ctx context.Context, req BatchTriageExecuteRequest) (any, error) {
	cleaned, warnings, err := triage.ValidateAndPrepareDecisions(req.Decisions, triage.ValidateOptions{
		SkipTodo:       req.SkipTodo,
		AllowEmptyNote: req.AllowEmptyNote,
		StrictNotes:    req.StrictNotes,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBatchInputInvalid, err)
	}

	if !HasActionableDecisions(cleaned) {
		return nil, fmt.Errorf("%w: no actionable entries remaining after validation (check if all entries were skipped by --skip-todo)", ErrBatchInputInvalid)
	}

	applyOpts := store.BatchApplyOptions{SkipConflicts: req.SkipConflicts}
	if req.DryRun {
		planned, conflicts, valErr := sy.store.PlanBatchDecisionsWithOptions(cleaned, applyOpts)
		if valErr != nil {
			if errors.Is(valErr, store.ErrBatchConflict) || errors.Is(valErr, store.ErrCanonicalCannotExclude) {
				return nil, valErr
			}
			return nil, fmt.Errorf("%w: %w", ErrBatchInputInvalid, valErr)
		}
		return FormatDryRunBatchResult(planned, true, warnings, conflicts), nil
	}

	syncNewPRs := true
	if req.SyncNewPRs != nil {
		syncNewPRs = *req.SyncNewPRs
	}
	minQuota := 20
	if req.MinQuota != nil {
		minQuota = *req.MinQuota
	}

	res, err := sy.ApplyTriageDecisionsWithOptions(ctx, cleaned, applyOpts, syncNewPRs, minQuota)
	if err != nil {
		return res, err
	}
	res.ValidationWarnings = warnings
	return res, nil
}
