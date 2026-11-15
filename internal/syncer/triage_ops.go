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
	ds, err := sy.store.BuildDataset(true)
	if err != nil {
		return nil, fmt.Errorf("build dataset: %w", err)
	}
	pendNums := ds.SyncMeta.PendingCandidateNums
	if pendNums == nil {
		pendNums = []int{}
	}
	retriageNums := ds.SyncMeta.RetriageIssueNums
	if retriageNums == nil {
		retriageNums = []int{}
	}
	runs, _ := sy.store.RecentSyncRuns(5)
	if runs == nil {
		runs = []store.SyncRunRecord{}
	}

	out := map[string]any{
		"database": map[string]any{
			"db_path":                         dbDisplay,
			"total_candidates":                ds.SyncMeta.TotalCandidates,
			"canonical_candidates":            ds.SyncMeta.CanonicalCandidates,
			"real_bugs":                       ds.SyncMeta.RealBugs,
			"open_pr_count":                   ds.SyncMeta.OpenPRCount,
			"mainline_open_pr_count":          ds.SyncMeta.MainlineOpenPRCount,
			"pending_count":                   len(pendNums),
			"pending_numbers":                 pendNums,
			"retriage_count":                  len(retriageNums),
			"retriage_numbers":                retriageNums,
			"excluded_count":                  len(ds.SyncMeta.ExcludedIssueNums),
			"issue_snapshot_utc8":             ds.SyncMeta.SnapshotUTC8,
			"issue_watermark_utc8":            ds.SyncMeta.MaxIssueUpdatedUTC8,
			"pr_snapshot_utc8":                ds.SyncMeta.PRSnapshotUTC8,
			"incremental_gh_search_qualifier": ds.SyncMeta.IncrementalGHSearchQualifier,
		},
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
	// Mode A: Backfill notes for existing issues lacking root-cause notes
	if opts.BackfillNotes > 0 {
		exportedAt := model.NowRFC3339Nano()
		recs, err := sy.store.ListBackfillNoteCandidates(opts.BackfillNotes, opts.CatFilter)
		if err != nil {
			return nil, 0, fmt.Errorf("list backfill candidates: %w", err)
		}
		var prDetailMap map[string]model.PendingPRDetail
		if opts.EnrichPRs && len(recs) <= 30 {
			var allPRs []model.LinkedPRRef
			for _, r := range recs {
				allPRs = append(allPRs, triage.FilterRelevantKernelPRs(r.PRs)...)
			}
			if len(allPRs) > 0 {
				prDetailMap, _ = triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, allPRs)
			}
		}

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
		tmplOvr := make(map[string]model.TriagePatchRequest, len(recs))
		for _, r := range recs {
			var dets []model.PendingPRDetail
			for _, p := range triage.FilterRelevantKernelPRs(r.PRs) {
				if d, ok := prDetailMap[model.PRKey(p.Repo, p.Num)]; ok {
					dets = append(dets, d)
				}
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
				PRs:         r.PRs,
				PRDetails:   dets,
			})
			tmplOvr[strconv.Itoa(r.Number)] = model.NewTemplateOverridePatch(
				r.Cat,
				r.Severity,
				r.Subsystem,
				r.TriagedAt,
				&model.LocalizedNote{
					ZhCN: "TODO: 填写中文根因与影响简述",
					En:   "TODO: Fill in concise English root-cause & impact note",
				},
			)
		}

		bundle := map[string]any{
			"mode":            "backfill_notes",
			"exported_at_utc": exportedAt,
			"backfill_count":  len(items),
			"instructions":    "Inspect each issue & linked PR diff (`gh issue view <num> --repo pingcap/tidb --comments`, `gh pr diff <pr> --repo pingcap/tidb`), fill in `overrides` notes (preserving `base_exported_at` and `if_unmodified_since`), and apply via `triage-hub triage --stdin --skip-todo`.",
			"triage_rubric":   triage.DefaultRubric(opts.Compact),
			"backfill_issues": items,
			"decisions_template": model.BatchTriageDecisions{
				BaseExportedAt: exportedAt,
				Overrides:      tmplOvr,
			},
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

	// Capture exportedAt and read both SQLite snapshots BEFORE running slow GraphQL PR enrichment,
	// ensuring concurrency watermarks accurately match the database state exported here.
	exportedAt := model.NowRFC3339Nano()

	var pending []model.PendingCandidate
	if !skipPending {
		var err error
		pending, err = sy.store.ListPendingCandidates()
		if err != nil {
			return nil, 0, fmt.Errorf("list pending candidates: %w", err)
		}
		if len(wantPendingNums) > 0 || strings.TrimSpace(opts.ScopeFilter) != "" {
			sf := strings.ToLower(strings.TrimSpace(opts.ScopeFilter))
			filtered := make([]model.PendingCandidate, 0, len(pending))
			for _, pc := range pending {
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
			pending = filtered
		}
	}

	var retriageList []model.RetriageCandidate
	if !skipRetriage {
		var err error
		if len(specificRetriageNums) > 0 {
			retriageList, err = sy.store.ListRetriageCandidates(false, specificRetriageNums)
		} else {
			retriageList, err = sy.store.ListRetriageCandidates(true, nil)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("list retriage candidates: %w", err)
		}
	}

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
		var toPersist []model.PendingCandidate
		for k, idx := range dirtyIdx {
			wasUnanalyzed := pending[idx].SuggestedTriage == nil
			pending[idx] = enriched[k]
			if enrichErr == nil || !opts.EnrichPRs || wasUnanalyzed || !triage.CandidateNeedsPREnrichment(enriched[k]) {
				toPersist = append(toPersist, enriched[k])
			}
		}
		if len(toPersist) > 0 {
			_ = sy.store.UpdatePendingCandidates(toPersist)
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
			detMap, _ = triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, allPRs)
		}
		for i := range retriageList {
			for _, p := range triage.FilterRelevantKernelPRs(retriageList[i].PRs) {
				if d, ok := detMap[model.PRKey(p.Repo, p.Num)]; ok {
					retriageList[i].PRDetails = append(retriageList[i].PRDetails, d)
				}
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
			Cat:              cat,
			Severity:         sev,
			Subsystem:        subsys,
			IsBorderlineKept: borderline,
			Note: &model.LocalizedNote{
				ZhCN: "TODO: 填写中文根因与影响简述",
				En:   "TODO: Fill in concise English root-cause & impact note",
			},
		}
	}

	tmplOvr := make(map[string]model.TriagePatchRequest)
	for _, rc := range retriageList {
		if len(specificRetriageNums) > 0 {
			tmplOvr[strconv.Itoa(rc.Number)] = model.NewTemplateOverridePatch(
				rc.CurrentCat,
				rc.CurrentSeverity,
				rc.CurrentSubsys,
				rc.TriagedAt,
				&model.LocalizedNote{
					ZhCN: "TODO: 填写重审后的中文根因与影响简述（原备注见 retriage_candidates[].current_note）",
					En:   "TODO: Fill in recalibrated English root-cause & impact note (see retriage_candidates[].current_note)",
				},
			)
		}
	}

	bundle := map[string]any{
		"exported_at_utc": exportedAt,
		"pending_count":   len(pending),
		"retriage_count":  len(retriageList),
		"instructions":    "Independently judge `pending_candidates` (SQL realism/reachability, PR diff, difficulty, and severity — do NOT blindly accept `decisions_template` pre-fills) and `retriage_candidates` against `triage_rubric` (including `retriage_guide`). Preserve `base_exported_at` (and `if_unmodified_since` on `overrides`) so concurrent Web UI edits are safely detected. Submit decisions via `triage-hub triage --stdin` (supports `new_issues`, `exclude_issues`, `overrides`, `ack_retriage`, `manual_pr_links`, `--skip-todo`, and `--dry-run`). Canonical (`sig/planner` + `type/bug`) issues must NEVER be put in `exclude_issues`.",
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

	return bundle, len(pending) + len(retriageList), nil
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

	if len(cleaned.NewIssues) == 0 &&
		len(cleaned.ExcludeIssues) == 0 &&
		len(cleaned.Overrides) == 0 &&
		len(cleaned.AckRetriage) == 0 &&
		len(cleaned.ManualPRLinks) == 0 {
		return nil, fmt.Errorf("%w: no actionable entries remaining after validation (check if all entries were skipped by --skip-todo)", ErrBatchInputInvalid)
	}

	applyOpts := store.BatchApplyOptions{SkipConflicts: req.SkipConflicts}
	if req.DryRun {
		conflicts, valErr := sy.store.ValidateBatchDecisionsWithOptions(cleaned, applyOpts)
		if valErr != nil {
			if errors.Is(valErr, store.ErrBatchConflict) || errors.Is(valErr, store.ErrCanonicalCannotExclude) {
				return nil, valErr
			}
			return nil, fmt.Errorf("%w: %w", ErrBatchInputInvalid, valErr)
		}
		out := map[string]any{
			"dry_run":             true,
			"db_validated":        true,
			"new_issues_count":    len(cleaned.NewIssues),
			"exclude_count":       len(cleaned.ExcludeIssues),
			"overrides_count":     len(cleaned.Overrides),
			"ack_retriage_count":  len(cleaned.AckRetriage),
			"manual_pr_links":     len(cleaned.ManualPRLinks),
			"validation_warnings": warnings,
			"prepared_decisions":  cleaned,
		}
		if len(conflicts) > 0 {
			out["skipped_conflicts"] = conflicts
		}
		return out, nil
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
