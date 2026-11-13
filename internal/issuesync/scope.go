package issuesync

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

const DefaultUntriagedLookbackDays = 90

var KernelRepos = []string{
	"pingcap/tidb",
	"pingcap/tiflash",
	"tikv/tikv",
	"pingcap/parser",
	"tikv/pd",
	"pingcap/tiflow",
	"pingcap/ticdc",
}

var kernelRepoSet = func() map[string]bool {
	m := make(map[string]bool, len(KernelRepos))
	for _, r := range KernelRepos {
		m[r] = true
	}
	return m
}()

func IsKernelRepo(repo string) bool {
	return kernelRepoSet[strings.TrimSpace(repo)]
}

var hardNonOptComponents = map[string]bool{
	"component/br":                      true,
	"component/lightning":               true,
	"component/dumpling":                true,
	"component/ddl":                     true,
	"component/privilege":               true,
	"component/cdc":                     true,
	"component/ticdc":                   true,
	"component/tikv":                    true,
	"component/tikv-client":             true,
	"component/coprocessor":             true,
	"component/pd":                      true,
	"component/pd-client":               true,
	"component/infoschema":              true,
	"component/dxf":                     true,
	"component/disttask":                true,
	"component/import":                  true,
	"component/metrics":                 true,
	"component/util":                    true,
	"component/tiproxy":                 true,
	"component/dashboard":               true,
	"component/gc":                      true,
	"component/ttl":                     true,
	"component/pessimistic-transaction": true,
	"component/transaction":             true,
	"component/raftstore":               true,
	"component/log-backup":              true,
	"component/pitr":                    true,
	"component/resource-control":        true,
	"component/ru":                      true,
	"component/plugin":                  true,
	"component/audit":                   true,
	"component/telemetry":               true,
	"component/security":                true,
	"component/tls":                     true,
	"component/server":                  true,
	"component/protocol":                true,
	"component/slow-log":                true,
	"component/statement-summary":       true,
	"component/topsql":                  true,
}

var hardNegativeLabels = map[string]bool{
	"flaky-test":       true,
	"type/cherry-pick": true,
	"type/question":    true,
	"status/duplicate": true,
	"duplicate":        true,
	"wontfix":          true,
	"invalid":          true,
}

var optimizerEpics = map[string]string{
	"epic/cardinality-estimation": "epic_cardinality_estimation",
	"epic/cost-model":             "epic_cost_model",
	"epic/plan-cache":             "epic_plan_cache",
	"epic/sql-plan-management":    "epic_sql_plan_management",
	"epic/hint":                   "epic_hint",
	"epic/cascades":               "epic_cascades",
}

var (
	nonOptTitleGuardRe = regexp.MustCompile(
		`(?i)^(\s*\[?(br|lightning|dumpling|ticdc|cdc|tiproxy|pd|tikv|ddl|dxf|disttask|infoschema|metrics|ttl|cherry-pick|backport)\]?\s*[:\-]|` +
			`flaky\s+test\b|unstable\s+test\b|automated\s+cherry\s+pick|` +
			`(refactor|chore|ci|build|docs|test)\s*(\([a-z0-9_/-]+\))?:)`,
	)
	flakyTestTitleRe = regexp.MustCompile(`(?i)\bTest[A-Z][A-Za-z0-9_]+\b.*\b(flaky|unstable|timed?\s*out)\b`)
	dataRaceTitleRe  = regexp.MustCompile(`(?i)\bDATA\s+RACE\b`)

	optPkgPathRe = regexp.MustCompile(`(?i)\bpkg/(planner|statistics|bindinfo|util/ranger)\b`)

	strongOptTitleRe = regexp.MustCompile(
		`(?i)\b(` +
			`planner|optimizer|plan[\s_-]*cache|prepared[\s_-]*plan|non-prepared[\s_-]*plan|` +
			`explain(\s+analyze|\s+for)?|binding|spm|sql[\s_-]*plan[\s_-]*management|` +
			`analyze\s+table|auto[\s_-]*analyze|histogram|cmsketch|topn|stats[\s_-]*meta|pseudo[\s_-]*stats|` +
			`cardinality|row[\s_-]*count|cost[\s_-]*model|join[\s_-]*reorder|` +
			`index[\s_-]*merge|index[\s_-]*join|index[\s_-]*lookup|index[\s_-]*hash[\s_-]*join|` +
			`hash[\s_-]*join|merge[\s_-]*join|semi[\s_-]*join|anti[\s_-]*join|` +
			`decorrelat\w*|subquery|apply|max1row|` +
			`predicate[\s_-]*pushdown|agg[\s_-]*pushdown|topn[\s_-]*pushdown|limit[\s_-]*pushdown|push[\s_-]*down\s+to\s+tiflash|` +
			`partition[\s_-]*prun\w*|dynamic[\s_-]*prun\w*|access[\s_-]*path|skyline|ranger|range[\s_-]*detacher|` +
			`point[\s_-]*get|batch[\s_-]*point[\s_-]*get|tiflash[\s_-]*mpp|mpp[\s_-]*plan|` +
			`cascades|memo|enforcer|plan[\s_-]*replayer|opt[\s_-]*rule|` +
			`logical[\s_-]*(plan|opt\w*|join|agg\w*|apply|selection|projection|sort|topn|limit)|` +
			`physical[\s_-]*(plan|opt\w*|join|agg\w*|index|table|hash|sort|topn)|` +
			`optimizer[\s_-]*hint|hint\b|leading\s*\(|inl_join|inl_hash_join|inl_merge_join|` +
			`use_index|force_index|ignore_index|no_index_merge|agg_to_cop|read_from_storage|nth_plan|` +
			`materialized[\s_-]*view|mview|mv[\s_-]*rewrite` +
			`)\b`,
	)

	linkedOptPRTitleRe = regexp.MustCompile(`(?i)^\s*(planner|statistics|bindinfo|ranger)\s*[:(]`)

	strongOptBodyRe = regexp.MustCompile(
		`(?i)(` +
			`\bpkg/(planner|statistics|bindinfo|util/ranger)[/.]|` +
			`\b(expressionRewriter|buildPhysicalPlan|findBestTask|exhaustPhysicalPlans|deriveStats|` +
			`detachCondAndBuildRange|LogicalJoin|PhysicalHashJoin|PhysicalIndexJoin|PlanBuilder)\b|` +
			`/\*\+\s*[a-zA-Z_][a-zA-Z0-9_]*\s*\(|` +
			`\btidb_(opt_\w+|enable_prepared_plan_cache|enable_non_prepared_plan_cache|cost_model_version|` +
			`analyze_\w+|auto_analyze_\w+|partition_prune_mode|allow_mpp|enforce_mpp)\b|` +
			`\bPLAN\s+REPLAYER\b` +
			`)`,
	)

	mediumOptBugTitleRe = regexp.MustCompile(
		`(?i)(` +
			`\b(wrong|incorrect|inconsistent|unexpected)\s+result\b|` +
			`\bcan'?t\s+find\s+column\b|\bcannot\s+find\s+column\b|` +
			`\bunknown\s+column\b.*\b(order\s+by|group\s+by|having|join|subquery|lateral|view)\b|` +
			`\b(correlated|lateral\s+join|natural\s+join|semi\s+join|anti\s+join)\b|` +
			`\bouter\s+join\b.*\bnull\b|` +
			`\bonly_full_group_by\b|` +
			`\bcte\b.*\b(inline|recursive|plan|wrong|panic|error)\b` +
			`)`,
	)
)

type DiscoveryCandidateInput struct {
	Title                 string
	Body                  string
	Author                string
	CreatedAt             string
	Labels                []string
	LinkedPRTitles        []string
	Now                   time.Time
	UntriagedLookbackDays int
}

func labelSet(labels []string) map[string]bool {
	m := make(map[string]bool, len(labels))
	for _, lb := range labels {
		lb = strings.TrimSpace(lb)
		if lb != "" {
			m[lb] = true
		}
	}
	return m
}

func hasNonPlannerSig(labels []string) bool {
	for _, lb := range labels {
		l := strings.ToLower(strings.TrimSpace(lb))
		if strings.HasPrefix(l, "sig/") && l != "sig/planner" {
			return true
		}
	}
	return false
}

func hasHardNonOptComponent(labels []string) bool {
	for _, lb := range labels {
		l := strings.ToLower(strings.TrimSpace(lb))
		if hardNonOptComponents[l] {
			return true
		}
	}
	return false
}

func hasHardNegativeLabel(labels []string) bool {
	for _, lb := range labels {
		l := strings.ToLower(strings.TrimSpace(lb))
		if hardNegativeLabels[l] {
			return true
		}
	}
	return false
}

func isDiscoveryBotAuthor(author string) bool {
	a := strings.TrimSpace(author)
	if a == "" || strings.EqualFold(a, "ghost") {
		return false
	}
	return prstatus.IsBotLogin(a)
}

func matchTier1Scopes(labels []string) []string {
	lbs := labelSet(labels)
	var hits []string

	hasPlanner := lbs["sig/planner"]
	hasBug := lbs["type/bug"]
	hasEnh := lbs["type/enhancement"]
	hasFeat := lbs["type/feature-request"]
	hasSubtask := lbs["type/sub-task"]

	if hasPlanner && hasBug {
		hits = append(hits, "canonical_planner_bug")
	}
	if hasPlanner && !hasBug && !hasEnh && !hasFeat && !hasSubtask {
		hits = append(hits, "planner_no_type")
	}

	if !hasPlanner {
		isBugLike := hasBug || lbs["type/compatibility"] || lbs["type/regression"]
		for _, comp := range []string{"statistics", "bindinfo", "spm", "mview"} {
			if lbs["component/"+comp] && isBugLike {
				hits = append(hits, "comp_"+comp)
			}
		}
	}

	if !hasFeat && !hasSubtask {
		var subPlanner []string
		for lb := range lbs {
			if strings.HasPrefix(lb, "planner/") {
				suffix := strings.ReplaceAll(strings.TrimPrefix(lb, "planner/"), "-", "_")
				if suffix != "" {
					subPlanner = append(subPlanner, "planner_"+suffix)
				}
			}
			if scopeName, ok := optimizerEpics[lb]; ok {
				subPlanner = append(subPlanner, scopeName)
			}
		}
		sort.Strings(subPlanner)
		hits = append(hits, subPlanner...)
	}

	return hits
}

func passesTier2PreFilter(in DiscoveryCandidateInput) (map[string]bool, bool) {
	lbs := labelSet(in.Labels)
	if lbs["sig/planner"] ||
		lbs["type/enhancement"] ||
		lbs["type/feature-request"] ||
		lbs["type/sub-task"] {
		return nil, false
	}

	// Hard negative guards for Tier-2 untriaged / under-labeled issues
	if hasNonPlannerSig(in.Labels) ||
		hasHardNonOptComponent(in.Labels) ||
		hasHardNegativeLabel(in.Labels) ||
		isDiscoveryBotAuthor(in.Author) {
		return nil, false
	}

	if nonOptTitleGuardRe.MatchString(in.Title) || flakyTestTitleRe.MatchString(in.Title) {
		return nil, false
	}

	// Rolling lookback window applies ONLY to Tier-2 discovery
	lookbackDays := in.UntriagedLookbackDays
	if lookbackDays <= 0 {
		lookbackDays = DefaultUntriagedLookbackDays
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoffDate := now.UTC().AddDate(0, 0, -lookbackDays).Format("2006-01-02")
	createdDate := strings.TrimSpace(in.CreatedAt)
	if len(createdDate) >= 10 {
		createdDate = createdDate[:10]
	}
	if createdDate != "" && createdDate < cutoffDate {
		return nil, false
	}
	return lbs, true
}

// MatchDiscoveryScopes evaluates whether a newly seen OPEN issue should enter pending_candidates.
func MatchDiscoveryScopes(in DiscoveryCandidateInput) []string {
	if tier1 := matchTier1Scopes(in.Labels); len(tier1) > 0 {
		return tier1
	}

	lbs, ok := passesTier2PreFilter(in)
	if !ok {
		return nil
	}

	hasOptPkgInBody := optPkgPathRe.MatchString(in.Body)
	if dataRaceTitleRe.MatchString(in.Title) && !hasOptPkgInBody {
		return nil
	}

	var hits []string
	if strongOptTitleRe.MatchString(in.Title) {
		hits = append(hits, "affinity_opt_title")
	} else {
		for _, prTitle := range in.LinkedPRTitles {
			if linkedOptPRTitleRe.MatchString(prTitle) {
				hits = append(hits, "affinity_opt_pr")
				break
			}
		}
	}

	if strongOptBodyRe.MatchString(in.Body) {
		hits = append(hits, "affinity_opt_body")
	}

	if len(hits) == 0 && lbs["type/bug"] && !lbs["component/executor"] && !lbs["component/expression"] {
		if mediumOptBugTitleRe.MatchString(in.Title) {
			hits = append(hits, "affinity_sql_bug_title")
		}
	}

	return hits
}

// NeedsTier2BodyInspection reports whether an issue that did not match title/label discovery
// in lightweight search could still qualify via Tier-2 body signals (`strongOptBodyRe` or `DATA RACE` + `optPkgPathRe`).
func NeedsTier2BodyInspection(in DiscoveryCandidateInput) bool {
	if len(matchTier1Scopes(in.Labels)) > 0 {
		return false
	}
	_, ok := passesTier2PreFilter(in)
	return ok
}

// ShouldRetainExistingIssue determines whether an already-triaged open issue stays in `issues`.
// It intentionally never drops existing issues due to age or rolling lookback windows.
func ShouldRetainExistingIssue(labels []string, forceKeep bool) (bool, string) {
	return ShouldRetainTrackedIssue(labels, forceKeep, "")
}

// ShouldRetainTrackedIssue evaluates retention while honoring accepted ENHANCEMENT issues
// when `type/enhancement` or `type/feature-request` is subsequently added on GitHub.
func ShouldRetainTrackedIssue(labels []string, forceKeep bool, effectiveCat string) (bool, string) {
	if forceKeep {
		return true, ""
	}
	lbs := labelSet(labels)
	if effectiveCat == "ENHANCEMENT" {
		if hasNonPlannerSig(labels) && !lbs["sig/planner"] {
			return false, fmt.Sprintf("Reassigned to non-planner SIG (labels: %v)", labels)
		}
		isTerminalClosureLabel := lbs["type/sub-task"] ||
			lbs["status/duplicate"] ||
			lbs["duplicate"] ||
			lbs["wontfix"] ||
			lbs["invalid"] ||
			lbs["flaky-test"] ||
			(lbs["type/question"] && !lbs["sig/planner"])
		if isTerminalClosureLabel {
			return false, fmt.Sprintf("Graduated out of enhancement scope (labels: %v)", labels)
		}
		return true, ""
	}
	if lbs["sig/planner"] && lbs["type/bug"] {
		return true, ""
	}
	if lbs["type/enhancement"] || lbs["type/feature-request"] || lbs["type/sub-task"] {
		return false, fmt.Sprintf("Graduated out of bug candidate scope (labels: %v)", labels)
	}
	if len(matchTier1Scopes(labels)) > 0 {
		return true, ""
	}
	if lbs["sig/planner"] {
		return false, fmt.Sprintf("Graduated out of bug candidate scope (labels: %v)", labels)
	}
	if hasNonPlannerSig(labels) {
		return false, fmt.Sprintf("Reassigned to non-planner SIG (labels: %v)", labels)
	}
	return true, ""
}

type ExcludedIssueState struct {
	Labels      []string
	LabelsKnown bool
}

func isDisqualifiedFromReadmission(labels []string) bool {
	lbs := labelSet(labels)
	if lbs["sig/planner"] && lbs["type/bug"] {
		return false
	}
	if lbs["type/enhancement"] || lbs["type/feature-request"] || lbs["type/sub-task"] {
		return true
	}
	tier1 := matchTier1Scopes(labels)
	if lbs["sig/planner"] && !lbs["type/bug"] {
		if len(tier1) == 0 {
			return true
		}
	}
	if (hasNonPlannerSig(labels) && !lbs["sig/planner"]) || hasHardNegativeLabel(labels) {
		return true
	}
	if hasHardNonOptComponent(labels) && len(tier1) == 0 {
		return true
	}
	return false
}

// EvaluateExcludedReadmission checks whether an excluded issue should be unlocked for re-triage,
// guarding against unknown-baseline legacy exclusions (`LabelsKnown == false`) and hard-negative labels.
func EvaluateExcludedReadmission(currentLabels []string, ex ExcludedIssueState) []string {
	if isDisqualifiedFromReadmission(currentLabels) {
		return nil
	}
	curSet := labelSet(currentLabels)
	if !ex.LabelsKnown {
		if curSet["sig/planner"] && curSet["type/bug"] {
			return []string{"readmitted:+canonical_planner_bug"}
		}
		return nil
	}
	return ShouldReadmitExcluded(currentLabels, ex.Labels)
}

func isReadmitOptimizerLabel(lb string) bool {
	l := strings.ToLower(strings.TrimSpace(lb))
	if l == "sig/planner" ||
		l == "component/statistics" ||
		l == "component/bindinfo" ||
		l == "component/spm" ||
		strings.HasPrefix(l, "planner/") {
		return true
	}
	_, isOptEpic := optimizerEpics[l]
	return isOptEpic
}

// ShouldReadmitExcluded returns `readmitted:+...` scope markers if an excluded issue gained
// new high-signal optimizer labels (or transitioned into canonical `sig/planner` + `type/bug`)
// after it was excluded.
func ShouldReadmitExcluded(currentLabels, excludedLabels []string) []string {
	curSet := labelSet(currentLabels)
	exSet := labelSet(excludedLabels)

	var markers []string
	// If it newly became canonical (`sig/planner` + `type/bug`), always re-admit.
	if curSet["sig/planner"] && curSet["type/bug"] && !(exSet["sig/planner"] && exSet["type/bug"]) {
		markers = append(markers, "readmitted:+canonical_planner_bug")
	}

	var added []string
	for lb := range curSet {
		if !exSet[lb] && isReadmitOptimizerLabel(lb) {
			added = append(added, lb)
		}
	}
	sort.Strings(added)
	for _, lb := range added {
		markers = append(markers, "readmitted:+"+lb)
	}
	return markers
}
