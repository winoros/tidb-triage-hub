# TiDB Optimizer Issue Triage & Dynamic Re-Triage Playbook

This playbook documents the domain rules, architecture, and CLI/Web workflows for **TiDB Triage Hub**.

## 1. Two-Tier Discovery vs. Retention

Discovery of new untriaged issues (`internal/issuesync/scope.go`) is decoupled from retention of already-triaged issues:

- **Tier-1 Deterministic Discovery (100% recall, no age limit)**:
  - `canonical_planner_bug`: `sig/planner` + `type/bug`
  - `planner_no_type`: `sig/planner` without `type/{bug,enhancement,feature-request,sub-task}`
  - `comp_{statistics,bindinfo,spm,mview}`: without `sig/planner`, paired with `type/{bug,compatibility,regression}`
  - `planner_*` & core optimizer `epic/*` (`cardinality-estimation`, `cost-model`, `plan-cache`, `sql-plan-management`, `hint`, `cascades`)
- **Tier-2 Affinity-Gated Discovery (rolling `--untriaged-lookback-days`, default 90d)**:
  - Blocks any non-planner `sig/*`, 35+ non-optimizer `component/*` labels, bot authors (`@coderabbitai`, CI bots — excluding `@ghost`), flaky tests, and cherry-pick tracking issues.
  - Requires positive optimizer signals in the title (`S1`), body stack/hints/sysvars (`S2`), linked PR title (`planner:`/`statistics:`/`bindinfo:`), or plan-sensitive SQL bug patterns (`S3`).
- **Retention & Re-Admission**:
  - Triaged issues in SQLite never age out due to rolling lookback windows; if closed or graduated, their `auto_*` baseline is preserved in `archived_issues` and restored automatically if reopened on GitHub.
  - Excluded issues record `excluded_labels_json` at exclusion time and automatically re-enter `pending_candidates` (`readmitted:+<label>`) only if they later gain a strong optimizer label (`sig/planner`, `component/{statistics,bindinfo,spm}`, `planner/*`, optimizer `epic/*`) or become canonical (`sig/planner` + `type/bug`).

## 2. `FILTER_*` vs. `excluded_issues` Boundary

| Issue Pool | Condition | Where Noise Goes | PR Status Board Impact |
| :--- | :--- | :--- | :--- |
| **Canonical** | `sig/planner` + `type/bug` | Stay in `issues` as `FILTER_RANDOM_FUZZ`, `FILTER_AI_OBSCURE`, or `FILTER_NON_OPT_OR_FIXED` (cannot be excluded) | Open PRs linked *solely* to `FILTER_NON_OPT_OR_FIXED` issues are isolated from PR Status polling & board |
| **Supplemental** | Lacks `sig/planner` + `type/bug` | Exclude into `excluded_issues` (`triage --exclude` or Inbox/Drawer Exclude) | Completely removed from `issues` and `issue_pr_links` |

## 3. Dynamic Re-Triage Lifecycle (6 Baseline-Diff Triggers)

Every tracked issue stores an acknowledged snapshot in `issue_triage_state`. Re-triage (`needs_retriage = 1`) fires **only** on high-signal transitions relative to that baseline:

1. `new_fix_pr` — Unclaimed real bug or `FILTER_*` issue receives a new mainline `OPEN`/`DRAFT` fix PR.
2. `fix_pr_merged_still_open` — Mainline fix PR transitions to `MERGED` while the issue remains `OPEN` (and is not `FILTER_NON_OPT_OR_FIXED`).
3. `customer_reported` — `report/customer` label is newly added.
4. `gh_severity_escalated` — GitHub `severity/*` escalates to `major`/`critical` above baseline and exceeds current triaged severity.
5. `label_scope_changed` — `sig/planner` removed, `duplicate`/`wontfix` added, or `type/enhancement` added to a real bug.
6. `substantive_new_comments` — Post-watermark human comment (stripping bots, quotes, code blocks, and `/label` Prow commands) hits a root-cause/not-a-bug/duplicate pivot or ≥50 chars of non-author technical analysis.

Resolve flagged items via:
- **Keep Current**: Web `Keep Current` button, `POST /api/issues/{number}/retriage/ack`, or CLI `triage --ack-retriage <nums>` / `"ack_retriage": [...]`.
- **Recalibrate**: Save updated category/severity/subsystem/note in Web Calibration Drawer or CLI `"overrides"`.
- **Exclude (when no longer canonical)**: If `label_scope_changed` removes `sig/planner` and the issue is non-optimizer, move it to `exclude_issues` (or `triage --exclude <num>`).
