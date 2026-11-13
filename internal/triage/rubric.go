package triage

import "github.com/winoros/tidb-triage-hub/internal/model"

type SubsystemGuide struct {
	ID           string   `json:"id"`
	NameZh       string   `json:"name_zh"`
	NameEn       string   `json:"name_en"`
	CodePrefixes []string `json:"code_prefixes"`
	Scope        string   `json:"scope"`
}

type FewShotExample struct {
	Scenario         string              `json:"scenario"`
	IssueRef         string              `json:"issue_ref"`
	Action           string              `json:"action"` // "accept" or "exclude"
	Cat              string              `json:"cat,omitempty"`
	Severity         string              `json:"severity,omitempty"`
	Subsystem        string              `json:"subsystem,omitempty"`
	IsBorderlineKept bool                `json:"is_borderline_kept,omitempty"`
	ExcludeReason    string              `json:"exclude_reason,omitempty"`
	Note             *model.LocalizedNote `json:"note,omitempty"`
}

type Rubric struct {
	DecisionSteps   []string          `json:"decision_steps"`
	BoundaryRules   map[string]string `json:"boundary_rules"`
	Subsystems      []SubsystemGuide  `json:"subsystems"`
	DifficultyGuide map[string]string `json:"difficulty_guide"`
	SeverityGuide   map[string]string `json:"severity_guide"`
	NoteStyleGuide  map[string]string `json:"note_style_guide"`
	FewShotExamples []FewShotExample  `json:"few_shot_examples,omitempty"`
}

func DefaultRubric(compact bool) Rubric {
	r := Rubric{
		DecisionSteps: []string{
			"Step 1 (Scope & Canonical Check): Check `is_canonical` (`sig/planner` + `type/bug`). Canonical issues MUST NEVER be put in `exclude_issues` (use `FILTER_*` instead to preserve 1:1 GitHub canonical parity). Non-canonical supplemental issues outside optimizer scope MUST go to `exclude_issues`.",
			"Step 2 (Noise vs Real Bug vs Enhancement): Distinguish actionable bugs from `FILTER_RANDOM_FUZZ` (SQLancer/TLP/NoREC obscure nested expressions), `FILTER_AI_OBSCURE` (unreachable static-audit edge cases or off-by-default flags), `FILTER_NON_OPT_OR_FIXED` (already fixed on master, flaky unit test, or non-optimizer module on a canonical issue), and `ENHANCEMENT` (correct result, feature/rule/cost improvement request).",
			"Step 3 (Borderline Flag): Set `is_borderline_kept = true` ONLY on real bugs (`EASY`/`MEDIUM`/`HARD`) that were discovered via fuzzing or AI/code audit yet trigger on realistic SQL with wrong results, panic, deadlock, or missing locks.",
			"Step 4 (Subsystem & Difficulty): Map root cause (using `pr_details[].non_test_files` and `clues.code_paths` first) to one of the 7 subsystems, then grade engineering effort (`EASY` 1-3d single-point guard, `MEDIUM` 3-7d cross-stage invariant, `HARD` 1-3w+ architectural/CBO/MPP/concurrency).",
			"Step 5 (Severity & Bilingual Note): Set `severity` (`N/A` for `ENHANCEMENT`/`FILTER_*`; `CRITICAL`/`MAJOR`/`MODERATE`/`MINOR` for real bugs) and write concise root-cause + symptom bilingual notes (`zh-CN` & `en`) wrapping code/SQL identifiers in paired backticks.",
		},
		BoundaryRules: map[string]string{
			"canonical_issues":    "Has BOTH `sig/planner` AND `type/bug`. Always keep in `new_issues` (never `exclude_issues`). Use `FILTER_RANDOM_FUZZ`, `FILTER_AI_OBSCURE`, or `FILTER_NON_OPT_OR_FIXED` when noisy or non-optimizer.",
			"supplemental_issues": "Lacks `sig/planner` + `type/bug`. Accept ONLY genuine optimizer bugs (`EASY`/`MEDIUM`/`HARD`) or core optimizer `ENHANCEMENT`s. Put executor/expression/tikv/DDL/UI/flaky/fuzz noise into `exclude_issues` so their PRs never pollute the PR board.",
			"is_borderline_kept":  "True ONLY when `cat` is `EASY`, `MEDIUM`, or `HARD` AND origin is fuzzer/SQLancer/AI-audit, kept because trigger SQL is realistic and impact is severe (wrong result / panic / missing lock).",
		},
		Subsystems: []SubsystemGuide{
			{
				ID:           "statistics",
				NameZh:       "统计信息与采样维护",
				NameEn:       "Statistics & ANALYZE",
				CodePrefixes: []string{"pkg/statistics/", "pkg/executor/analyze"},
				Scope:        "ANALYZE, auto-analyze, histogram/CMSketch/TopN, stats meta/cache/sync load, Global Stats merge, pseudo stats.",
			},
			{
				ID:           "plan_cache_spm_hint",
				NameZh:       "Plan Cache / SPM / Hint",
				NameEn:       "Plan Cache, SPM & Hints",
				CodePrefixes: []string{"pkg/bindinfo/", "pkg/planner/core/plan_cache"},
				Scope:        "Prepared/Non-Prepared Plan Cache key/param guard, SQL Plan Management (Bindings), optimizer hints (`/*+ ... */`).",
			},
			{
				ID:           "logical_rewrite",
				NameZh:       "逻辑改写与等价变换",
				NameEn:       "Logical Rewrite & Transformation",
				CodePrefixes: []string{"pkg/planner/core/rule_", "pkg/planner/core/expression_rewriter"},
				Scope:        "Predicate pushdown, column pruning, subquery decorrelation/Apply, outer-to-inner join, constant propagation, CTE/MView rewrite.",
			},
			{
				ID:           "access_path_partition_lock",
				NameZh:       "索引路径 / Ranger / 分区裁剪与加锁",
				NameEn:       "Access Path, Ranger, Partition Pruning & Lock",
				CodePrefixes: []string{"pkg/util/ranger/", "pkg/planner/core/find_best_task", "pkg/planner/core/point_get_plan", "pkg/planner/core/part_"},
				Scope:        "Ranger range detachment, PointGet/BatchPointGet, IndexMerge, skyline pruning, static/dynamic partition pruning, pessimistic lock pushdown.",
			},
			{
				ID:           "cbo_cost_join_reorder",
				NameZh:       "代价模型 / 基数估算 / 连接重排",
				NameEn:       "CBO Cost, Cardinality & Join Reorder",
				CodePrefixes: []string{"pkg/planner/cardinality/", "pkg/planner/core/cbo", "pkg/planner/core/rule_join_reorder", "pkg/planner/cascades/"},
				Scope:        "Row-count/NDV/selectivity estimation, cost formulas, Join Reorder DP/greedy, physical join selection, Cascades/Memo.",
			},
			{
				ID:           "name_resolution_group_by",
				NameZh:       "名称解析 / 聚合语义与视图展开",
				NameEn:       "Name Resolution, Aggregation & View/Window",
				CodePrefixes: []string{"pkg/planner/core/logical_plan_builder", "pkg/planner/core/resolve"},
				Scope:        "PlanBuilder single-pass column/alias resolution, ONLY_FULL_GROUP_BY, GROUPING SETS/ROLLUP, window/view/derived-table schema.",
			},
			{
				ID:           "tiflash_mpp",
				NameZh:       "TiFlash / MPP 分布式计划",
				NameEn:       "TiFlash & MPP Distributed Plan",
				CodePrefixes: []string{"pkg/planner/core/mpp", "pingcap/tiflash"},
				Scope:        "MPP task generation, Exchange sender/receiver enforcement, broadcast/hash shuffle, TiFlash pushdown compatibility.",
			},
		},
		DifficultyGuide: map[string]string{
			"EASY":   "1-3 days: Clear root cause, localized guard/key/type fix in 1-2 source files (~10-60 non-test lines) without cross-stage contract changes.",
			"MEDIUM": "3-7 days: Multi-function or adjacent-stage invariant coordination (e.g., Ranger residual filter retention, schema mapping across rewrite + pruning, Global Stats merge).",
			"HARD":   "1-3+ weeks: Cross-package architecture, CBO/cardinality tradeoffs risking plan regressions, ANALYZE concurrency/memory governance, MPP protocol, or single-pass name-resolution debt.",
		},
		SeverityGuide: map[string]string{
			"CRITICAL": "S0: Panic/crash, instance OOM, permanent background worker exit, cluster GC block, DML wrong data mutation, silent missing `FOR UPDATE` lock, privilege bypass.",
			"MAJOR":    "S1: Read `SELECT` wrong result, valid common SQL failing with `ERROR 1105` / column-not-found, stats system-table deadlock, or order-of-magnitude production plan regression.",
			"MODERATE": "S2: Moderate cardinality/cost skew (correct result, suboptimal plan without outage), Plan Cache/SPM miss, hint export mismatch, narrow MySQL compatibility gap.",
			"MINOR":    "S3: Hint warning text, `EXPLAIN` / `SHOW STATS` / slow-log formatting, minor diagnostic polish.",
			"N/A":      "Required whenever `cat` is `ENHANCEMENT` or `FILTER_*`.",
		},
		NoteStyleGuide: map[string]string{
			"formula":    "[Origin tag if borderline] + `<Component/Func>` + <specific flaw> + <user-visible symptom>",
			"length":     "zh-CN: 25-85 Chinese characters; en: 50-170 English characters. Concise, dense, single sentence.",
			"formatting": "Wrap Go functions, structs, SQL clauses, hints, and sysvars in paired backticks (e.g. `detachCNFCondAndBuildRangeForIndex`, `INL_JOIN`, `ERROR 1105`). Never leave `TODO:`.",
		},
	}

	if !compact {
		r.FewShotExamples = []FewShotExample{
			{
				Scenario:  "Real Bug (MEDIUM / MAJOR) with exact Ranger function root cause",
				IssueRef:  "#73063",
				Action:    "accept",
				Cat:       "MEDIUM",
				Severity:  "MAJOR",
				Subsystem: "access_path_partition_lock",
				Note: &model.LocalizedNote{
					ZhCN: "Ranger `detachCNFCondAndBuildRangeForIndex` 在 `eqOrInCount==0` 替换复合前缀索引区间时漏保留残余 `OR` 过滤条件导致错结果",
					En:   "Ranger `detachCNFCondAndBuildRangeForIndex` drops residual `OR` filter on composite prefix index when `eqOrInCount==0`, causing wrong results",
				},
			},
			{
				Scenario:         "Borderline Kept (EASY / MAJOR): Code-audit found but triggers on default plan",
				IssueRef:         "#73031",
				Action:           "accept",
				Cat:              "EASY",
				Severity:         "MAJOR",
				Subsystem:        "cbo_cost_join_reorder",
				IsBorderlineKept: true,
				Note: &model.LocalizedNote{
					ZhCN: "源码审计发现但默认触发：`ENUM`/`SET` 外键与 `_ci` 字符串内表走默认 `IndexHashJoin` 时跳过排序规则检查导致错结果",
					En:   "Code-audit found (default plan): `IndexHashJoin` skips collation check for `ENUM`/`SET` outer key joining `_ci` string inner column",
				},
			},
			{
				Scenario:         "Borderline Kept (MEDIUM / CRITICAL): Fuzz-reduced SQL causing panic on empty table",
				IssueRef:         "#67451",
				Action:           "accept",
				Cat:              "MEDIUM",
				Severity:         "CRITICAL",
				Subsystem:        "logical_rewrite",
				IsBorderlineKept: true,
				Note: &model.LocalizedNote{
					ZhCN: "Fuzz 约简但空表常规可达：关联 `HAVING expr IN (SELECT ... GROUP BY 1)` 重写时列下标越界触发 `panic`",
					En:   "Fuzz-reduced (reproduces on empty table): Correlated `HAVING expr IN (SELECT ... GROUP BY 1)` triggers index-out-of-range `panic`",
				},
			},
			{
				Scenario:         "Real Bug (HARD / MODERATE): Architectural name resolution gap",
				IssueRef:         "#67540",
				Action:           "accept",
				Cat:              "HARD",
				Severity:         "MODERATE",
				Subsystem:        "name_resolution_group_by",
				IsBorderlineKept: true,
				Note: &model.LocalizedNote{
					ZhCN: "外部贡献简化：关联标量子查询在单遍名称解析阶段绕过 `ONLY_FULL_GROUP_BY` 聚合列校验",
					En:   "Reduced external report: Correlated scalar subquery bypasses `ONLY_FULL_GROUP_BY` validation during single-pass name resolution",
				},
			},
			{
				Scenario:  "Canonical Fuzzer Noise (`sig/planner` + `type/bug` -> keep as `FILTER_RANDOM_FUZZ`, never exclude)",
				IssueRef:  "#71200 (archetype)",
				Action:    "accept",
				Cat:       "FILTER_RANDOM_FUZZ",
				Severity:  "N/A",
				Subsystem: "logical_rewrite",
				Note: &model.LocalizedNote{
					ZhCN: "SQLancer 差分测试多层嵌套类型强转与冷门位运算组合边界，常规业务 SQL 不可达",
					En:   "SQLancer differential fuzzing case combining deeply nested casts and obscure bitwise ops; unreachable in realistic SQL",
				},
			},
			{
				Scenario:  "Canonical Flaky Test (`sig/planner` + `type/bug` -> `FILTER_NON_OPT_OR_FIXED`)",
				IssueRef:  "#39453",
				Action:    "accept",
				Cat:       "FILTER_NON_OPT_OR_FIXED",
				Severity:  "N/A",
				Subsystem: "plan_cache_spm_hint",
				Note: &model.LocalizedNote{
					ZhCN: "单测 Flaky 数据竞争 (`bindinfo.TestCaptureUserFilter`)，非生产路径缺陷",
					En:   "Flaky unit test data race (`bindinfo.TestCaptureUserFilter`), not a production optimizer bug",
				},
			},
			{
				Scenario:  "Optimizer Enhancement (`ENHANCEMENT` -> `severity: N/A`)",
				IssueRef:  "#72500 (archetype)",
				Action:    "accept",
				Cat:       "ENHANCEMENT",
				Severity:  "N/A",
				Subsystem: "cbo_cost_join_reorder",
				Note: &model.LocalizedNote{
					ZhCN: "诉求为多表星型连接补充基于外键约束的冗余连接消除规则（结果正确，属优化器能力增强）",
					En:   "Feature request to eliminate redundant joins using foreign-key constraints in star-schema queries (results are correct)",
				},
			},
			{
				Scenario:      "Non-Canonical Non-Optimizer Issue -> `exclude_issues`",
				IssueRef:      "#73100 (archetype)",
				Action:        "exclude",
				ExcludeReason: "Vectorized builtin cast bug in pkg/expression without optimizer plan involvement",
			},
		}
	}

	return r
}
