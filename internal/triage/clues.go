package triage

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

var (
	codePathRe  = regexp.MustCompile(`\b(?:pkg|dbms|components)/[A-Za-z0-9_./-]+\.(?:go|h|cpp|cc|rs)\b|\bpkg/[A-Za-z0-9_/-]+\b`)
	stackFuncRe = regexp.MustCompile(`(?:\(\*[A-Za-z0-9_]+\)\.[A-Za-z0-9_]+|\b[a-z][A-Za-z0-9_]*(?:Plan|Stats|Range|Join|Rewrite|Prun|Bind|Cache|Hint|Agg|Column)[A-Za-z0-9_]*\b)`)
	backtickSym = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.*]{3,60})`")

	errSigPatterns = []struct {
		tag string
		re  *regexp.Regexp
	}{
		{"panic", regexp.MustCompile(`(?i)\b(panic:|runtime error:|nil pointer dereference|index out of range|slice bounds out of range)\b`)},
		{"wrong_result", regexp.MustCompile(`(?i)\b(wrong\s+result|incorrect\s+result|inconsistent\s+result|unexpected\s+result|missing\s+row|extra\s+row|empty\s+set)\b`)},
		{"error_1105", regexp.MustCompile(`(?i)\b(ERROR\s+1105|HY000.*unknown\s+error|internal\s+error|Can'?t\s+find\s+column)\b`)},
		{"oom", regexp.MustCompile(`(?i)\b(out\s+of\s+memory|OOM|memory\s+exceed|memory\s+leak)\b`)},
		{"deadlock_or_hang", regexp.MustCompile(`(?i)\b(deadlock|hangs?\b|stuck\b|infinite\s+loop|lock\s+wait\s+timeout)\b`)},
		{"missing_lock", regexp.MustCompile(`(?i)\b(for\s+update.*lock|missing\s+lock|pessimistic\s+lock)\b`)},
		{"plan_regression", regexp.MustCompile(`(?i)\b(plan\s+regression|slow\s+query|suboptimal\s+plan|wrong\s+index|full\s+table\s+scan)\b`)},
		{"data_race", regexp.MustCompile(`(?i)\bDATA\s+RACE\b`)},
	}

	sqlFeaturePatterns = []struct {
		tag string
		re  *regexp.Regexp
	}{
		{"plan_cache", regexp.MustCompile(`(?i)\b(plan[\s_-]*cache|prepare\b|execute\b)\b`)},
		{"spm_binding", regexp.MustCompile(`(?i)\b(create\s+(global|session)?\s*binding|bindinfo|spm\b)\b`)},
		{"optimizer_hint", regexp.MustCompile(`(?i)(/\*\+\s*[A-Za-z0-9_]+|\b(INL_JOIN|HASH_JOIN|MERGE_JOIN|USE_INDEX|FORCE_INDEX|LEADING|READ_FROM_STORAGE|SEMI_JOIN_REWRITE|NO_DECORRELATE)\b)`)},
		{"partition_pruning", regexp.MustCompile(`(?i)\b(partition\s+by|partition[\s_-]*prun\w*|tidb_partition_prune_mode)\b`)},
		{"analyze_stats", regexp.MustCompile(`(?i)\b(analyze\s+table|auto[\s_-]*analyze|histogram|cmsketch|topn|stats_meta)\b`)},
		{"subquery_apply", regexp.MustCompile(`(?i)\b(correlated|exists\s*\(|in\s*\(\s*select|LogicalApply|PhysicalApply|decorrelat\w*)\b`)},
		{"cte", regexp.MustCompile(`(?i)\b(with\s+recursive|\bCTE\b|CTEFullScan)\b`)},
		{"window_func", regexp.MustCompile(`(?i)\b(over\s*\(|row_number|first_value|last_value|rank\s*\()\b`)},
		{"tiflash_mpp", regexp.MustCompile(`(?i)\b(tiflash|mpp\b|ExchangeSender|ExchangeReceiver)\b`)},
		{"index_merge", regexp.MustCompile(`(?i)\b(index[\s_-]*merge|IndexMerge)\b`)},
		{"materialized_view", regexp.MustCompile(`(?i)\b(materialized\s+view|mview)\b`)},
	}

	fuzzSignalRe    = regexp.MustCompile(`(?i)\b(sqlancer|tlp\b|norec\b|pqs\b|cert\b|fuzzer|fuzzing|randgen|differential\s+testing)\b`)
	aiAuditSignalRe = regexp.MustCompile(`(?i)\b(code[\s_-]*audit|static[\s_-]*audit|ai[\s_-]*found|llm\b)\b`)
)

func ExtractClues(
	title string,
	body string,
	labels []string,
	comments []model.PendingComment,
	prDetails []model.PendingPRDetail,
) *model.PendingClues {
	hasActiveOrMerged := false
	for _, pd := range prDetails {
		if !pd.IsCherryPick && pd.State != "CLOSED" {
			hasActiveOrMerged = true
			break
		}
	}

	var sb strings.Builder
	sb.WriteString(title)
	sb.WriteByte('\n')
	sb.WriteString(body)
	for _, c := range comments {
		sb.WriteByte('\n')
		sb.WriteString(c.Body)
	}
	for _, pd := range prDetails {
		if pd.IsCherryPick || (hasActiveOrMerged && pd.State == "CLOSED") {
			continue
		}
		sb.WriteByte('\n')
		sb.WriteString(pd.Title)
		sb.WriteByte('\n')
		sb.WriteString(pd.BodyExcerpt)
	}
	combined := sb.String()

	pathSet := make(map[string]bool)
	pkgSet := make(map[string]bool)
	for _, pd := range prDetails {
		if pd.IsCherryPick || (hasActiveOrMerged && pd.State == "CLOSED") {
			continue
		}
		for _, f := range pd.NonTestFiles {
			pathSet[f] = true
			dir := path.Dir(f)
			if dir != "." && dir != "" {
				pkgSet[dir] = true
			}
		}
	}
	for _, m := range codePathRe.FindAllString(combined, 30) {
		if !isTestOrFixtureFile(m) {
			pathSet[m] = true
			if strings.HasSuffix(m, ".go") {
				pkgSet[path.Dir(m)] = true
			} else {
				pkgSet[m] = true
			}
		}
	}

	funcSet := make(map[string]bool)
	for _, m := range stackFuncRe.FindAllString(combined, 25) {
		funcSet[m] = true
	}
	for _, m := range backtickSym.FindAllStringSubmatch(combined, 25) {
		if len(m) >= 2 && !strings.Contains(m[1], "/") {
			sym := m[1]
			if strings.ToUpper(sym) != sym { // skip pure SQL keywords like SELECT
				funcSet[sym] = true
			}
		}
	}

	var errSigs []string
	for _, p := range errSigPatterns {
		if p.re.MatchString(combined) {
			errSigs = append(errSigs, p.tag)
		}
	}

	var sqlFeats []string
	for _, p := range sqlFeaturePatterns {
		if p.re.MatchString(combined) {
			sqlFeats = append(sqlFeats, p.tag)
		}
	}

	var origins []string
	if fuzzSignalRe.MatchString(combined) {
		origins = append(origins, "fuzzer_or_sqlancer")
	}
	if aiAuditSignalRe.MatchString(combined) {
		origins = append(origins, "code_or_ai_audit")
	}
	for _, lb := range labels {
		if lb == "report/customer" {
			origins = append(origins, "customer_reported")
		}
	}

	return &model.PendingClues{
		CodePaths:       sortedKeysCap(pathSet, 12),
		TouchedPackages: sortedKeysCap(pkgSet, 8),
		Functions:       sortedKeysCap(funcSet, 10),
		ErrorSignatures: errSigs,
		SQLFeatures:     sqlFeats,
		OriginSignals:   origins,
	}
}

func sortedKeysCap(m map[string]bool, maxItems int) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if maxItems > 0 && len(out) > maxItems {
		out = out[:maxItems]
	}
	return out
}
