package triage

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

type staticRunner struct {
	payload string
}

func (s staticRunner) Query(_ context.Context, _ string, _ map[string]string) ([]byte, error) {
	return []byte(s.payload), nil
}

func TestEnrichAndSuggestTriage(t *testing.T) {
	mockResp := `{
	  "data": {
	    "rateLimit": {"limit": 5000, "cost": 1, "remaining": 4999, "used": 1, "resetAt": "2026-11-13T04:00:00Z"},
	    "repository": {
	      "p73101": {
	        "number": 73101,
	        "title": "planner: include session var in plan cache key",
	        "state": "OPEN",
	        "isDraft": false,
	        "baseRefName": "master",
	        "additions": 255,
	        "deletions": 8,
	        "changedFiles": 3,
	        "body": "<!-- checklist -->\n### What problem does this PR solve?\nMissing var in ` + "`planCacheKey`" + `.\n- [x] Tests",
	        "author": {"login": "dev1"},
	        "files": {
	          "nodes": [
	            {"path": "pkg/planner/core/plan_cache_param.go", "additions": 15, "deletions": 3},
	            {"path": "pkg/planner/core/plan_cache_test.go", "additions": 120, "deletions": 5},
	            {"path": "pkg/planner/core/testdata/plan_cache_suite_out.json", "additions": 120, "deletions": 0}
	          ]
	        }
	      }
	    }
	  }
	}`

	cands := []model.PendingCandidate{
		{
			Number: 73100,
			Title:  "Prepared plan cache returns wrong result when session variable changes",
			Labels: []string{"sig/planner", "type/bug", "severity/major"},
			PRs: []model.LinkedPRRef{
				{Num: 73101, Repo: "pingcap/tidb", Label: "#73101", State: "OPEN"},
			},
			Body: "Prepared statement returns wrong result.",
		},
	}

	enriched, err := EnrichAndAnalyzePending(context.Background(), staticRunner{payload: mockResp}, nil, cands, true)
	if err != nil {
		t.Fatalf("EnrichAndAnalyzePending: %v", err)
	}
	got := enriched[0]
	if len(got.PRDetails) != 1 {
		t.Fatalf("expected 1 PR detail, got %d", len(got.PRDetails))
	}
	pd := got.PRDetails[0]
	if pd.NonTestFileCount != 1 || pd.TestFileCount != 2 || pd.NonTestAdditions != 15 {
		t.Fatalf("unexpected test/non-test file split: %+v", pd)
	}
	if got.SuggestedTriage == nil {
		t.Fatal("expected SuggestedTriage")
	}
	if got.SuggestedTriage.Cat != "EASY" || got.SuggestedTriage.Severity != "MAJOR" || got.SuggestedTriage.Subsystem != "plan_cache_spm_hint" {
		t.Fatalf("unexpected suggestion: %+v", got.SuggestedTriage)
	}

	// Non-canonical candidate whose PR only touches pkg/expression -> suggest exclude
	nonOptSug := SuggestTriage(
		"cast decimal error",
		[]string{"type/bug"},
		[]model.LinkedPRRef{{Num: 73200, Repo: "pingcap/tidb", State: "OPEN"}},
		[]model.PendingPRDetail{{
			Repo:             "pingcap/tidb",
			Num:              73200,
			State:            "OPEN",
			NonTestFileCount: 1,
			NonTestFiles:     []string{"pkg/expression/builtin_cast.go"},
		}},
		&model.PendingClues{CodePaths: []string{"pkg/expression/builtin_cast.go"}},
	)
	if nonOptSug.RecommendedAction != "exclude" {
		t.Fatalf("expected non-canonical non-opt PR to recommend exclude, got %+v", nonOptSug)
	}

	// Canonical candidate whose PR only touches pkg/expression -> keep as FILTER_NON_OPT_OR_FIXED
	canNonOptSug := SuggestTriage(
		"cast decimal error",
		[]string{"sig/planner", "type/bug"},
		[]model.LinkedPRRef{{Num: 73200, Repo: "pingcap/tidb", State: "OPEN"}},
		[]model.PendingPRDetail{{
			Repo:             "pingcap/tidb",
			Num:              73200,
			State:            "OPEN",
			NonTestFileCount: 1,
			NonTestFiles:     []string{"pkg/expression/builtin_cast.go"},
		}},
		&model.PendingClues{CodePaths: []string{"pkg/expression/builtin_cast.go"}},
	)
	if canNonOptSug.RecommendedAction != "accept" || canNonOptSug.Cat != "FILTER_NON_OPT_OR_FIXED" {
		t.Fatalf("expected canonical non-opt PR to suggest accept + FILTER_NON_OPT_OR_FIXED, got %+v", canNonOptSug)
	}
}

func TestEvaluateRetriageAllSixTriggers(t *testing.T) {
	// Zero false positives when live matches baseline
	reasons, _ := EvaluateRetriage(RetriageEvalInput{
		IssueAuthor:          "reporter",
		CurrentCat:           "MEDIUM",
		CurrentSeverity:      "MAJOR",
		CurrentLabels:        []string{"sig/planner", "type/bug", "severity/major"},
		CurrentGHSeverity:    "major",
		CurrentIsCustomer:    false,
		CurrentPRs:           []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 100, Label: "#100", State: "OPEN"}},
		BaselineLabels:       []string{"sig/planner", "type/bug", "severity/major"},
		BaselineGHSeverity:   "major",
		BaselineIsCustomer:   false,
		BaselineOpenPRKeys:   []string{"pingcap/tidb#100"},
		CommentWatermarkUTC:  "2026-11-10T00:00:00Z",
		RecentComments: []model.PendingComment{
			// Bot comment + slash command after watermark -> MUST NOT trigger
			{Author: "ti-chi-bot", CreatedAt: "2026-11-11T00:00:00Z", Body: "root cause analysis by bot"},
			{Author: "maintainer", CreatedAt: "2026-11-11T01:00:00Z", Body: "/label severity/major\n/cc @someone"},
		},
	})
	if len(reasons) != 0 {
		t.Fatalf("expected 0 false-positive triggers, got %v", reasons)
	}

	// Trigger all 6 signals simultaneously on state transition
	reasons6, details6 := EvaluateRetriage(RetriageEvalInput{
		IssueAuthor:          "reporter",
		CurrentCat:           "MEDIUM",
		CurrentSeverity:      "MAJOR",
		CurrentLabels:        []string{"sig/planner", "type/bug", "type/enhancement", "report/customer", "severity/critical"},
		CurrentGHSeverity:    "critical",
		CurrentIsCustomer:    true,
		CurrentPRs: []model.LinkedPRRef{
			{Repo: "pingcap/tidb", Num: 201, Label: "#201", State: "OPEN"},
			{Repo: "pingcap/tidb", Num: 202, Label: "#202", State: "MERGED"},
		},
		BaselineLabels:       []string{"sig/planner", "type/bug", "severity/major"},
		BaselineGHSeverity:   "major",
		BaselineIsCustomer:   false,
		BaselineOpenPRKeys:   nil,
		BaselineMergedPRKeys: nil,
		CommentWatermarkUTC:  "2026-11-10T00:00:00Z",
		RecentComments: []model.PendingComment{
			{
				Author:    "maintainer1",
				CreatedAt: "2026-11-12T00:00:00Z",
				Body:      "Root cause is in `detachCondAndBuildRange`, but part of this request is actually by design.",
			},
		},
	})
	if len(reasons6) != 6 || len(details6) != 6 {
		t.Fatalf("expected all 6 triggers, got %d: %v", len(reasons6), reasons6)
	}
}

func TestValidateAndPrepareDecisions(t *testing.T) {
	dec := model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"73001": {
				Cat:       "EASY",
				Severity:  "MAJOR",
				Subsystem: "logical_rewrite",
				Note: &model.LocalizedNote{
					ZhCN: "`expressionRewriter` 在改写常量比较时遗漏空值检查导致错结果",
					En:   "`expressionRewriter` misses null check when rewriting constant comparison, causing wrong result",
				},
			},
			// Leftover TODO in new_issues while moved to exclude_issues -> auto-resolved!
			"73002": {
				Cat:       "MEDIUM",
				Severity:  "MAJOR",
				Subsystem: "logical_rewrite",
				Note: &model.LocalizedNote{
					ZhCN: "TODO: 填写中文",
					En:   "TODO: fill English",
				},
			},
			// Incomplete TODO entry -> skipped when SkipTodo=true
			"73003": {
				Cat:       "MEDIUM",
				Severity:  "MAJOR",
				Subsystem: "statistics",
				Note: &model.LocalizedNote{
					ZhCN: "TODO: pending",
					En:   "TODO: pending",
				},
			},
		},
		ExcludeIssues: map[string]string{
			"73002": "Executor builtin cast bug",
		},
	}

	cleaned, _, err := ValidateAndPrepareDecisions(dec, ValidateOptions{SkipTodo: true})
	if err != nil {
		t.Fatalf("expected clean validation with SkipTodo + conflict auto-resolve, got: %v", err)
	}
	if len(cleaned.NewIssues) != 1 || cleaned.NewIssues["73001"].Cat != "EASY" {
		t.Fatalf("expected only #73001 in NewIssues, got %+v", cleaned.NewIssues)
	}
	if len(cleaned.ExcludeIssues) != 1 || cleaned.ExcludeIssues["73002"] == "" {
		t.Fatalf("expected #73002 in ExcludeIssues, got %+v", cleaned.ExcludeIssues)
	}

	// Unmatched backtick is rejected
	bad := model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"73009": {
				Cat:       "EASY",
				Severity:  "MAJOR",
				Subsystem: "logical_rewrite",
				Note: &model.LocalizedNote{
					ZhCN: "缺少闭合反引号 `PlanBuilder 导致崩溃",
					En:   "Unclosed backtick `PlanBuilder causes panic",
				},
			},
		},
	}
	if _, _, err := ValidateAndPrepareDecisions(bad, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "backtick") {
		t.Fatalf("expected unmatched backtick error, got: %v", err)
	}
}

func TestDefaultRubricModes(t *testing.T) {
	full := DefaultRubric(false)
	compact := DefaultRubric(true)
	if len(full.FewShotExamples) != 12 {
		t.Fatalf("expected 12 few-shot examples in full rubric, got %d", len(full.FewShotExamples))
	}
	if len(compact.FewShotExamples) != 0 {
		t.Fatalf("expected 0 few-shot examples in compact rubric, got %d", len(compact.FewShotExamples))
	}
	expectedRetriageKeys := []string{
		model.RetriageNewFixPR,
		model.RetriageFixPRMergedOpen,
		model.RetriageCustomerReported,
		model.RetriageGHSeverityEscalated,
		model.RetriageLabelScopeChanged,
		model.RetriageSubstantiveComments,
	}
	for _, r := range []Rubric{full, compact} {
		if len(r.RetriageGuide) != len(expectedRetriageKeys) {
			t.Fatalf("expected %d retriage guide entries, got %d", len(expectedRetriageKeys), len(r.RetriageGuide))
		}
		for _, k := range expectedRetriageKeys {
			if strings.TrimSpace(r.RetriageGuide[k]) == "" {
				t.Fatalf("missing retriage guide entry for %q", k)
			}
		}
	}
	for idx, ex := range full.FewShotExamples {
		numStr := strconv.Itoa(90000 + idx)
		switch ex.Action {
		case "accept":
			dec := model.BatchTriageDecisions{
				NewIssues: map[string]model.TriagePatchRequest{
					numStr: {
						Cat:              ex.Cat,
						Severity:         ex.Severity,
						Subsystem:        ex.Subsystem,
						IsBorderlineKept: ex.IsBorderlineKept,
						Note:             ex.Note,
					},
				},
			}
			_, warns, err := ValidateAndPrepareDecisions(dec, ValidateOptions{StrictNotes: true})
			if err != nil || len(warns) > 0 {
				t.Fatalf("few-shot example %q failed validation: err=%v warns=%+v", ex.IssueRef, err, warns)
			}
		case "exclude":
			if strings.TrimSpace(ex.ExcludeReason) == "" {
				t.Fatalf("few-shot exclude example %q missing ExcludeReason", ex.IssueRef)
			}
		default:
			t.Fatalf("unexpected few-shot action %q on %q", ex.Action, ex.IssueRef)
		}
	}
}

func TestSuggestTriageCrossRepoTruncatedAndClosedPRs(t *testing.T) {
	// 1. Cross-repo TiFlash MPP PR must NOT be misclassified as non-optimizer exclude/FILTER_NON_OPT_OR_FIXED
	tiflashSug := SuggestTriage(
		"MPP exchange sender hangs on partitioned table",
		[]string{"type/bug"},
		[]model.LinkedPRRef{{Num: 11200, Repo: "pingcap/tiflash", State: "OPEN"}},
		[]model.PendingPRDetail{{
			Repo:             "pingcap/tiflash",
			Num:              11200,
			State:            "OPEN",
			NonTestFileCount: 1,
			NonTestFiles:     []string{"dbms/src/Flash/Coprocessor/DAGQueryInfo.cpp"},
		}},
		&model.PendingClues{CodePaths: []string{"dbms/src/Flash/Coprocessor/DAGQueryInfo.cpp"}},
	)
	if tiflashSug.RecommendedAction == "exclude" || tiflashSug.Cat == "FILTER_NON_OPT_OR_FIXED" {
		t.Fatalf("expected cross-repo TiFlash PR not to be marked non-opt, got %+v", tiflashSug)
	}
	if tiflashSug.Subsystem != "tiflash_mpp" {
		t.Fatalf("expected subsystem tiflash_mpp, got %s", tiflashSug.Subsystem)
	}

	// 2. Truncated >30-file PR (FilesTruncated=true) must NOT trigger allNonOpt
	truncSug := SuggestTriage(
		"planner: expression pushdown wrong result",
		[]string{"type/bug"},
		[]model.LinkedPRRef{{Num: 73500, Repo: "pingcap/tidb", State: "OPEN"}},
		[]model.PendingPRDetail{{
			Repo:             "pingcap/tidb",
			Num:              73500,
			State:            "OPEN",
			FilesTruncated:   true,
			NonTestFileCount: 30,
			NonTestFiles:     []string{"pkg/expression/scalar_function.go"},
		}},
		nil,
	)
	if truncSug.RecommendedAction == "exclude" || truncSug.Cat == "FILTER_NON_OPT_OR_FIXED" {
		t.Fatalf("expected FilesTruncated PR not to trigger non-opt exclusion, got %+v", truncSug)
	}

	// 3. Abandoned CLOSED PR touching pkg/expression must NOT override active OPEN PR touching pkg/planner
	mixedSug := SuggestTriage(
		"wrong result in plan cache",
		[]string{"type/bug"},
		[]model.LinkedPRRef{
			{Num: 73601, Repo: "pingcap/tidb", State: "CLOSED"},
			{Num: 73602, Repo: "pingcap/tidb", State: "OPEN"},
		},
		[]model.PendingPRDetail{
			{
				Repo:             "pingcap/tidb",
				Num:              73601,
				State:            "CLOSED",
				NonTestFileCount: 1,
				NonTestFiles:     []string{"pkg/expression/builtin_cast.go"},
			},
			{
				Repo:             "pingcap/tidb",
				Num:              73602,
				State:            "OPEN",
				NonTestFileCount: 1,
				NonTestAdditions: 12,
				NonTestDeletions: 2,
				NonTestFiles:     []string{"pkg/planner/core/plan_cache_param.go"},
			},
		},
		nil,
	)
	if mixedSug.RecommendedAction != "accept" || mixedSug.Cat != "EASY" {
		t.Fatalf("expected active OPEN planner PR to win over CLOSED expression PR, got %+v", mixedSug)
	}
}

func TestRetriageChronologicalCompareAndValidateCollisions(t *testing.T) {
	// Comment at 2026-11-10T08:30:00+08:00 (= 00:30:00Z) is BEFORE watermark 2026-11-10T01:00:00Z,
	// even though string comparison "2026-11-10T08:30..." > "2026-11-10T01:00..." would be true!
	reasons, _ := EvaluateRetriage(RetriageEvalInput{
		IssueAuthor:         "reporter",
		CurrentCat:          "MEDIUM",
		CurrentSeverity:     "MAJOR",
		CurrentLabels:       []string{"sig/planner", "type/bug"},
		BaselineLabels:      []string{"sig/planner", "type/bug"},
		CommentWatermarkUTC: "2026-11-10T01:00:00Z",
		RecentComments: []model.PendingComment{
			{Author: "maintainer", CreatedAt: "2026-11-10T08:30:00+08:00", Body: "Root cause is in `PlanBuilder`."},
		},
	})
	if len(reasons) != 0 {
		t.Fatalf("expected +08:00 comment earlier than UTC watermark not to trigger retriage, got %v", reasons)
	}

	// Duplicate normalized keys ("73001" vs "#73001") are rejected
	dup := model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"73001":  {Cat: "EASY", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "一", En: "One"}},
			"#73001": {Cat: "HARD", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "二", En: "Two"}},
		},
	}
	if _, _, err := ValidateAndPrepareDecisions(dup, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate issue key collision error, got: %v", err)
	}

	// Invalid manual_pr_links spec is rejected
	badLink := model.BatchTriageDecisions{
		ManualPRLinks: map[string][]string{
			"73001": {"not_a_pr"},
		},
	}
	if _, _, err := ValidateAndPrepareDecisions(badLink, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "manual_pr_links") {
		t.Fatalf("expected invalid manual_pr_links error, got: %v", err)
	}

	// Cross-field conflict between exclude_issues and overrides is rejected
	conflict := model.BatchTriageDecisions{
		ExcludeIssues: map[string]string{"73002": "non-opt"},
		Overrides: map[string]model.TriagePatchRequest{
			"#73002": {Cat: "EASY", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "备注", En: "Note"}},
		},
	}
	if _, _, err := ValidateAndPrepareDecisions(conflict, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "both `overrides` and `exclude_issues`") {
		t.Fatalf("expected cross-field conflict error, got: %v", err)
	}

	// Cross-field conflict between new_issues and overrides is rejected
	newOvrConflict := model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"73005": {Cat: "EASY", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "新增", En: "New"}},
		},
		Overrides: map[string]model.TriagePatchRequest{
			"#73005": {Cat: "HARD", Severity: "MAJOR", Subsystem: "statistics", Note: &model.LocalizedNote{ZhCN: "覆盖", En: "Override"}},
		},
	}
	if _, _, err := ValidateAndPrepareDecisions(newOvrConflict, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "both `new_issues` and `overrides`") {
		t.Fatalf("expected new_issues + overrides conflict error, got: %v", err)
	}
}

func TestRound2PatchMarshalRoundTripAndCluesClosedPRFilter(t *testing.T) {
	// 1. TriagePatchRequest JSON round-trip preserves unset optional booleans and ignores JSON null
	var req model.TriagePatchRequest
	if err := req.UnmarshalJSON([]byte(`{"cat":"HARD","severity":"MAJOR","subsystem":"statistics","is_borderline_kept":null}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if req.HasBorderlineField() || req.HasForceKeepField() {
		t.Fatalf("expected null/omitted booleans to leave presence flags false, got bdr=%v fk=%v", req.HasBorderlineField(), req.HasForceKeepField())
	}
	outBytes, err := req.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(outBytes), "is_borderline_kept") || strings.Contains(string(outBytes), "force_keep") {
		t.Fatalf("expected unset boolean fields to be omitted from dry-run JSON, got %s", string(outBytes))
	}

	// 2. ExtractClues ignores abandoned CLOSED PR paths when an active OPEN PR exists
	clues := ExtractClues(
		"wrong result in query",
		"",
		[]string{"type/bug"},
		nil,
		[]model.PendingPRDetail{
			{
				Repo:             "pingcap/tidb",
				Num:              70001,
				State:            "CLOSED",
				NonTestFileCount: 1,
				NonTestFiles:     []string{"pkg/statistics/handle/cache.go"},
			},
			{
				Repo:             "pingcap/tidb",
				Num:              70002,
				State:            "OPEN",
				NonTestFileCount: 1,
				NonTestFiles:     []string{"pkg/planner/core/logical_join.go"},
			},
		},
	)
	for _, p := range clues.CodePaths {
		if strings.Contains(p, "pkg/statistics") {
			t.Fatalf("expected CLOSED PR path pkg/statistics to be excluded when OPEN PR exists, got %v", clues.CodePaths)
		}
	}
	sug := SuggestTriage("wrong result in query", []string{"type/bug"}, []model.LinkedPRRef{
		{Repo: "pingcap/tidb", Num: 70001, State: "CLOSED"},
		{Repo: "pingcap/tidb", Num: 70002, State: "OPEN"},
	}, []model.PendingPRDetail{
		{Repo: "pingcap/tidb", Num: 70002, State: "OPEN", NonTestFileCount: 1, NonTestAdditions: 15, NonTestFiles: []string{"pkg/planner/core/logical_join.go"}},
	}, clues)
	if sug.Subsystem != "logical_rewrite" {
		t.Fatalf("expected subsystem logical_rewrite from active OPEN PR (ignoring CLOSED statistics PR), got %s", sug.Subsystem)
	}

	// Fuzz + panic without customer report must still require AI Judge review_carefully (low confidence)
	fuzzPanicSug := SuggestTriage(
		"panic in planner",
		[]string{"sig/planner", "type/bug"},
		nil,
		nil,
		&model.PendingClues{
			OriginSignals:   []string{"fuzzer_or_sqlancer"},
			ErrorSignatures: []string{"panic"},
		},
	)
	if fuzzPanicSug.RecommendedAction != "review_carefully" || fuzzPanicSug.Confidence != "low" {
		t.Fatalf("expected fuzz+panic without PR to require review_carefully (low confidence), got %+v", fuzzPanicSug)
	}

	// Small CBO diff must not be auto-graded EASY with high confidence
	smallCboSug := SuggestTriage(
		"wrong cardinality estimate",
		[]string{"sig/planner", "type/bug"},
		[]model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 70003, State: "OPEN"}},
		[]model.PendingPRDetail{{
			Repo:             "pingcap/tidb",
			Num:              70003,
			State:            "OPEN",
			NonTestFileCount: 1,
			NonTestAdditions: 10,
			NonTestFiles:     []string{"pkg/planner/cardinality/selectivity.go"},
		}},
		&model.PendingClues{CodePaths: []string{"pkg/planner/cardinality/selectivity.go"}},
	)
	if smallCboSug.Cat == "EASY" || smallCboSug.Confidence != "low" {
		t.Fatalf("expected small CBO diff not to auto-grade EASY, got %+v", smallCboSug)
	}
}
