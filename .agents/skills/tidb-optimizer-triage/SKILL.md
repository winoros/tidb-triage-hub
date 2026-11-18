---
name: tidb-optimizer-triage
description: Incrementally sync, inspect, deduplicate, triage, re-triage, and backfill root-cause notes for TiDB SQL Optimizer open issues and in-flight PRs in tidb-triage-hub.
---

# TiDB Optimizer Issue Triage SOP

Use this workflow when syncing GitHub issues/PRs or performing triage, deduplication, dynamic re-triage, or root-cause note backfill in `tidb-triage-hub`. For domain rules and state lifecycle details, see [`docs/AI_TRIAGE_PLAYBOOK.md`](../../../docs/AI_TRIAGE_PLAYBOOK.md).

## Prerequisites

- **CLI & GitHub auth**: Ensure `gh` is authenticated (used for `gh pr diff` / `gh issue view` / `gh issue list`, and by `triage-hub` when neither `GH_TOKEN` nor `GITHUB_TOKEN` is set) and `./bin/triage-hub` is built (`go build -o bin/triage-hub ./cmd/triage-hub`).
- **Local vs. remote mode**: Non-`serve` commands use local SQLite unless `TRIAGE_HUB_URL` (`--remote`, explicit `https://` or `http://` scheme required) is set. Protected remote commands authenticate via `TRIAGE_HUB_TOKEN` (`--token`), and non-loopback `http://` URLs require `--allow-insecure-http` (`TRIAGE_HUB_ALLOW_INSECURE_HTTP=1`) when sending a token. Remote `triage` defaults to `--skip-conflicts=true` and saves `data/remote_backup.db` whenever a batch commits changes (`--auto-backup=false` disables intermediate backups).

## 1. Triage Workflow

### Step 1: Sync issues and open PRs

```bash
./bin/triage-hub sync-issues
```

- `sync-issues` checks GraphQL quota automatically and prints updated watermarks plus `pending_count` / `retriage_count` and their issue numbers (use `./bin/triage-hub status` for a 0-point read-only check when not syncing).
- Pass `--skip-prs` for a fast issue-only sync, `--dry-run` to preview without writing, or `--full` after long gaps.
- Always sync *before* exporting: `triage` never fetches from GitHub. If both `pending_count` and `retriage_count` are `0`, no routine review export is needed.

### Step 2: Export the review bundle

```bash
./bin/triage-hub pending --out /tmp/triage_bundle.json
```

- Omit `--compact` on the first batch so `triage_rubric` includes all 12 few-shot calibration examples; add `--compact` on subsequent sliced batches or `--backfill-notes` runs to save context tokens.
- **Slice large queues**: For pending candidates, use `--only-pending`, `--number <n>` / `--numbers 73100,73101` (pending-only), or `--only-pending --scope <substr>` (`--scope` alone still exports re-triage items). For re-triage, use `--only-retriage` or `--re-triage 73063,73040`.
- On a read-only database, `pending` exports persisted facts without fetching missing PR file stats; run sync/export against the writable store when fresh enrichment is needed.

### Step 3: Judge candidates and construct decisions

Use `triage_rubric` and `valid_enums` in the `pending` output (`cat`, `severity`, `subsystem`, `decision_steps`, `difficulty_guide`, `severity_guide`, `retriage_guide`, `subsystems`, `note_style_guide`) and enforce these rules when building the decision payload:

- **Do not trust `suggested_triage` or template pre-fills**: Pre-filled `cat`, `severity`, `subsystem`, and `is_borderline_kept` values are rough heuristics (see `triage_rubric.boundary_rules.ai_judge_priority` and `decision_steps`); verify the SQL, PR diff, and `severity_guide` independently.
- **Judge the SQL, not just labels**: Unlabelled external issues can still be synthetic fuzzer/audit edge cases. Set `is_borderline_kept: true` only when `triage_rubric.boundary_rules.is_borderline_kept` holds (`true` on non-bugs warns, or fails under `--strict-notes`).
- **Inspect code and discussion when exported clues are insufficient**:
  - Regular `pending` exports embed `pr_details[].non_test_files` + `body_excerpt` plus fetched `body` and latest 15 `recent_comments`, though seed-bootstrapped issues may omit `body` / `recent_comments` until `sync-issues` refreshes them (and `--backfill-notes` always omits both).
  - When a linked PR's source diff is needed, target `pr_details[].repo` (`pingcap/tidb`, `pingcap/tiflash`, etc.) and exclude test and golden files by basename (`gh pr diff` does not accept positional file paths or `**` globs):
    ```bash
    gh pr diff <pr> --repo <owner/repo> --exclude '*_test.go' --exclude '*.test' --exclude '*.json' --exclude '*.result' --exclude '*.out'
    ```
  - Run `gh issue view <num> --repo pingcap/tidb --comments` when `body` / `recent_comments` is absent or has 15 items and earlier context is needed. If a local TiDB checkout is available (`tidb-triage-hub` does not contain TiDB source), inspect `pkg/planner/`, `pkg/statistics/`, `pkg/bindinfo/`, or `pkg/util/ranger/`.
- **Fast duplicate pre-check**: While scanning the exported bundle and comments, flag obvious in-batch duplicates or explicit `duplicate of #<num>` references early so you do not deeply grade the same bug twice.
- **Route Canonical vs. Supplemental issues**:
  - **Canonical (`sig/planner` + `type/bug` in `labels` — exposed as `is_canonical: true` on `retriage_candidates[]`/`backfill_issues[]`/`issues`, or `"canonical_planner_bug"` in `pending_candidates[].matched_scopes`; note `pending_candidates[]` has no `is_canonical` field)**: Never place in `exclude_issues`. Classify noise, flaky tests, confirmed duplicates (Step 4), already-fixed bugs, or non-optimizer root causes under `FILTER_*` (`severity: "N/A"`, `is_borderline_kept: false`) in `new_issues` or `overrides`.
  - **Supplemental (lacks `sig/planner` + `type/bug`)**: Accept only genuine optimizer bugs (`EASY`/`MEDIUM`/`HARD`) or core optimizer `ENHANCEMENT`s; put non-optimizer, unreachable-fuzz, or confirmed-duplicate (Step 4) items into `exclude_issues`. Set `"force_keep": true` in `new_issues` or `overrides` when an open supplemental issue must stay tracked despite graduation labels, such as recalibrating an open `type/enhancement` issue to `FILTER_NON_OPT_OR_FIXED` (see [`docs/AI_TRIAGE_PLAYBOOK.md`](../../../docs/AI_TRIAGE_PLAYBOOK.md#retention-graduation--re-admission)).
- **Resolve `retriage_candidates` (`decisions_template.overrides` starts empty)**:
  - Place each flagged item (`needs_retriage = 1`) into `overrides`, `ack_retriage`, or `exclude_issues` (`!is_canonical` only; inspect the full PR diff first if `files_truncated` is `true`). Never use standalone `notes` to resolve re-triage: it leaves `needs_retriage = 1` unchanged and cannot combine with `ack_retriage`.
  - When an already-tracked supplemental bug triggers `fix_pr_merged_still_open`, use `ack_retriage` if waiting for `release-*` backports, keep an active bug category (via `ack_retriage` or `overrides`) for partial fixes, or recalibrate via `overrides` to `FILTER_NON_OPT_OR_FIXED` once fully fixed on `master` and awaiting closure (prefer this over `exclude_issues`, which deletes its note).
  - In `overrides`, **omit `"note"`** to preserve the existing note (checking only `version.review`); include `"note"` when updating prose while clearing `needs_retriage`. Passing empty strings `{"zh-CN": "", "en": ""}` fails validation, or clears the stored note under `--allow-empty-note`.
  - When comments mention an unlinked PR on a retained issue, add `"manual_pr_links": {"73100": ["pingcap/tidb#73200"]}`. Re-exporting kernel PRs on a writable DB (`pending --re-triage <num> --compact`) fetches `pr_details` directly; run `sync-prs` for non-kernel repos or full blocker refresh, and pass `--pr '<spec>'` only to retry a `NOT_FOUND` lookup or pin an isolated PR to the PR Status Board.
- **Write concise bilingual notes**: Follow `triage_rubric.note_style_guide` (`zh-CN` and `en`, wrapping Go symbols, SQL clauses, hints, and sysvars in paired backticks). Length checks count **total UTF-8 runes including backticked ASCII identifiers** on `notes` and real bugs (`EASY`/`MEDIUM`/`HARD`): length outliers emit warnings unless `--strict-notes` is passed, while empty fields, `TODO:` prefixes, identical `zh-CN`/`en`, missing Chinese characters in `zh-CN`, and unmatched backticks always fail.

### Step 4: Deduplicate surviving new issues

Run full deduplication on surviving real-bug (`EASY`/`MEDIUM`/`HARD`) and `ENHANCEMENT` candidates (plus any item with explicit duplicate clues). Skip external dedup lookups for items already ruled out as non-optimizer `exclude_issues`, `FILTER_RANDOM_FUZZ`, `FILTER_AI_OBSCURE`, or already fixed on `master`.

1. **Tiered lookup (0-API sources first, GitHub Search API last)**:
   - **Current pending batch (0 API)**: Compare stack traces, error signatures, SQL patterns, and root causes across the exported bundle. When two pending items share the same root cause, pick the primary by strict precedence: **(1) canonical (`sig/planner` + `type/bug` in `labels`, or `"canonical_planner_bug"` in `matched_scopes`) over supplemental**, **(2) active/linked fix PR or higher customer impact**, **(3) earlier issue number / clearer reproducer**.
   - **Tracked `tidb-triage-hub` SQLite DB (0 API)**: Query `data/tidb_triage.db` locally—or in remote mode, first run `./bin/triage-hub backup` to pull a fresh snapshot to `data/remote_backup.db`. Search `issues` (`title`, `body`), `issue_notes` (`note_zh`, `note_en`), `pending_candidates` (`payload_json`, to catch untriaged duplicates outside partial-batch exports), and `archived_issues` / `excluded_issues` when checking recently closed or excluded history. (Do not rely on legacy `issues.auto_note_*` columns, which are empty for newly triaged issues.)
   - **GitHub (`pingcap/tidb`) — Search API (30 req/min, use selectively)**: Only for surviving candidates with distinctive anchors (exact error message, panic frame, function name, hint/sysvar), run at most 1 focused open-issue search (adding a date-bounded closed search such as `--state closed --search "<keywords> closed:>=YYYY-MM-DD"` only when suspecting a recently fixed duplicate):
     ```bash
     gh issue list --repo pingcap/tidb --state open --search "<keywords>" --limit 20
     ```
2. **Confirm root-cause equivalence & guard against regressions / filtered primaries**:
   - Never mark an issue as a duplicate based solely on a similar title, shared subsystem, or generic symptom (e.g., two unrelated `ERROR 1105` or wrong-result bugs). Confirm the same code path, panic stack, invariant violation, or fix PR.
   - **Closed / fixed-on-master regression guard**: If `#<orig>` is **closed** (or open and already marked `FILTER_NON_OPT_OR_FIXED` as fixed on `master`), do **not** mark the new issue as a duplicate unless `#<orig>`'s fix PR actually resolves it on current `master` (already fixed). If the bug still reproduces on `master` (regression, partial fix, or distinct trigger path), triage it as a real bug and note the relation (`回归/关联 #<orig>` / `Regression / follow-up to #<orig>`).
   - **Filtered / excluded primary guard**: If `#<orig>` is itself a duplicate, resolve to and cite the active primary `#<root>`. If an open `#<orig>` was previously `FILTER_RANDOM_FUZZ` / `FILTER_AI_OBSCURE` or in `excluded_issues` while the new report demonstrates realistic/customer triggerability, do not leave both filtered out—either promote `#<orig>` via `./bin/triage-hub pending --re-triage <orig>` + `overrides` (if actively tracked) or keep the new issue as the active real bug.
3. **Record the decision**:
   - **Confirmed duplicate — Canonical (`sig/planner` + `type/bug` in `labels`)**: Keep in `new_issues` with `cat: "FILTER_NON_OPT_OR_FIXED"`, `severity: "N/A"`, `is_borderline_kept: false`, matching `subsystem`, and a bilingual `note` passing Step 3's hard-block note checks that cites `#<orig>` (e.g., `与 #<orig> 重复：...` / `Duplicate of #<orig>: ...`).
   - **Confirmed duplicate — Supplemental (lacks `sig/planner` + `type/bug`)**: Move to `exclude_issues` with a clear reason citing the primary issue (e.g., `"Duplicate of #<orig> (<brief root cause>)"`).
   - **Suspected (unconfirmed) duplicate**: Do **not** filter it out. Triage normally and mention the possible relation in `note` (e.g., `疑似与 #<orig> 同因` / `possibly related to #<orig>`).
   - **Cross-update `#<orig>` when relevant** (only when `#<orig>` is actively tracked in `tidb-triage-hub` or accepted in the current pending batch):
     - If `#<orig>` is already exported in the current `pending` bundle (present in `expected_versions`) and not in `exclude_issues`, attach an unlinked fix PR via `manual_pr_links` and/or reflect escalated customer impact (`severity`/`note`) directly in `new_issues` or `overrides`. For an in-batch pending `#<orig>`, submit `manual_pr_links` in the same batch that accepts `#<orig>` in `new_issues` (otherwise `--skip-todo` drops the link and standalone `manual_pr_links` fails validation).
     - Otherwise, export `#<orig>` first with `./bin/triage-hub pending --re-triage <orig> --compact --out /tmp/retriage.json` and apply `manual_pr_links` / `overrides` / `notes`.

### Step 5: Dry-run and apply decisions

Before running `triage`, verify these batch protocol rules:

- **Preserve version bindings**: Keep `decisions_template.expected_versions` and any pre-filled `expected_version` fields intact and matching for every issue in the payload, including untouched `TODO:` entries and items moved to `exclude_issues`, `ack_retriage`, `notes`, or `manual_pr_links`. Version protocol checks run before `--skip-todo` filtering.
- **Never invent versions**: Copy exact `{"review": N, "notes": M}` values from the reviewed export. To include an issue outside the current export, export and review it first.
- **Bucket exclusivity**: Place each issue in at most one of `new_issues`, `exclude_issues`, `overrides`, `notes`, or `ack_retriage`. `manual_pr_links` cannot combine with `exclude_issues`; use it standalone on a tracked issue or alongside an active `new_issues`, `overrides`, `notes`, or `ack_retriage` entry (`--skip-todo` also skips companion `manual_pr_links` on skipped `new_issues` entries).
- **Partial batches**: Pass `--skip-todo` to **both** `--dry-run` and the live `triage` command whenever unreviewed `"TODO: ..."` template entries remain in the payload.

Save the payload to a JSON file so `--dry-run` and the live apply reuse the exact same file without regenerating JSON (`--stdin` is also supported):

```json
{
  "expected_versions": {
    "73100": {"review": 7, "notes": 1},
    "73101": {"review": 3, "notes": 1},
    "73063": {"review": 12, "notes": 2}
  },
  "new_issues": {
    "73100": {
      "cat": "EASY",
      "severity": "MAJOR",
      "subsystem": "plan_cache_spm_hint",
      "is_borderline_kept": false,
      "expected_version": {"review": 7, "notes": 1},
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
  "notes": {},
  "ack_retriage": [73063]
}
```

```bash
./bin/triage-hub triage --file /tmp/decisions.json --dry-run
./bin/triage-hub triage --file /tmp/decisions.json
```

- Verify zero errors **and** inspect `skipped_conflicts` in the JSON output: remote mode defaults to `--skip-conflicts=true` and exits `0` when conflicts are skipped; pass `--skip-conflicts=false` to fail the batch instead.
- `--dry-run` never reserves versions. Without an existing local SQLite file or explicit `--seed-json`, local `--dry-run` checks syntax and rubric rules only (`db_validated: false`), while the live run validates against the initialized DB.

## 2. Targeted Operations

- **Refresh local SQLite snapshot from remote server** (default `data/remote_backup.db`) before local deduplication or SQL inspection:

  ```bash
  ./bin/triage-hub backup
  ```

- **Backfill root-cause notes** for active real bugs missing `zh-CN` or `en`. Exports compact metadata without issue `body`, `recent_comments`, or partial note text, and enriches missing PR details when the returned batch has `<= 30` issues. Submitting `notes` requires both locales (replacing any partial note) without changing classification or `needs_retriage`:

  ```bash
  ./bin/triage-hub pending --backfill-notes 10 --cat HARD --compact --out /tmp/backfill.json
  ```

- **Re-inspect specific tracked issues** with live PR file stats (`decisions_template.overrides` starts empty: use `overrides`, `ack_retriage`, or `exclude_issues` for flagged items, `notes` for note-only edits on unflagged items, or omit unflagged items that need no change):

  ```bash
  ./bin/triage-hub pending --re-triage 73063,73040 --compact --out /tmp/retriage.json
  ```

- **Quickly exclude non-canonical noise** without JSON (skips version counters):

  ```bash
  ./bin/triage-hub triage --exclude 72985 --reason "TiDB Dashboard UI issue"
  ```
