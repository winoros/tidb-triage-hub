---
name: tidb-optimizer-triage
description: Incrementally sync, inspect, triage, re-triage, and backfill root-cause notes for TiDB SQL Optimizer open issues and in-flight PRs in tidb-triage-hub.
---

# TiDB Optimizer Issue Triage SOP

Use this workflow whenever syncing GitHub issues/PRs or performing AI-assisted triage, dynamic re-triage, or note backfill in `tidb-triage-hub`.

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
3. **Export pending & flagged re-triage bundle** (includes `triage_rubric`, PR non-test changed file stats `pr_details`, extracted `clues`, and `suggested_triage`):
   ```bash
   ./bin/triage-hub pending --compact
   ```
   - Omit `--compact` if you want the 8 full few-shot examples embedded in the JSON output.
4. **Verify root cause before finalizing decisions**:
   - Never rely blindly on `suggested_triage`. When an issue has a linked mainline PR, inspect its source diff if `pr_details[].body_excerpt` + `pr_details[].non_test_files` leave ambiguity:
     ```bash
     gh pr diff <pr_num> --repo pingcap/tidb
     ```
   - When an issue has no PR yet, inspect full comments or locate the relevant function in TiDB source (`pkg/planner/`, `pkg/statistics/`, `pkg/bindinfo/`, `pkg/util/ranger/`).
5. **Dry-run & apply decisions via stdin** (no temporary files needed):
   ```bash
   ./bin/triage-hub triage --stdin --dry-run <<'EOF'
   {
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
   Once `--dry-run` passes with zero errors, re-run without `--dry-run` (add `--skip-todo` if submitting a partial batch).

## 2. Hard Invariants & Boundary Rules

1. **Canonical Completeness (`sig/planner` + `type/bug`)**:
   - Issues carrying **both** `sig/planner` and `type/bug` (`is_canonical == true`) **MUST NEVER** be placed in `exclude_issues`.
   - Keep noisy/non-optimizer canonical issues in `new_issues` using `FILTER_RANDOM_FUZZ`, `FILTER_AI_OBSCURE`, or `FILTER_NON_OPT_OR_FIXED` (`severity: "N/A"`).
2. **Supplemental Pool Hygiene (`!is_canonical`)**:
   - Accept **only** genuine SQL optimizer bugs (`EASY` / `MEDIUM` / `HARD`) or core optimizer `ENHANCEMENT`s.
   - Put all non-optimizer supplemental issues (`executor`, `expression` vectorized eval, `tikv`, `ddl`, Dashboard UI, flaky tests, obscure fuzzers) into `exclude_issues` so their open PRs never pollute the PR Status Board.
3. **`is_borderline_kept` Contract**:
   - Set `is_borderline_kept: true` **only** when `cat` is `EASY`, `MEDIUM`, or `HARD` **and** the bug originated from fuzzing/SQLancer or static AI/code audit, kept because the trigger SQL is realistic and causes wrong results, panic, deadlock, or missing locks.
4. **Dynamic Re-Triage Resolution**:
   - For each item in `retriage_candidates`:
     - If current `cat` / `severity` / `subsystem` / `note` remain accurate (e.g. a new fix PR confirms the original diagnosis), add its number to `"ack_retriage": [73063]` (or run `./bin/triage-hub triage --ack-retriage 73063`).
     - If scope/severity/difficulty changed (e.g. `fix_pr_merged_still_open` or root cause shifted subsystem), submit updated fields in `"overrides"`.
5. **Bilingual Note Quality Gate**:
   - Formula: `[Origin tag if borderline] + <Component/Func> + <specific flaw> + <user-visible symptom>`.
   - Target `25–85` Chinese chars (`zh-CN`) and `50–170` English chars (`en`).
   - Wrap all Go functions, structs, SQL clauses, hints, and sysvars in **paired backticks** (e.g. `` `detachCNFCondAndBuildRangeForIndex` ``, `` `INL_JOIN` ``, `` `ERROR 1105` ``).

## 3. Useful Targeted Commands

```bash
# Backfill bilingual root-cause notes for top 10 highest-priority unnoted real bugs
./bin/triage-hub pending --backfill-notes 10 --compact

# Re-inspect specific existing issues with live PR file stats
./bin/triage-hub pending --re-triage 73063,73040 --compact

# Quickly exclude misclassified non-canonical supplemental noise
./bin/triage-hub triage --exclude 72985 --reason "TiDB Dashboard UI issue"
```
