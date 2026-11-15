---
name: tidb-optimizer-triage
description: Incrementally sync, inspect, triage, re-triage, and backfill root-cause notes for TiDB SQL Optimizer open issues and in-flight PRs in tidb-triage-hub.
---

# TiDB Optimizer Issue Triage SOP

Use this workflow whenever syncing GitHub issues/PRs or performing AI-assisted triage, dynamic re-triage, or note backfill in `tidb-triage-hub`.

**Prerequisites**:
- Ensure `gh` CLI is authenticated (used on the AI worker for `gh pr diff` / `gh issue view`, or by local `triage-hub` when `GITHUB_TOKEN` is unset) and `./bin/triage-hub` is up to date (`go build -o bin/triage-hub ./cmd/triage-hub` if missing or after Go code changes).
- **Local vs. Remote Split Mode**: All `./bin/triage-hub` commands below work identically against a local SQLite DB (default) **or** against a remote Machine A web server when `TRIAGE_HUB_URL` + `TRIAGE_HUB_TOKEN` (or `--remote` + `--token`) are set. In remote mode, `triage` automatically skips items modified concurrently on the Web UI (`--skip-conflicts=true` by default) and downloads a local disaster-recovery snapshot to `data/remote_backup.db`.

## 1. End-to-End Triage Loop (Zero Temp Files)

1. **Check status & GraphQL quota** (0 GraphQL point cost):
   ```bash
   ./bin/triage-hub status
   ```
2. **Run pure-Go dual-stream incremental sync** (discovers new candidates, updates existing metadata & 3-tier PR links, evaluates 6 dynamic re-triage triggers, and refreshes open PR blockers):
   ```bash
   ./bin/triage-hub sync-issues
   ```
   - Use `--skip-prs` for a fast issue-only sync, `--dry-run` to preview without writing to SQLite, or `--full` after long gaps.
3. **Export pending & flagged re-triage bundle** (includes `triage_rubric` with full few-shot examples and `retriage_guide`, PR non-test changed file stats `pr_details`, extracted `clues`, and `suggested_triage`):
   ```bash
   ./bin/triage-hub pending
   ```
   - Omit `--compact` by default so the full few-shot examples stay in context for boundary calibration (reserve `--compact` for later token-constrained runs once a benchmark set is established).
   - When the queue is large, process in batches using `--only-retriage`, `--only-pending`, `--number <n>`, `--numbers 73100,73101`, or `--scope <substr>` (paired with `triage --skip-todo` on submission).
4. **Apply independent AI Judge & verify root cause before finalizing decisions**:
   - **Do NOT anchor on `decisions_template` pre-fills or `suggested_triage`**: `decisions_template.new_issues` pre-populates `suggested_triage` (including `default:logical_rewrite`, line-count `EASY`/`HARD` heuristics, and `hasMergedMainline -> FILTER_NON_OPT_OR_FIXED`). Independently verify and overwrite `cat`, `severity`, `subsystem`, and `is_borderline_kept`.
   - **Universal AI Judge (labels are not enough)**: External reports often omit `fuzz` or `ai` labels. Always inspect the trigger SQL itself for edge-case signals (synthetic table/column names, deeply nested redundant `CAST`/`CONVERT`/bitwise ops, disabled optimizer switches `@@tidb_opt_*` or rule blacklists, obscure `BIT`/`SET`/`JSON` type mixing)—and conversely, do NOT auto-filter a tagged fuzz/AI case if the SQL is realistic.
   - **Verify root cause, difficulty & independent severity**:
     - When an issue has a linked mainline PR, inspect its source diff if `pr_details[].body_excerpt` + `pr_details[].non_test_files` leave ambiguity:
       ```bash
       gh pr diff <pr_num> --repo pingcap/tidb
       ```
     - When an issue has no PR yet, inspect full comments (`gh issue view <num> --repo pingcap/tidb --comments`) and, if a local TiDB checkout is available (note: `tidb-triage-hub` does **not** contain TiDB source), inspect `pkg/planner/`, `pkg/statistics/`, `pkg/bindinfo/`, `pkg/util/ranger/`.
     - Grade `cat` (Difficulty) and `severity` independently using Section 2.4 and `triage_rubric.severity_guide` (low-confidence items pre-fill `"TODO: REVIEW"` in `decisions_template`).
5. **Dry-run & apply decisions via stdin** (no temporary files needed; **always carry `base_exported_at`** from the `pending` bundle's `exported_at_utc` — and `if_unmodified_since` from `retriage_candidates[].triaged_at` when overriding — so concurrent Web UI calibrations are protected):
   ```bash
   ./bin/triage-hub triage --stdin --dry-run <<'EOF'
   {
     "base_exported_at": "2026-11-13T15:00:00.000000000Z",
     "new_issues": {
       "73100": {
         "cat": "EASY",
         "severity": "MAJOR",
         "subsystem": "plan_cache_spm_hint",
         "is_borderline_kept": false,
         "note": {
           "zh-CN": "`planCacheKey` 漏编码影响常量折叠的会话变量导致 Prepared Plan Cache 返回过期结果",
           "en": "`planCacheKey` omits session variable affecting constant folding, causing stale Prepared Plan Cache results"
         }
       }
     },
     "exclude_issues": {
       "73101": "Vectorized builtin cast bug in pkg/expression without optimizer involvement"
     },
     "overrides": {},
     "ack_retriage": [73063]
   }
   EOF
   ```
   Once `--dry-run` passes with zero errors, re-run without `--dry-run` (add `--skip-todo` if submitting a partial batch). Note that applying `triage` defaults to `--sync-new-prs=true` (requires `>=20` GraphQL points via `--min-quota`; pass `--sync-new-prs=false` if offline or low on quota).

## 2. Hard Invariants & Boundary Rules

1. **Canonical Completeness (`sig/planner` + `type/bug`)**:
   - Issues carrying **both** `sig/planner` and `type/bug` (`is_canonical == true`) **MUST NEVER** be placed in `exclude_issues`.
   - Keep noisy/non-optimizer canonical issues in `new_issues` using `FILTER_RANDOM_FUZZ`, `FILTER_AI_OBSCURE`, or `FILTER_NON_OPT_OR_FIXED` (`severity: "N/A"`).
2. **Supplemental Pool Hygiene (`!is_canonical`)**:
   - Accept **only** genuine SQL optimizer bugs (`EASY` / `MEDIUM` / `HARD`) or core optimizer `ENHANCEMENT`s.
   - Put all non-optimizer supplemental issues (`executor`, `expression` vectorized eval, `tikv`, `ddl`, Dashboard UI, flaky tests, obscure fuzzers) into `exclude_issues` so their open PRs never pollute the PR Status Board.
3. **`is_borderline_kept` Contract**:
   - Set `is_borderline_kept: true` **only** when `cat` is `EASY`, `MEDIUM`, or `HARD` **and** the bug originated from fuzzing/SQLancer or static AI/code audit, kept because the trigger SQL is realistic and causes wrong results, panic, deadlock, or missing locks.
4. **Difficulty & Severity Calibration (Real Bugs Only)**:
   - **Difficulty (`cat`: `EASY` / `MEDIUM` / `HARD`; `ENHANCEMENT` and `FILTER_*` occupy `cat` and skip difficulty)**:
     - *With PR*: Judge from `gh pr diff` / `non_test_files` whether cross-stage contracts or high-risk invariants change. A small diff (`<=55` lines) touching CBO cost/cardinality, Join Reorder, or single-pass name resolution still warrants `MEDIUM`/`HARD` due to regression risk; localized guards/Plan Cache key fixes without contract changes are `EASY`.
     - *Without PR*: Estimate from module coupling (`EASY`: Plan Cache key, single-point guard; `MEDIUM`: Ranger residual filter, rewrite+pruning schema, partition lock, Global Stats merge; `HARD`: CBO/cardinality, Join Reorder, name resolution, `ANALYZE` concurrency, MPP).
   - **Severity (echo AI Judge triggerability; `ENHANCEMENT` / `FILTER_*` -> `"N/A"`)**:
     - *Wrong results graded by trigger realism & reachability*:
       - `CRITICAL` (S0): Common/easily-hit `SELECT` wrong result, DML wrong data mutation, silent missing `FOR UPDATE` lock, panic/crash, OOM, permanent worker exit, cluster GC block, or privilege bypass.
       - `MAJOR` (S1): Realistic `SELECT` wrong result requiring specific non-trivial conditions (not an edge case, yet not everyday SQL), valid common SQL failing with `ERROR 1105` / column-not-found, stats meta deadlock, or order-of-magnitude production plan regression.
       - `MODERATE` (S2): Corner-case / hard-to-hit wrong result retained via borderline Judge, moderate cardinality/cost skew (correct result, no outage), Plan Cache/SPM miss, hint export mismatch, or narrow MySQL compatibility gap.
       - `MINOR` (S3): Hint warning text, `EXPLAIN` / `SHOW STATS` / slow-log formatting, or diagnostic polish.
5. **Dynamic Re-Triage Resolution** (follow `triage_rubric.retriage_guide`):
   - `new_fix_pr`: Inspect `pr_details[].non_test_files` / `gh pr diff`; if accurate, add to `"ack_retriage"`, otherwise update `"overrides"`.
   - `fix_pr_merged_still_open`: If waiting for cherry-pick/backport only, `"ack_retriage"`; if fully fixed on master and pending close, `"overrides"` to `FILTER_NON_OPT_OR_FIXED` (`severity: "N/A"`); if partial fix/regression, update `"overrides"`.
   - `customer_reported` / `gh_severity_escalated`: Re-evaluate `severity` independently against `severity_guide` (`"overrides"` vs `"ack_retriage"`).
   - `label_scope_changed`: If `sig/planner` was removed and the issue is non-optimizer, it is no longer canonical-protected and can go to `"exclude_issues"`; if `duplicate`/`wontfix` or `type/enhancement` was added, update `"overrides"`.
   - `substantive_new_comments`: If root cause, subsystem, or validity (`not a bug`) pivoted, submit `"overrides"`; if comments only confirm current diagnosis, `"ack_retriage"`.
   - If a PR is mentioned in comments without automatic Timeline linking, attach it via `"manual_pr_links": {"73100": ["pingcap/tidb#73200"]}`.
6. **Bilingual Note Quality Gate**:
   - Formula: `[Origin tag if borderline] + <Component/Func> + <specific flaw> + <user-visible symptom>`.
   - Target `25–85` Chinese chars (`zh-CN`) and `50–170` English chars (`en`). Note: `--dry-run` hard-blocks unmatched backticks, missing Chinese in `zh-CN`, identical `zh-CN`/`en`, empty fields, or `TODO:`, while length outliers (`<12`/`>140` zh, `<25`/`>260` en) emit warnings unless `--strict-notes` is passed.
   - Wrap all Go functions, structs, SQL clauses, hints, and sysvars in **paired backticks** (e.g. `` `detachCNFCondAndBuildRangeForIndex` ``, `` `INL_JOIN` ``, `` `ERROR 1105` ``).

## 3. Useful Targeted Commands

```bash
# Backfill bilingual root-cause notes for top 10 highest-priority unnoted real bugs (filterable with `--cat HARD|MEDIUM|EASY`)
# Note: `overrides` entries require valid `cat`, `severity`, and `subsystem` alongside `note`
# (preserve existing classification unless PR diff/comments reveal a clear misgrade).
./bin/triage-hub pending --backfill-notes 10 --cat HARD

# Re-inspect specific existing issues with live PR file stats
./bin/triage-hub pending --re-triage 73063,73040

# Quickly exclude misclassified non-canonical supplemental noise
./bin/triage-hub triage --exclude 72985 --reason "TiDB Dashboard UI issue"
```
