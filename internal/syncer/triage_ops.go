package syncer

import (
	"context"
	"encoding/json"
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

// refreshExportEvidence persists missing evidence before an export reads its final
// snapshot. Read-only exports consistently use the evidence already in the store.
func (sy *Syncer) refreshExportEvidence(ctx context.Context, enabled bool, refs []model.LinkedPRRef) (string, error) {
	if !enabled || len(refs) == 0 {
		return "", nil
	}
	refs, err := sy.store.FilterAutomaticPRLookups(refs)
	if err != nil {
		return "", err
	}
	if len(refs) == 0 {
		return "", nil
	}
	if sy.store.ReadOnly() {
		return "read-only database: exporting persisted evidence without GitHub refresh", nil
	}
	details, fetchErr := triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, refs)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := sy.store.SyncFetchedPRDetails(details); err != nil {
		if store.IsReadOnlyError(err) {
			return "read-only database: refreshed evidence could not be persisted; exporting stored evidence", nil
		}
		return "", err
	}
	if fetchErr != nil {
		return fetchErr.Error(), nil
	}
	return "", nil
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
		var refs []model.LinkedPRRef
		for _, r := range recs {
			refs = append(refs, triage.PRsNeedingEnrichment(r.PRs, r.PRDetails)...)
		}
		enrichWarning, err := sy.refreshExportEvidence(ctx, opts.EnrichPRs && len(recs) <= 30, refs)
		if err != nil {
			return nil, 0, err
		}
		// Re-read after all external work, even when enrichment returned no PRs.
		// The timestamp is a conservative legacy cutoff; versions identify the snapshot.
		exportedAt := model.NowRFC3339Nano()
		recs, err = sy.store.ListBackfillNoteCandidates(opts.BackfillNotes, opts.CatFilter)
		if err != nil {
			return nil, 0, err
		}

		type backfillItem struct {
			Version     *model.ReviewVersion    `json:"version"`
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
		expectedVersions := make(map[string]model.ReviewVersion, len(recs))
		for _, r := range recs {
			expectedVersions[strconv.Itoa(r.Number)] = *r.Version
			syncedPRs := r.PRs
			dets := r.PRDetails
			items = append(items, backfillItem{
				Version:     r.Version,
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
				ExpectedVersion:   r.Version,
				IfUnmodifiedSince: r.TriagedAt,
			}
		}

		bundle := map[string]any{
			"mode":            "backfill_notes",
			"exported_at_utc": exportedAt,
			"backfill_count":  len(items),
			"instructions":    "Review each issue using `pr_details` first, and inspect `gh issue view <num> --repo pingcap/tidb --comments` or `gh pr diff <pr> --repo <owner/repo> --exclude '*_test.go' --exclude '*.test' --exclude '*.json' --exclude '*.result' --exclude '*.out'` when needed. Fill in both `zh-CN` and `en` under `notes` while keeping `expected_versions` and `expected_version` intact, then validate with `./bin/triage-hub triage --file <path> --skip-todo --dry-run` and apply without `--dry-run` (or via `--stdin`).",
			"triage_rubric":   triage.DefaultRubric(opts.Compact),
			"backfill_issues": items,
			"decisions_template": model.BatchTriageDecisions{
				ExpectedVersions: expectedVersions,
				BaseExportedAt:   exportedAt,
				Notes:            tmplNotes,
			},
		}
		if enrichWarning != "" {
			bundle["enrichment_warning"] = "PR file enrichment degraded: " + enrichWarning
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

	var refs []model.LinkedPRRef
	for _, pc := range pending {
		refs = append(refs, triage.PRsNeedingEnrichment(pc.PRs, pc.PRDetails)...)
	}
	for _, rc := range retriageList {
		refs = append(refs, triage.PRsNeedingEnrichment(rc.PRs, rc.PRDetails)...)
	}
	enrichWarning, err := sy.refreshExportEvidence(ctx, opts.EnrichPRs, refs)
	if err != nil {
		return nil, 0, err
	}

	// Finish enrichment first, then bind facts, notes and versions in one read.
	snapshot, err := sy.store.LoadReviewSnapshot(!skipPending, !skipRetriage, specificRetriageNums)
	if err != nil {
		return nil, 0, err
	}
	pending = filterPendingSlice(snapshot.Pending)
	retriageList = snapshot.Retriage
	exportedAt := snapshot.ExportedAt
	for i := range pending {
		p := &pending[i]
		if p.Clues == nil {
			p.Clues = triage.ExtractClues(p.Title, p.Body, p.Labels, p.RecentComments, p.PRDetails)
		}
		if p.SuggestedTriage == nil {
			p.SuggestedTriage = triage.SuggestTriage(p.Title, p.Labels, p.PRs, p.PRDetails, p.Clues)
		}
	}
	for i := range retriageList {
		r := &retriageList[i]
		r.Clues = triage.ExtractClues(r.Title, r.Body, r.Labels, r.RecentComments, r.PRDetails)
		r.SuggestedTriage = triage.SuggestTriage(r.Title, r.Labels, r.PRs, r.PRDetails, r.Clues)
	}

	if pending == nil {
		pending = []model.PendingCandidate{}
	}
	if retriageList == nil {
		retriageList = []model.RetriageCandidate{}
	}

	expectedVersions := make(map[string]model.ReviewVersion, len(pending)+len(retriageList))
	for _, r := range retriageList {
		expectedVersions[strconv.Itoa(r.Number)] = *r.Version
	}
	tmplNew := make(map[string]model.TriagePatchRequest, len(pending))
	suggestedExcludesHint := make(map[string]string)
	for _, pc := range pending {
		expectedVersions[strconv.Itoa(pc.Number)] = *pc.Version
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
			ExpectedVersion:   pc.Version,
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
		"instructions":    "Independently judge `pending_candidates` and `retriage_candidates` against `triage_rubric` for SQL realism/reachability, PR diff, difficulty, and severity; do not trust `decisions_template` pre-fills. Keep `expected_versions` and any pre-filled `expected_version` fields intact and matching so decisions stay bound to the reviewed snapshot. Resolve each flagged `retriage_candidates` item (`needs_retriage`) via ONE of `overrides` (omit `note` when unchanged; include `note` to update prose while clearing re-triage), `ack_retriage`, or `exclude_issues` (supplemental non-optimizer only; standalone `notes` updates prose without clearing `needs_retriage`). Canonical (`sig/planner` + `type/bug`) issues must NEVER be put in `exclude_issues`. Submit via `./bin/triage-hub triage --file <path>` or `--stdin` (`--dry-run`, `--skip-todo`).",
		"valid_enums": map[string]any{
			"cat":       model.ValidCategories,
			"severity":  model.ValidSeverities,
			"subsystem": model.ValidSubsystems,
		},
		"triage_rubric": triage.DefaultRubric(opts.Compact),
		"decisions_template": model.BatchTriageDecisions{
			ExpectedVersions: expectedVersions,
			BaseExportedAt:   exportedAt,
			NewIssues:        tmplNew,
			ExcludeIssues:    map[string]string{},
			Overrides:        tmplOvr,
			AckRetriage:      model.FlexIntList{},
		},
		"pending_candidates":  pending,
		"retriage_candidates": retriageList,
	}
	if len(suggestedExcludesHint) > 0 {
		bundle["suggested_excludes_hint"] = suggestedExcludesHint
	}
	if enrichWarning != "" {
		bundle["enrichment_warning"] = "PR file enrichment degraded: " + enrichWarning
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

type DryRunBatchResult struct {
	DryRun             bool                       `json:"dry_run"`
	DBValidated        bool                       `json:"db_validated"`
	NewIssuesCount     int                        `json:"new_issues_count"`
	ExcludeCount       int                        `json:"exclude_count"`
	OverridesCount     int                        `json:"overrides_count"`
	NotesCount         int                        `json:"notes_count"`
	AckRetriageCount   int                        `json:"ack_retriage_count"`
	ManualPRLinks      int                        `json:"manual_pr_links"`
	ValidationWarnings []triage.ValidationWarning `json:"validation_warnings"`
	PreparedDecisions  model.BatchTriageDecisions `json:"prepared_decisions"`
	SkippedConflicts   []model.TriageConflict     `json:"skipped_conflicts,omitempty"`
}

type BatchTriageExecutionResult struct {
	Applied TriageApplyResult
	Preview *DryRunBatchResult
}

func (r BatchTriageExecutionResult) MarshalJSON() ([]byte, error) {
	if r.Preview != nil {
		return json.Marshal(r.Preview)
	}
	return json.Marshal(r.Applied)
}

func FormatDryRunBatchResult(planned model.BatchTriageDecisions, dbValidated bool, warnings []triage.ValidationWarning, conflicts []model.TriageConflict) DryRunBatchResult {
	return DryRunBatchResult{DryRun: true, DBValidated: dbValidated, NewIssuesCount: len(planned.NewIssues), ExcludeCount: len(planned.ExcludeIssues), OverridesCount: len(planned.Overrides), NotesCount: len(planned.Notes), AckRetriageCount: len(planned.AckRetriage), ManualPRLinks: len(planned.ManualPRLinks), ValidationWarnings: warnings, PreparedDecisions: planned, SkippedConflicts: conflicts}
}

type PreparedBatchTriage struct {
	decisions     model.BatchTriageDecisions
	warnings      []triage.ValidationWarning
	dryRun        bool
	skipConflicts bool
}

func (p PreparedBatchTriage) SyntaxPreview() DryRunBatchResult {
	return FormatDryRunBatchResult(p.decisions, false, p.warnings, nil)
}

// PrepareBatchTriage is the shared syntax/rubric preflight for local and remote callers.
func PrepareBatchTriage(req BatchTriageExecuteRequest) (PreparedBatchTriage, error) {
	cleaned, warnings, err := triage.ValidateAndPrepareDecisions(req.Decisions, triage.ValidateOptions{
		SkipTodo:       req.SkipTodo,
		AllowEmptyNote: req.AllowEmptyNote,
		StrictNotes:    req.StrictNotes,
	})
	if err != nil {
		return PreparedBatchTriage{}, fmt.Errorf("%w: %v", ErrBatchInputInvalid, err)
	}

	if !HasActionableDecisions(cleaned) {
		return PreparedBatchTriage{}, fmt.Errorf("%w: no actionable entries remaining after validation (check if all entries were skipped by --skip-todo)", ErrBatchInputInvalid)
	}

	return PreparedBatchTriage{decisions: cleaned, warnings: warnings, dryRun: req.DryRun, skipConflicts: req.SkipConflicts}, nil
}

// ExecuteBatchTriage validates, optionally dry-runs, or applies batch triage decisions.
func (sy *Syncer) ExecuteBatchTriage(ctx context.Context, req BatchTriageExecuteRequest) (BatchTriageExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return BatchTriageExecutionResult{}, err
	}
	prepared, err := PrepareBatchTriage(req)
	if err != nil {
		return BatchTriageExecutionResult{}, err
	}

	return sy.ExecutePreparedBatchTriage(ctx, prepared)
}

// ExecutePreparedBatchTriage rechecks database preconditions but never repeats
// syntax preparation; CLI callers can safely validate before opening a database.
func (sy *Syncer) ExecutePreparedBatchTriage(ctx context.Context, prepared PreparedBatchTriage) (BatchTriageExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return BatchTriageExecutionResult{}, err
	}
	cleaned, warnings := prepared.decisions, prepared.warnings
	applyOpts := store.BatchApplyOptions{SkipConflicts: prepared.skipConflicts}
	if prepared.dryRun {
		planned, conflicts, valErr := sy.store.PlanBatchDecisionsWithOptions(cleaned, applyOpts)
		if valErr != nil {
			return BatchTriageExecutionResult{}, valErr
		}
		preview := FormatDryRunBatchResult(planned, true, warnings, conflicts)
		return BatchTriageExecutionResult{Preview: &preview}, nil
	}

	res, err := sy.ApplyTriageDecisionsWithOptions(ctx, cleaned, applyOpts, false, 0)
	if err != nil {
		return BatchTriageExecutionResult{Applied: res}, err
	}
	res.ValidationWarnings = warnings
	return BatchTriageExecutionResult{Applied: res}, nil
}
