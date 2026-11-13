package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

func inferSubsystem(labels []string, title string, clues *model.PendingClues, prs []model.LinkedPRRef) (string, string) {
	// 1. Code path / PR file prefix has highest fidelity
	if clues != nil {
		for _, p := range clues.CodePaths {
			lp := strings.ToLower(p)
			switch {
			case strings.HasPrefix(lp, "pkg/statistics/") || strings.Contains(lp, "/analyze"):
				return "statistics", fmt.Sprintf("code_path:%s", p)
			case strings.HasPrefix(lp, "pkg/bindinfo/") || strings.Contains(lp, "plan_cache"):
				return "plan_cache_spm_hint", fmt.Sprintf("code_path:%s", p)
			case strings.Contains(lp, "mpp") || strings.HasPrefix(lp, "dbms/"):
				return "tiflash_mpp", fmt.Sprintf("code_path:%s", p)
			case strings.HasPrefix(lp, "pkg/util/ranger/") ||
				strings.Contains(lp, "find_best_task") ||
				strings.Contains(lp, "point_get") ||
				strings.Contains(lp, "part_"):
				return "access_path_partition_lock", fmt.Sprintf("code_path:%s", p)
			case strings.HasPrefix(lp, "pkg/planner/cardinality/") ||
				strings.HasPrefix(lp, "pkg/planner/cascades/") ||
				strings.Contains(lp, "cbo") ||
				strings.Contains(lp, "join_reorder") ||
				strings.Contains(lp, "cost"):
				return "cbo_cost_join_reorder", fmt.Sprintf("code_path:%s", p)
			case strings.Contains(lp, "resolve") ||
				strings.Contains(lp, "logical_plan_builder"):
				return "name_resolution_group_by", fmt.Sprintf("code_path:%s", p)
			case strings.Contains(lp, "rule_") || strings.Contains(lp, "expression_rewriter"):
				return "logical_rewrite", fmt.Sprintf("code_path:%s", p)
			}
		}
	}

	for _, pr := range prs {
		if strings.HasPrefix(pr.Repo, "pingcap/tiflash") {
			return "tiflash_mpp", "linked_pr_repo:pingcap/tiflash"
		}
	}

	// 2. GitHub labels
	for _, lb := range labels {
		l := strings.ToLower(lb)
		switch {
		case l == "component/statistics":
			return "statistics", "label:component/statistics"
		case l == "component/bindinfo" || l == "component/spm" || l == "epic/plan-cache" || l == "epic/sql-plan-management" || l == "epic/hint":
			return "plan_cache_spm_hint", "label:" + lb
		case l == "component/tiflash":
			return "tiflash_mpp", "label:component/tiflash"
		case l == "planner/join-order" || l == "planner/cascades" || l == "epic/cardinality-estimation" || l == "epic/cost-model":
			return "cbo_cost_join_reorder", "label:" + lb
		case l == "component/tablepartition":
			return "access_path_partition_lock", "label:component/tablepartition"
		}
	}

	// 3. Title & SQL feature keywords
	lt := strings.ToLower(title)
	switch {
	case strings.Contains(lt, "analyze") || strings.Contains(lt, "histogram") || strings.Contains(lt, "stats") || strings.Contains(lt, "cmsketch"):
		return "statistics", "title_keyword:statistics"
	case strings.Contains(lt, "plan cache") || strings.Contains(lt, "binding") || strings.Contains(lt, "hint"):
		return "plan_cache_spm_hint", "title_keyword:plan_cache_spm_hint"
	case strings.Contains(lt, "tiflash") || strings.Contains(lt, "mpp"):
		return "tiflash_mpp", "title_keyword:tiflash_mpp"
	case strings.Contains(lt, "partition") || strings.Contains(lt, "ranger") || strings.Contains(lt, "index merge") || strings.Contains(lt, "point get") || strings.Contains(lt, "prefix index"):
		return "access_path_partition_lock", "title_keyword:access_path"
	case strings.Contains(lt, "cardinality") || strings.Contains(lt, "cost") || strings.Contains(lt, "join reorder") || strings.Contains(lt, "row count"):
		return "cbo_cost_join_reorder", "title_keyword:cbo_cost"
	case strings.Contains(lt, "only_full_group_by") || strings.Contains(lt, "unknown column") || strings.Contains(lt, "can't find column") || strings.Contains(lt, "grouping("):
		return "name_resolution_group_by", "title_keyword:name_resolution"
	}

	return "logical_rewrite", "default:logical_rewrite"
}

func hasOptimizerCodePath(paths []string) bool {
	for _, p := range paths {
		lp := strings.ToLower(p)
		if strings.HasPrefix(lp, "pkg/planner/") ||
			strings.HasPrefix(lp, "pkg/statistics/") ||
			strings.HasPrefix(lp, "pkg/bindinfo/") ||
			strings.HasPrefix(lp, "pkg/util/ranger/") {
			return true
		}
	}
	return false
}

func hasOnlyNonOptCodePaths(paths []string) bool {
	if len(paths) == 0 || hasOptimizerCodePath(paths) {
		return false
	}
	for _, p := range paths {
		lp := strings.ToLower(p)
		if strings.HasPrefix(lp, "pkg/executor/analyze") {
			return false
		}
	}
	return true
}

// SuggestTriage computes explainable pre-triage defaults from labels, clues, and PR file stats.
func SuggestTriage(
	title string,
	labels []string,
	prs []model.LinkedPRRef,
	prDetails []model.PendingPRDetail,
	clues *model.PendingClues,
) *model.SuggestedTriage {
	lbs := make(map[string]bool, len(labels))
	ghSev := ""
	for _, lb := range labels {
		lbs[lb] = true
		if strings.HasPrefix(lb, "severity/") && ghSev == "" {
			ghSev = strings.ToUpper(strings.TrimPrefix(lb, "severity/"))
		}
	}
	isCanonical := lbs["sig/planner"] && lbs["type/bug"]
	subsys, subSignal := inferSubsystem(labels, title, clues, prs)
	signals := []string{subSignal}
	if isCanonical {
		signals = append(signals, "canonical:sig/planner+type/bug")
	}

	var activeOrMergedDetails []model.PendingPRDetail
	var allMainlineDetails []model.PendingPRDetail
	hasMergedMainline := false
	for _, p := range prs {
		if !p.IsCherryPick && p.State == "MERGED" {
			hasMergedMainline = true
		}
	}
	for _, d := range prDetails {
		if d.IsCherryPick {
			continue
		}
		allMainlineDetails = append(allMainlineDetails, d)
		if d.State != "CLOSED" {
			activeOrMergedDetails = append(activeOrMergedDetails, d)
		}
	}
	mainlineDetails := activeOrMergedDetails
	if len(mainlineDetails) == 0 {
		mainlineDetails = allMainlineDetails
	}

	// Check if PR touches strictly non-optimizer code in pingcap/tidb (and file list was not truncated).
	// Cross-repo kernel PRs (e.g. pingcap/tiflash, pingcap/parser, tikv/tikv) and >30-file truncated PRs
	// must NOT be classified as non-optimizer noise based on TiDB directory prefixes.
	var (
		allNonTest           []string
		hasCrossRepoMainline bool
		hasTruncatedMainline bool
	)
	for _, d := range mainlineDetails {
		if d.Repo != "" && d.Repo != "pingcap/tidb" {
			hasCrossRepoMainline = true
		}
		if d.FilesTruncated {
			hasTruncatedMainline = true
		}
		allNonTest = append(allNonTest, d.NonTestFiles...)
	}
	if !hasCrossRepoMainline && !hasTruncatedMainline && hasOnlyNonOptCodePaths(allNonTest) {
		signals = append(signals, fmt.Sprintf("non_opt_pr_files:%v", allNonTest))
		if isCanonical {
			return &model.SuggestedTriage{
				RecommendedAction: "accept",
				Cat:               "FILTER_NON_OPT_OR_FIXED",
				Severity:          "N/A",
				Subsystem:         subsys,
				Confidence:        "high",
				Signals:           signals,
			}
		}
		return &model.SuggestedTriage{
			RecommendedAction: "exclude",
			ExcludeReason:     fmt.Sprintf("Fix PR modifies non-optimizer files (%s)", strings.Join(allNonTest, ", ")),
			Cat:               "FILTER_NON_OPT_OR_FIXED",
			Severity:          "N/A",
			Subsystem:         subsys,
			Confidence:        "high",
			Signals:           signals,
		}
	}

	// Check flaky unit test / already fixed on master
	lowerTitle := strings.ToLower(title)
	if lbs["flaky-test"] || strings.Contains(lowerTitle, "data race") && strings.Contains(lowerTitle, "test") {
		signals = append(signals, "flaky_or_unit_test")
		if isCanonical {
			return &model.SuggestedTriage{
				RecommendedAction: "accept",
				Cat:               "FILTER_NON_OPT_OR_FIXED",
				Severity:          "N/A",
				Subsystem:         subsys,
				Confidence:        "high",
				Signals:           signals,
			}
		}
		return &model.SuggestedTriage{
			RecommendedAction: "exclude",
			ExcludeReason:     "Flaky unit test / test-only issue",
			Cat:               "FILTER_NON_OPT_OR_FIXED",
			Severity:          "N/A",
			Subsystem:         subsys,
			Confidence:        "high",
			Signals:           signals,
		}
	}

	if hasMergedMainline {
		signals = append(signals, "mainline_pr_already_merged")
		return &model.SuggestedTriage{
			RecommendedAction: "review_carefully",
			Cat:               "FILTER_NON_OPT_OR_FIXED",
			Severity:          "N/A",
			Subsystem:         subsys,
			Confidence:        "medium",
			Signals:           signals,
		}
	}

	// Enhancement check
	if lbs["type/enhancement"] || lbs["type/feature-request"] {
		signals = append(signals, "label:enhancement")
		return &model.SuggestedTriage{
			RecommendedAction: "accept",
			Cat:               "ENHANCEMENT",
			Severity:          "N/A",
			Subsystem:         subsys,
			Confidence:        "high",
			Signals:           signals,
		}
	}

	hasFuzzOrigin := false
	hasAuditOrigin := false
	hasSevereErr := false
	hasPanicOrOOM := false
	hasWrongRes := false
	if clues != nil {
		for _, o := range clues.OriginSignals {
			if o == "fuzzer_or_sqlancer" {
				hasFuzzOrigin = true
			}
			if o == "code_or_ai_audit" {
				hasAuditOrigin = true
			}
		}
		for _, e := range clues.ErrorSignatures {
			switch e {
			case "panic", "oom", "missing_lock":
				hasPanicOrOOM = true
				hasSevereErr = true
			case "wrong_result", "error_1105", "deadlock_or_hang":
				hasWrongRes = true
				hasSevereErr = true
			}
		}
	}

	if hasFuzzOrigin && !lbs["report/customer"] && !hasPanicOrOOM && len(mainlineDetails) == 0 {
		signals = append(signals, "origin:fuzzer_or_sqlancer")
		if isCanonical {
			return &model.SuggestedTriage{
				RecommendedAction: "review_carefully",
				Cat:               "FILTER_RANDOM_FUZZ",
				Severity:          "N/A",
				Subsystem:         subsys,
				Confidence:        "medium",
				Signals:           signals,
			}
		}
		return &model.SuggestedTriage{
			RecommendedAction: "review_carefully",
			ExcludeReason:     "Potential non-canonical fuzzer noise; verify whether trigger SQL is realistic",
			Cat:               "FILTER_RANDOM_FUZZ",
			Severity:          "N/A",
			Subsystem:         subsys,
			Confidence:        "low",
			Signals:           signals,
		}
	}

	// Real Bug grading: Difficulty (EASY / MEDIUM / HARD)
	cat := "MEDIUM"
	confidence := "medium"
	if len(mainlineDetails) > 0 {
		totalFiles := 0
		totalChangedLines := 0
		reposTouched := make(map[string]bool)
		for _, d := range mainlineDetails {
			totalFiles += d.NonTestFileCount
			totalChangedLines += d.NonTestAdditions + d.NonTestDeletions
			reposTouched[d.Repo] = true
		}
		signals = append(signals, fmt.Sprintf("pr_non_test_files:%d,lines:%d", totalFiles, totalChangedLines))
		switch {
		case len(reposTouched) > 1 || totalFiles >= 6 || totalChangedLines >= 280:
			cat = "HARD"
			confidence = "high"
		case totalFiles >= 1 && totalFiles <= 2 && totalChangedLines > 0 && totalChangedLines <= 55:
			cat = "EASY"
			confidence = "high"
		default:
			cat = "MEDIUM"
			confidence = "high"
		}
	} else if subsys == "tiflash_mpp" {
		cat = "HARD"
		confidence = "low"
	}

	// Severity grading
	sev := "MODERATE"
	switch {
	case ghSev == "CRITICAL" || hasPanicOrOOM:
		sev = "CRITICAL"
		signals = append(signals, "severity_signal:critical_or_panic")
	case ghSev == "MAJOR" || hasWrongRes || lbs["report/customer"]:
		sev = "MAJOR"
		signals = append(signals, "severity_signal:major_or_wrong_result")
	case ghSev == "MINOR":
		sev = "MINOR"
	case ghSev == "MODERATE":
		sev = "MODERATE"
	default:
		if hasSevereErr {
			sev = "MAJOR"
		}
	}

	borderline := (hasFuzzOrigin || hasAuditOrigin) && model.IsRealBugCategory(cat)
	if borderline {
		signals = append(signals, "borderline:fuzz_or_audit_real_bug")
	}

	action := "accept"
	if !isCanonical && confidence == "low" {
		action = "review_carefully"
	}

	return &model.SuggestedTriage{
		RecommendedAction: action,
		Cat:               cat,
		Severity:          sev,
		Subsystem:         subsys,
		IsBorderlineKept:  borderline,
		Confidence:        confidence,
		Signals:           signals,
	}
}

func AnalyzeCandidate(pc *model.PendingCandidate) {
	if pc == nil {
		return
	}
	pc.Clues = ExtractClues(pc.Title, pc.Body, pc.Labels, pc.RecentComments, pc.PRDetails)
	pc.SuggestedTriage = SuggestTriage(pc.Title, pc.Labels, pc.PRs, pc.PRDetails, pc.Clues)
}

// CandidateNeedsPREnrichment reports whether any active/merged kernel mainline PR on `c`
// lacks cached PRDetails or has changed state/updatedAt since it was last enriched.
func CandidateNeedsPREnrichment(c model.PendingCandidate) bool {
	detailMap := make(map[string]model.PendingPRDetail, len(c.PRDetails))
	for _, d := range c.PRDetails {
		detailMap[model.PRKey(d.Repo, d.Num)] = d
	}
	hasActiveOrMerged := false
	for _, pr := range c.PRs {
		if !pr.IsCherryPick && pr.State != "CLOSED" && issuesync.IsKernelRepo(pr.Repo) {
			hasActiveOrMerged = true
			break
		}
	}
	for _, pr := range c.PRs {
		if pr.IsCherryPick || !issuesync.IsKernelRepo(pr.Repo) {
			continue
		}
		if hasActiveOrMerged && pr.State == "CLOSED" {
			continue
		}
		prev, ok := detailMap[model.PRKey(pr.Repo, pr.Num)]
		if !ok || prev.State != pr.State || (pr.UpdatedAt != "" && prev.UpdatedAt != pr.UpdatedAt) {
			return true
		}
	}
	return false
}

// EnrichAndAnalyzePending enriches pending candidates with PR file stats (when fetchPRFiles is true)
// and computes Clues + SuggestedTriage on each candidate. Network errors fail open cleanly.
func EnrichAndAnalyzePending(
	ctx context.Context,
	runner ghgql.Runner,
	tracker *ghgql.CostTracker,
	candidates []model.PendingCandidate,
	fetchPRFiles bool,
) ([]model.PendingCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	var fetchErr error
	detailMap := make(map[string]model.PendingPRDetail)
	for _, c := range candidates {
		for _, d := range c.PRDetails {
			detailMap[model.PRKey(d.Repo, d.Num)] = d
		}
	}
	if fetchPRFiles {
		var missingPRs []model.LinkedPRRef
		for _, c := range candidates {
			hasActiveOrMerged := false
			for _, pr := range c.PRs {
				if !pr.IsCherryPick && pr.State != "CLOSED" && issuesync.IsKernelRepo(pr.Repo) {
					hasActiveOrMerged = true
					break
				}
			}
			for _, pr := range c.PRs {
				if pr.IsCherryPick || !issuesync.IsKernelRepo(pr.Repo) {
					continue
				}
				if hasActiveOrMerged && pr.State == "CLOSED" {
					continue
				}
				if prev, ok := detailMap[model.PRKey(pr.Repo, pr.Num)]; ok &&
					prev.State == pr.State &&
					(pr.UpdatedAt == "" || prev.UpdatedAt == pr.UpdatedAt) {
					continue
				}
				missingPRs = append(missingPRs, pr)
			}
		}
		if len(missingPRs) > 0 {
			fetched, err := FetchPendingPRDetails(ctx, runner, tracker, missingPRs)
			fetchErr = err
			for k, v := range fetched {
				detailMap[k] = v
			}
		}
	}

	out := make([]model.PendingCandidate, len(candidates))
	for i := range candidates {
		pc := candidates[i]
		if len(detailMap) > 0 {
			hasActiveOrMerged := false
			for _, pr := range pc.PRs {
				if !pr.IsCherryPick && pr.State != "CLOSED" && issuesync.IsKernelRepo(pr.Repo) {
					hasActiveOrMerged = true
					break
				}
			}
			var dets []model.PendingPRDetail
			for _, pr := range pc.PRs {
				if hasActiveOrMerged && pr.State == "CLOSED" {
					continue
				}
				if d, ok := detailMap[model.PRKey(pr.Repo, pr.Num)]; ok {
					dets = append(dets, d)
				}
			}
			pc.PRDetails = dets
		}
		AnalyzeCandidate(&pc)
		out[i] = pc
	}
	return out, fetchErr
}
