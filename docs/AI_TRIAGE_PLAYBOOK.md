# TiDB Optimizer Issue Triage & Dynamic Re-Triage Playbook

Reference for pool boundaries, classification taxonomy, issue discovery and retention, dynamic re-triage triggers, PR blocker stages, and optimistic versioning in **TiDB Triage Hub**.

## 1. Canonical vs. Supplemental Pool Boundary

| Pool | Condition | How Noise Is Handled | PR Status Board Impact |
| :--- | :--- | :--- | :--- |
| **Canonical** | Has both `sig/planner` and `type/bug` | Kept in `issues` under `FILTER_*` (`FILTER_RANDOM_FUZZ`, `FILTER_AI_OBSCURE`, or `FILTER_NON_OPT_OR_FIXED`; never excluded, preserving 1:1 parity with GitHub) | Open PRs linked *solely* to `FILTER_NON_OPT_OR_FIXED` issues stay off the PR Status Board (unless pinned via `sync-prs --pr`) and stop recurring PR polling once resolved |
| **Supplemental** | Lacks `sig/planner` + `type/bug` | Moved to `excluded_issues` (`triage --exclude` or UI **Exclude**) | Removed from `issues` and `issue_pr_links` |

## 2. Classification Taxonomy

### Categories (`cat`)

| Category | Meaning | Severity Requirement |
| :--- | :--- | :--- |
| `EASY` | Localized guard/key/type fix in 1–2 files (~1–3 days) with no cross-stage contract changes | `CRITICAL` .. `MINOR` |
| `MEDIUM` | Multi-function or adjacent-stage invariant coordination (~3–7 days; e.g., Ranger residual filter, rewrite + pruning schema, Global Stats merge) | `CRITICAL` .. `MINOR` |
| `HARD` | Architectural change, CBO/cardinality tradeoff (even `<=55`-line diffs with regression risk), Join Reorder, name resolution, `ANALYZE` concurrency, or MPP (~1–3+ weeks) | `CRITICAL` .. `MINOR` |
| `ENHANCEMENT` | Feature request, new rewrite rule, or cost improvement where current query results are correct | `N/A` |
| `FILTER_RANDOM_FUZZ` | Unreachable fuzzer / SQLancer edge case on a canonical issue | `N/A` |
| `FILTER_AI_OBSCURE` | Unreachable static/AI code-audit edge case or disabled-by-default flag combination on a canonical issue | `N/A` |
| `FILTER_NON_OPT_OR_FIXED` | Already fixed on `master` and awaiting closure (any tracked issue), or flaky unit test / non-optimizer root cause on a canonical issue | `N/A` |

Set `is_borderline_kept: true` only for real bugs (`EASY`, `MEDIUM`, `HARD`) found by fuzzing/SQLancer or static/AI audit whose trigger SQL is realistic and causes wrong results, panics, deadlocks, or missing locks.

### Severities (`severity`)

| Severity | Criteria |
| :--- | :--- |
| `CRITICAL` | Panic/crash, instance OOM, permanent worker exit, cluster GC block, DML data corruption, missing `FOR UPDATE` lock, privilege bypass, or easily triggered `SELECT` wrong result |
| `MAJOR` | Realistic `SELECT` wrong result requiring specific non-trivial conditions, valid common SQL failing with `ERROR 1105` / column-not-found, stats meta deadlock, or order-of-magnitude production plan regression |
| `MODERATE` | Hard-to-hit borderline wrong result, moderate cardinality/cost skew without outage, Plan Cache / SPM miss, hint export mismatch, or narrow MySQL compatibility gap |
| `MINOR` | Hint warning text, `EXPLAIN` / `SHOW STATS` / slow-log formatting, or minor diagnostic polish |
| `N/A` | Mandatory when `cat` is `ENHANCEMENT` or `FILTER_*` |

### Subsystems (`subsystem`)

| Subsystem ID | Scope | Key Code Prefixes |
| :--- | :--- | :--- |
| `statistics` | `ANALYZE`, auto-analyze, histograms/CMSketch/TopN, stats cache/meta/sync load, Global Stats, pseudo stats | `pkg/statistics/`, `pkg/executor/analyze` |
| `plan_cache_spm_hint` | Prepared/Non-Prepared Plan Cache keys & guards, SQL Plan Management (bindings), optimizer hints | `pkg/bindinfo/`, `pkg/planner/core/plan_cache` |
| `logical_rewrite` | Predicate pushdown, column pruning, subquery decorrelation/`Apply`, outer-to-inner join, constant propagation, CTE/MView rewrite | `pkg/planner/core/rule_`, `pkg/planner/core/expression_rewriter` |
| `access_path_partition_lock` | Ranger range building, `PointGet`/`BatchPointGet`, `IndexMerge`, skyline pruning, static/dynamic partition pruning, pessimistic lock pushdown | `pkg/util/ranger/`, `pkg/planner/core/find_best_task`, `pkg/planner/core/point_get_plan`, `pkg/planner/core/part_` |
| `cbo_cost_join_reorder` | Row-count/NDV/selectivity estimation, cost formulas, Join Reorder, physical join selection, Cascades/Memo | `pkg/planner/cardinality/`, `pkg/planner/core/cbo`, `pkg/planner/core/rule_join_reorder`, `pkg/planner/cascades/` |
| `name_resolution_group_by` | `PlanBuilder` single-pass column/alias resolution, `ONLY_FULL_GROUP_BY`, `GROUPING SETS`/`ROLLUP`, window/view/derived-table schema | `pkg/planner/core/logical_plan_builder`, `pkg/planner/core/resolve` |
| `tiflash_mpp` | MPP task generation, `Exchange` enforcement, broadcast/hash shuffle, TiFlash pushdown compatibility | `pkg/planner/core/mpp`, `pingcap/tiflash` |

## 3. Issue Discovery, Retention & Re-Admission

Issue discovery (`internal/issuesync/scope.go`) separates untriaged candidate intake from the retention of already-triaged issues.

### Tier 1: Label-Based Discovery

- **`canonical_planner_bug`**: `sig/planner` + `type/bug`
- **`planner_no_type`**: `sig/planner` without `type/{bug,enhancement,feature-request,sub-task}`
- **`comp_{statistics,bindinfo,spm,mview}`**: lacks `sig/planner`, paired with `type/{bug,compatibility,regression}`
- **`planner_<subscope>` & `epic_<name>`**: any `planner/*` label or `epic/{cardinality-estimation,cost-model,plan-cache,sql-plan-management,hint,cascades}`, when `type/feature-request` and `type/sub-task` are absent

Tier-1 matching enforces no creation-age cutoff. When run without `--since`, `sync-issues --full` backfills all open history for `sig/planner` and the four optimizer `component/*` labels, while scanning standalone `planner/*` and `epic/*` issues within the `--untriaged-lookback-days` `updatedAt` window.

### Tier 2: Signal-Gated Discovery (`createdAt` Within `--untriaged-lookback-days`, Default 90d)

- **Hard exclusions**: `sig/planner`, `type/{enhancement,feature-request,sub-task}`, any non-planner `sig/*`, 35+ non-optimizer `component/*` labels, negative labels (`flaky-test`, `type/cherry-pick`, `type/question`, `duplicate`, `status/duplicate`, `wontfix`, `invalid`), bot authors (excluding `@ghost`), non-optimizer title prefixes, flaky-test titles, and `DATA RACE` titles without optimizer package paths in the body.
- **Matched scopes**:
  - **`affinity_opt_title`**: optimizer keywords in the issue title.
  - **`affinity_opt_pr`**: linked PR title starting with `planner`, `statistics`, `bindinfo`, or `ranger` (`:` or `(`), checked when `affinity_opt_title` did not match.
  - **`affinity_opt_body`**: optimizer package paths, planner symbols, optimizer hints, `tidb_*` optimizer/analyze sysvars, or `PLAN REPLAYER` in the issue body.
  - **`affinity_sql_bug_title`**: plan-sensitive SQL bug title patterns when none of the above matched, `type/bug` is present, and `component/{executor,expression}` are absent.

### Retention, Graduation & Re-Admission

- **Closed-issue archival**: Triaged issues never age out due to lookback windows. Closing an issue on GitHub moves its classification baseline to `archived_issues`; if the issue reopens while non-excluded and still meets retention rules, that baseline restores automatically.
- **Open-issue graduation**: Setting `force_keep: true` (in `new_issues` / `overrides` JSON or the Web UI drawer) pins an open issue in `issues`. Otherwise, `sync-issues` graduates a still-open tracked issue to `archived_issues` under these rules:
  - **`ENHANCEMENT` issues**: archived if they carry `type/sub-task`, `duplicate`, `status/duplicate`, `wontfix`, `invalid`, or `flaky-test`, or carry `type/question` or a non-planner `sig/*` when `sig/planner` is absent.
  - **Other tracked issues**: archived when non-canonical (lacking `sig/planner` + `type/bug`) and carrying either `type/{enhancement,feature-request,sub-task}` or a non-planner `sig/*` without any Tier-1 scope. Pass `force_keep: true` when recalibrating an open `type/enhancement` issue to `FILTER_NON_OPT_OR_FIXED` so it remains in `issues`.
- **Label-delta re-admission**: Excluded issues record `excluded_labels_json` at exclusion time.
  - Newly becoming canonical (`sig/planner` + `type/bug`) **always** re-admits the issue into `pending_candidates` (`readmitted:+canonical_planner_bug`), overriding all other labels and legacy unknown-label exclusions.
  - Otherwise, newly gaining `sig/planner`, `component/{statistics,bindinfo,spm}`, `planner/*`, or an optimizer `epic/*` re-admits the issue (`readmitted:+<label>`) when baseline labels are known and the issue is not blocked by `type/{enhancement,feature-request,sub-task}`, negative labels, a non-planner `sig/*` without `sig/planner`, or a non-optimizer `component/*` without a Tier-1 scope.

## 4. Dynamic Re-Triage Lifecycle

Each tracked issue stores an acknowledged baseline in `issue_triage_state`. Re-triage evaluation (run during sync, PR detail refresh, and override reset) sets `needs_retriage = 1` only when one of six transitions occurs relative to that baseline:

| Trigger Code | Firing Condition |
| :--- | :--- |
| `new_fix_pr` | Any `FILTER_*` issue, or a tracked issue with no open mainline PR in its baseline, gains a new mainline `OPEN` or `DRAFT` fix PR |
| `fix_pr_merged_still_open` | Mainline fix PR transitions to `MERGED` while the issue remains `OPEN` (unless already `FILTER_NON_OPT_OR_FIXED`) |
| `customer_reported` | `report/customer` label is newly added |
| `gh_severity_escalated` | GitHub `severity/*` escalates above baseline to `major` or `critical`, and either exceeds current triaged severity or current `cat` is not a real bug |
| `label_scope_changed` | `-sig/planner`; `+type/{enhancement,feature-request,question}` on a retained real bug; `+status/duplicate`, `+duplicate`, `+wontfix`, `+invalid`, or `+flaky-test`; or `+sig/<non-planner>` when `sig/planner` is absent |
| `substantive_new_comments` | Post-watermark human comment within the latest 15 comments (stripping bots, HTML comments, code blocks, quotes, and Prow slash commands) hits a root-cause / not-a-bug / duplicate / already-fixed / cannot-reproduce / regression pivot or ≥50 chars of non-author technical analysis |

Flagged issues are resolved through one of three actions:

- **Keep Current**: Acknowledge without changing classification (Web **Keep Current**, `POST /api/issues/{number}/retriage/ack`, or CLI `triage --ack-retriage <nums>` / `"ack_retriage"`), such as when a bug stays open only for `release-*` cherry-picks.
- **Recalibrate**: Update category, severity, subsystem, or note via the Web Calibration Drawer or CLI `"overrides"` (for example, setting `FILTER_NON_OPT_OR_FIXED` once a bug's fix PR merges on `master` and only issue closure remains).
- **Exclude**: Move a non-canonical (`!is_canonical`) issue to `"exclude_issues"` (`triage --exclude <num>`) when `label_scope_changed` removes `sig/planner`, or when `new_fix_pr` or `substantive_new_comments` confirms a non-optimizer root cause or unreachable fuzz case. When exported PR file lists exceed 30 files (`pr_details[].files_truncated: true`, see §6), inspect the full PR diff before excluding.

## 5. PR Blocker Classification

Tracked open PRs (both mainline and `release-*` cherry-picks in `internal/prstatus/classify.go`) are evaluated in strict precedence order:

| Priority | Blocker Stage | Ball Owner | Condition |
| :---: | :--- | :--- | :--- |
| 1 | `DRAFT_OR_HOLD` | `HOLD` | Draft PR or carries `do-not-merge/hold`, `do-not-merge/work-in-progress`, or `status/wip` |
| 2 | `CONFLICT` | `AUTHOR` | GitHub mergeability is `CONFLICTING` |
| 3 | `NEEDS_OK_TO_TEST` | `REVIEWER` | Carries `needs-ok-to-test` |
| 4 | `CI_FAILED` | `AUTHOR` | Any non-ignored CI check failed, errored, timed out, required action, failed startup, or was cancelled (`license/cla` and `check-issue-triage-complete` are evaluated at Priority 6 instead) |
| 5 | `WAITING_AUTHOR` | `AUTHOR` | Active human `CHANGES_REQUESTED` review at/after latest commit, or non-outdated unresolved thread whose latest comment is by a human reviewer at/after latest commit |
| 6 | `GATE_BLOCKED` | `AUTHOR` | Has `lgtm` / `status/lgt1` / `status/lgt2` / `approved` label, human GitHub `APPROVED` review, or `/approve` comment, and is blocked by `do-not-merge/*`, `needs-rebase`, non-`SUCCESS` `license/cla`, or failed `check-issue-triage-complete` |
| 7 | `NEEDS_LGTM2` | `REVIEWER` | Carries `lgtm` / `status/lgt1` / `status/lgt2` without `approved`, or has `tide` context with one human LGTM (`APPROVED` review or `/lgtm` / `/approve` comment) and no LGTM/`approved` labels |
| 8 | `WAITING_REVIEWER` | `REVIEWER` | Not yet fully approved (`tide` PRs require `approved` + LGTM label; non-`tide` PRs require `approved` or human `APPROVED` / `/approve` without active `CHANGES_REQUESTED`) |
| 9 | `READY_MERGING` | `MERGE_QUEUE` | Fully approved with no failing checks (CI running or queued to merge) |

## 6. State & Version Model

### PR Metadata & Enrichment Cache

- **Single source of PR facts**: `pull_requests` owns PR state, commit OIDs, and blocker assessments (`MERGED` is terminal); `issue_pr_links` and `pending` bundles project from it. When PR metadata updates without a full blocker refresh (such as `sync-issues --skip-prs`), its cached assessment is cleared, and open/draft PRs on active non-`FILTER_NON_OPT_OR_FIXED` issues render fallback rows (`DRAFT_OR_HOLD` or `WAITING_REVIEWER`) on the PR Status Board until `sync-prs` runs.
- **Commit- and timestamp-bound enrichment**: For each relevant kernel mainline PR, `pr_details` caches up to 30 changed files bound to head/base OIDs (`files_truncated: true` beyond 30) and a cleaned body excerpt bound to `UpdatedAt`. `CLOSED` PRs are skipped once an open, draft, or merged mainline kernel PR exists. `sync-issues` enriches `pending_candidates`, writable `pending` exports fill in missing entries unless `--backfill-notes` returns more than 30 items, and read-only connections export stored facts (emitting `enrichment_warning` when a needed PR refresh is skipped).
- **Manual PR links**: `"manual_pr_links"` records PR links in SQLite without calling GitHub, reusing cached `pull_requests` metadata if present or initializing state as `UNKNOWN`. Regular `sync-prs` resolves `UNKNOWN` links—even on `FILTER_NON_OPT_OR_FIXED` issues—without pinning them to the PR Status Board, and keeps polling open PRs linked to active non-`FILTER_NON_OPT_OR_FIXED` issues. If GitHub returns `NOT_FOUND`, `pr_lookup_failures` pauses automatic retries until you run `sync-prs --pr '<spec>'` explicitly, which also pins any open target PR to the board.

### Optimistic Version Counters

Every issue carries `version: {"review": N, "notes": M}`, which persists across acceptance, exclusion, archiving, and re-admission:

- **`review` counter**: Advances when issue metadata, classification, PR links/facts, visible `pr_details` evidence, or review baselines change; heuristic tweaks alone do not advance it. On linked PR updates, `review` advances when commit OIDs change, when `UpdatedAt` changes on a PR without commit OIDs, or when visible `pr_details` evidence changes (including hiding a stale non-empty excerpt or later restoring it, whereas a combined refresh with unchanged prose keeps `review` unchanged).
- **`notes` counter**: Tracks `issue_notes` (the sole note store) and increments on create, edit, or delete; saving identical text is a no-op. Notes survive override resets and archive/restore, and are deleted on exclusion. Version checks depend on the action:
  - **Both `review` and `notes`**: `new_issues`, `exclude_issues`, and `"overrides"` that include `"note"`.
  - **`review` only**: `"overrides"` omitting `"note"`, `ack_retriage`, standalone `manual_pr_links`, override reset, and exclusion restore.
  - **`notes` only**: standalone `"notes"` updates (leaves classification and `needs_retriage` unchanged; pairing `"notes"` with `manual_pr_links` also checks `review`).

### Client & UI Consistency

- **Versioned vs. unversioned writes**: All browser UI writes and any CLI write supplying `expected_versions` or `expected_version` verify version and queue preconditions inside the write transaction. Strict mode aborts the whole batch on conflict; `--skip-conflicts` commits valid items and skips conflicts. Unversioned quick flags (`triage --exclude`, `triage --ack-retriage` without JSON) skip version counters: `--exclude` rejects canonical or unknown issues (and closed issues in strict mode, updating the stored reason if already excluded), while `--ack-retriage` requires an active tracked issue (skipping closed/excluded items under `--skip-conflicts`). Unversioned JSON payloads still honor optional legacy `base_exported_at` / `if_unmodified_since` timestamps.
- **Snapshot ordering**: Workbench responses carry a monotonic `snapshot_revision`; the frontend ignores stale responses so arrival order cannot roll back newer state.
- **Draft lifecycle**: Version conflicts keep user-edited draft fields behind **Review latest**. Drafts live in browser memory: passive token-session expiry suspends them until **Unlock RW** re-authenticates in-page, whereas **Sign in with GitHub** performs a full-page OAuth redirect that clears in-memory drafts (as does explicit logout).
- **Post-commit recovery**: If a write commits in SQLite but reading the response snapshot fails (`committed: true`), the UI reloads state instead of retrying the mutation.
