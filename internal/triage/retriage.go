package triage

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

type RetriageEvalInput struct {
	IssueAuthor          string
	CurrentCat           string
	CurrentSeverity      string
	CurrentLabels        []string
	CurrentGHSeverity    string
	CurrentIsCustomer    bool
	CurrentPRs           []model.LinkedPRRef
	RecentComments       []model.PendingComment
	BaselineLabels       []string
	BaselineGHSeverity   string
	BaselineIsCustomer   bool
	BaselineOpenPRKeys   []string
	BaselineMergedPRKeys []string
	CommentWatermarkUTC  string
	TriagedAt            string
}

var (
	fencedCodeBlockRe = regexp.MustCompile("(?s)```.*?```")
	slashCmdLineRe    = regexp.MustCompile(`(?im)^\s*/(label|remove-label|cc|uncc|assign|unassign|sig|severity|type|component|retest|test|run-[a-z0-9_-]+|merge|hold|lgtm|approve|close|reopen|cherry-pick)\b.*$`)
	pivotCommentRe    = regexp.MustCompile(
		`(?i)(` +
			`\b(not\s+a\s+bug|expected\s+behavior|by\s+design|works\s+as\s+intended|` +
			`duplicate\s+of|duplicated?\s+with|already\s+fixed|fixed\s+by\s+#?\d+|` +
			`can'?t\s+reproduce|cannot\s+reproduce|unable\s+to\s+reproduce|` +
			`root\s+cause|caused\s+by|regression\s+from|introduced\s+by)\b|` +
			`非\s*bug|不是\s*bug|符合预期|重复单|已修复|无法复现|根因` +
			`)`,
	)
)

func ghSeverityRank(ghSev string) int {
	switch strings.ToLower(strings.TrimSpace(ghSev)) {
	case "critical":
		return 4
	case "major":
		return 3
	case "moderate":
		return 2
	case "minor":
		return 1
	default:
		return 0
	}
}

func cleanSubstantiveComment(body string) string {
	s := htmlCommentRe.ReplaceAllString(body, "")
	s = fencedCodeBlockRe.ReplaceAllString(s, " ")
	s = slashCmdLineRe.ReplaceAllString(s, "")

	lines := strings.Split(s, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ">") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.TrimSpace(strings.Join(kept, " "))
}

// ExtractMainlinePRKeySets returns sorted slices of verified mainline OPEN/DRAFT PR keys
// and mainline MERGED PR keys. Unverified UNKNOWN PRs are intentionally excluded from openKeys
// so that only genuinely verified OPEN -> MERGED transitions bypass pre-ack merge suppression.
func ExtractMainlinePRKeySets(prs []model.LinkedPRRef) (openKeys []string, mergedKeys []string) {
	openSet := make(map[string]bool)
	mergedSet := make(map[string]bool)
	for _, p := range prs {
		if p.IsCherryPick {
			continue
		}
		k := model.PRKey(p.Repo, p.Num)
		switch p.State {
		case "OPEN", "DRAFT":
			openSet[k] = true
		case "MERGED":
			mergedSet[k] = true
		}
	}
	for k := range openSet {
		openKeys = append(openKeys, k)
	}
	for k := range mergedSet {
		mergedKeys = append(mergedKeys, k)
	}
	sort.Strings(openKeys)
	sort.Strings(mergedKeys)
	return openKeys, mergedKeys
}

func wasUnknownPRAlreadyMergedAtAck(p model.LinkedPRRef, fallbackTriagedAt string) bool {
	ackAt := strings.TrimSpace(p.AckAt)
	if ackAt == "" {
		return false
	}
	mergedAt := strings.TrimSpace(p.MergedAt)
	if mergedAt == "" {
		return false
	}
	baseAt := ackAt
	if baseAt == "" {
		baseAt = strings.TrimSpace(fallbackTriagedAt)
	}
	return model.CompareRFC3339(mergedAt, baseAt) <= 0
}

// EvaluateRetriage compares an issue's live state against its last acknowledged baseline.
func EvaluateRetriage(in RetriageEvalInput) ([]string, []model.RetriageTrigger) {
	var (
		reasons []string
		details []model.RetriageTrigger
	)
	addTrigger := func(code, summary, actor, createdAt string) {
		reasons = append(reasons, code)
		details = append(details, model.RetriageTrigger{
			Code:      code,
			Summary:   summary,
			Actor:     actor,
			CreatedAt: createdAt,
		})
	}

	baseOpen := make(map[string]bool, len(in.BaselineOpenPRKeys))
	for _, k := range in.BaselineOpenPRKeys {
		baseOpen[k] = true
	}
	baseMerged := make(map[string]bool, len(in.BaselineMergedPRKeys))
	for _, k := range in.BaselineMergedPRKeys {
		baseMerged[k] = true
	}

	// 1. new_fix_pr & 2. fix_pr_merged_still_open
	var newOpenLabels []string
	var newMergedLabels []string
	for _, p := range in.CurrentPRs {
		if p.IsCherryPick {
			continue
		}
		k := model.PRKey(p.Repo, p.Num)
		if (p.State == "OPEN" || p.State == "DRAFT") && !baseOpen[k] && !baseMerged[k] && strings.TrimSpace(p.AckAt) == "" {
			newOpenLabels = append(newOpenLabels, p.Label)
		}
		if p.State == "MERGED" && !baseMerged[k] {
			if baseOpen[k] || !wasUnknownPRAlreadyMergedAtAck(p, in.TriagedAt) {
				newMergedLabels = append(newMergedLabels, p.Label)
			}
		}
	}
	isFilteredCat := strings.HasPrefix(in.CurrentCat, "FILTER_")
	if len(newOpenLabels) > 0 && (len(in.BaselineOpenPRKeys) == 0 || isFilteredCat) {
		addTrigger(
			model.RetriageNewFixPR,
			fmt.Sprintf("New mainline fix PR linked: %s", strings.Join(newOpenLabels, ", ")),
			"",
			"",
		)
	}
	if len(newMergedLabels) > 0 && in.CurrentCat != "FILTER_NON_OPT_OR_FIXED" {
		addTrigger(
			model.RetriageFixPRMergedOpen,
			fmt.Sprintf("Mainline fix PR merged while issue remains OPEN: %s", strings.Join(newMergedLabels, ", ")),
			"",
			"",
		)
	}

	// 3. customer_reported
	if in.CurrentIsCustomer && !in.BaselineIsCustomer {
		addTrigger(
			model.RetriageCustomerReported,
			"Added `report/customer` label since last triage",
			"",
			"",
		)
	}

	// 4. gh_severity_escalated
	curGHRank := ghSeverityRank(in.CurrentGHSeverity)
	baseGHRank := ghSeverityRank(in.BaselineGHSeverity)
	triageSevRank := model.SevRank[in.CurrentSeverity]
	if curGHRank > baseGHRank && curGHRank >= 3 && (curGHRank > triageSevRank || !model.IsRealBugCategory(in.CurrentCat)) {
		addTrigger(
			model.RetriageGHSeverityEscalated,
			fmt.Sprintf("GitHub severity escalated from `%s` to `%s` (current triage: %s/%s)",
				in.BaselineGHSeverity, in.CurrentGHSeverity, in.CurrentCat, in.CurrentSeverity),
			"",
			"",
		)
	}

	// 5. label_scope_changed
	baseLbs := make(map[string]bool, len(in.BaselineLabels))
	for _, lb := range in.BaselineLabels {
		if k := strings.ToLower(strings.TrimSpace(lb)); k != "" {
			baseLbs[k] = true
		}
	}
	curLbs := make(map[string]bool, len(in.CurrentLabels))
	for _, lb := range in.CurrentLabels {
		if k := strings.ToLower(strings.TrimSpace(lb)); k != "" {
			curLbs[k] = true
		}
	}

	var scopeChanges []string
	if baseLbs["sig/planner"] && !curLbs["sig/planner"] {
		scopeChanges = append(scopeChanges, "-sig/planner")
	}
	for _, lb := range in.CurrentLabels {
		l := strings.ToLower(strings.TrimSpace(lb))
		if l == "" || baseLbs[l] {
			continue
		}
		switch {
		case (l == "type/enhancement" || l == "type/feature-request" || l == "type/question") && model.IsRealBugCategory(in.CurrentCat):
			scopeChanges = append(scopeChanges, "+"+lb)
		case l == "status/duplicate" || l == "duplicate" || l == "wontfix" || l == "invalid" || l == "flaky-test":
			scopeChanges = append(scopeChanges, "+"+lb)
		case strings.HasPrefix(l, "sig/") && l != "sig/planner" && !curLbs["sig/planner"]:
			scopeChanges = append(scopeChanges, "+"+lb)
		}
	}
	if len(scopeChanges) > 0 {
		sort.Strings(scopeChanges)
		addTrigger(
			model.RetriageLabelScopeChanged,
			fmt.Sprintf("Scope-affecting label change: %s", strings.Join(scopeChanges, ", ")),
			"",
			"",
		)
	}

	// 6. substantive_new_comments
	for _, c := range in.RecentComments {
		if in.CommentWatermarkUTC != "" && model.CompareRFC3339(c.CreatedAt, in.CommentWatermarkUTC) <= 0 {
			continue
		}
		if prstatus.IsBotLogin(c.Author) {
			continue
		}
		cleaned := cleanSubstantiveComment(c.Body)
		if cleaned == "" {
			continue
		}
		isPivot := pivotCommentRe.MatchString(cleaned)
		isMaintainerAnalysis := !strings.EqualFold(c.Author, in.IssueAuthor) && utf8.RuneCountInString(cleaned) >= 50
		if isPivot || isMaintainerAnalysis {
			snippet := cleaned
			if runes := []rune(snippet); len(runes) > 110 {
				snippet = string(runes[:110]) + "..."
			}
			addTrigger(
				model.RetriageSubstantiveComments,
				fmt.Sprintf("@%s: %s", c.Author, snippet),
				c.Author,
				c.CreatedAt,
			)
			break
		}
	}

	return reasons, details
}
