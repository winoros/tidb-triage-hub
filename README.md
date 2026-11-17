# TiDB Optimizer Triage & PR Status Hub

Standalone pure-Go workbench (`Go` + `modernc.org/sqlite` + zero-dependency bilingual frontend) for **TiDB Optimizer Open Issue Triage**, **Dynamic Re-Triage**, and **In-Flight PR Blocker Tracking**.

## Directory Layout

- `cmd/triage-hub/` — Single-binary HTTP server & CLI entrypoint (`serve`, `status`, `sync-issues`, `sync-prs`, `pending`, `triage`, `backup`) supporting both local SQLite and remote HTTP (`--remote` / `TRIAGE_HUB_URL`)
- `internal/ghgql/` — Shared GitHub GraphQL v4 client (pure-Go `net/http` `HTTPRunner` via `GITHUB_TOKEN`/`GH_TOKEN`, falling back to `gh api graphql`), retry runner, quota pre-flight & `CostTracker`
- `internal/issuesync/` — Pure-Go Dual-Stream sync engine (`>800`-hit time-window bisection, 7-repo PR reverse index, Tier-1/Tier-2 optimizer-affinity discovery & label-delta re-admission)
- `internal/triage/` — Embedded triage rubric + few-shot examples, PR non-test file enrichment, clue extraction, heuristic pre-suggestions, 6-trigger dynamic re-triage evaluator & bilingual note validator
- `internal/prstatus/` — Batched GitHub GraphQL fetcher + 9-stage `pingcap/tidb` PR blocker classifier
- `internal/store/` — Incremental SQLite store (`issues`, `issue_overrides`, orthogonal `issue_notes`, 3-tier `issue_pr_links`, `issue_triage_state`, `archived_issues`, `pending_candidates`, `excluded_issues`, `pull_requests`)
- `internal/syncer/` — Pure-Go issue + PR sync orchestrator (zero Python or staging files)
- `internal/server/` — HTTP routes & single-point `resolveSession` RBAC hook
- `web/` — Self-contained bilingual (`zh-CN` / `en`) frontend (`index.html`, `i18n/`) with 3-tab Inbox (`Pending | Re-triage | Excluded`)
- `.agents/skills/tidb-optimizer-triage/SKILL.md` — In-repo AI Agent triage SOP
- `docs/AI_TRIAGE_PLAYBOOK.md` — Domain rules, boundary contracts & dynamic re-triage reference
- `seed/initial_dataset.json` — Initial seed snapshot used when `data/tidb_triage.db` is missing or empty

## Quick Start

```bash
# Build & run unit tests
go build -o bin/triage-hub ./cmd/triage-hub
go test -count=1 ./...
# Frontend request/session/draft concurrency tests (Node.js, no npm dependencies)
node --test web/review_state.test.cjs

# Start standalone RW workbench (default http://127.0.0.1:8787)
./bin/triage-hub

# Start in read-only viewer mode
./bin/triage-hub --readonly
```

## CLI Sync & AI Triage Workflow

SQLite runs in `WAL` mode (`busy_timeout=15000`), so CLI commands safely run concurrently while the web server is online.

```bash
# 1. Inspect SQLite watermarks, pending/retriage queues & `gh` GraphQL quota (0 point cost)
./bin/triage-hub status

# 2. Run pure-Go dual-stream incremental Issue discovery + PR refresh
#    (flags: --dry-run, --skip-prs, --full, --since <RFC3339>, --untriaged-lookback-days 90)
./bin/triage-hub sync-issues

# 3. Export pending & flagged re-triage bundle (with Rubric, PR file stats & smart suggestions)
./bin/triage-hub pending

# 4. Validate (--dry-run) and apply triage decisions via stdin (supports --skip-todo)
./bin/triage-hub triage --stdin --dry-run < decisions.json
./bin/triage-hub triage --stdin < decisions.json

# 5. Targeted operations: refresh specific PRs, ack re-triage, exclude noise, or backfill notes
./bin/triage-hub sync-prs --pr 73064,pingcap/tiflash#11155
./bin/triage-hub triage --ack-retriage 73063
./bin/triage-hub triage --exclude 72985 --reason "Dashboard UI issue"
./bin/triage-hub pending --backfill-notes 10
```

## Split Deployment (Machine A Web/Sync + Machine B AI Triage)

`bin/triage-hub` embeds `web/` and `seed/initial_dataset.json`, and uses a pure-Go HTTP GraphQL client whenever `GITHUB_TOKEN` (or `GH_TOKEN`) is set — so **Machine A** needs only the single binary (`1 vCPU / 512 MB RAM` is sufficient, no `gh` CLI required) and owns the single SQLite source of truth.

```bash
# Machine A (Web Dashboard + SQLite + GitHub Sync):
# Anonymous visitors see Read-Only mode; maintainers click "Unlock RW" in browser; Machine B uses Bearer token.
# Add --trust-proxy (or TRIAGE_HUB_TRUST_PROXY=1) when serving behind a TLS/reverse proxy:
# reads the rightmost X-Forwarded-* hop ONLY from loopback / RFC1918 / RFC4193 private peer IPs by default
# (override peer allowlist via --trusted-proxy-cidrs / TRIAGE_HUB_TRUSTED_PROXY_CIDRS).
GITHUB_TOKEN=ghp_xxx \
TRIAGE_HUB_TOKEN=your-strong-secret \
./bin/triage-hub serve \
  --addr 127.0.0.1:8787 \
  --trust-proxy \
  --db /var/lib/triage-hub/tidb_triage.db \
  --sync-prs-interval 30m \
  --sync-issues-interval 2h

# Machine B (AI Triage Worker with local TiDB repo & gh CLI — zero local SQLite state):
# Explicit http:// or https:// scheme is required. Sending a token over non-loopback plain http://
# is refused unless --allow-insecure-http (or TRIAGE_HUB_ALLOW_INSECURE_HTTP=1) is explicitly set.
export TRIAGE_HUB_URL="https://triage.example.com"
export TRIAGE_HUB_TOKEN="your-strong-secret"

./bin/triage-hub status
./bin/triage-hub sync-issues
./bin/triage-hub pending
./bin/triage-hub triage --stdin --dry-run < decisions.json
./bin/triage-hub triage --stdin < decisions.json   # conflict-safe + auto-saves snapshot to data/remote_backup.db
./bin/triage-hub backup --out data/remote_backup.db
```

## Review Snapshots and Conflicts

`pending` refreshes missing GitHub file evidence when the database is writable,
then reads candidates and their versions in one SQLite transaction. Matching
cached evidence is reused. A read-only connection exports only persisted facts
and reports a skipped refresh only when evidence is missing. If SQLite rejects
a refresh write as read-only, the export uses stored evidence with a warning.
Workbench state responses also read the dataset and inbox queues together with
`snapshot_revision`, a durable counter advanced by the same transactions that
change the workbench. The UI ignores older snapshots before reconciling editors;
response arrival order cannot roll back the displayed queues or discard a newer
draft. No GitHub requests run inside these read transactions.
Submitting decisions never fetches GitHub: use `sync-prs` / `sync-issues`, then export
and review the refreshed facts. `triage --sync-new-prs` and its `--min-quota` flag
are deprecated compatibility options and have no effect. Manual links to uncached
PRs remain UNKNOWN until a refresh. Regular `sync-prs` includes unresolved links
even on filtered issues. A lookup explicitly returning no PR is recorded separately
from PR facts, retains the manual relationship, and stops automatic `sync-prs`
and export-enrichment retries; use `sync-prs --pr <spec>` after correcting access or the target.

PR metadata has one owner (`pull_requests`); links and pending payloads are projections.
Shared file evidence lives in `pr_details`, bound to head/base commits when available
(or conservatively to state/update time for legacy data). Missing enrichment never
deletes an existing cache entry. Metadata-only activity on unchanged commits keeps
file evidence available. Mutable PR prose is bound to its fetched `UpdatedAt`:
a newer observation hides the old body from clues and schedules enrichment even
when commits match. Hiding or restoring nonempty prose also invalidates the reviewed version;
refreshing unchanged visible evidence does not. With known commits, timestamp-only
updates leave reviews valid when there is no cached prose, the prose is empty, or
it is already hidden. A combined fact/detail refresh compares the final visible
evidence, so fetching unchanged prose with a newer timestamp keeps reviews valid.
Pending PR updates advance review versions only through material changes to the
pending projection and its watermark.
MERGED is terminal, including a same-second merge with a changed commit.
CI/review/owner fields belong to a complete observation;
a partial change to its state, commits or update time invalidates that assessment.
The PR Status Board uses only current complete assessments. PR set comparison is
independent of display order. Database repairs and trigger upgrades run atomically once
per schema migration; opening the current schema does not repair business data.

Each reviewable object includes `version: {"review": ..., "notes": ...}`. These are
opaque, persistent counters: copy them from the snapshot, never generate them from
wall-clock time. They survive acceptance, exclusion, archiving and re-admission.

- `review` changes with issue evidence, classification, PR links/facts and review
  baselines, including changes that do not produce a re-triage trigger.
- `issue_notes` is the sole effective note store; old `auto_note` columns are
  import compatibility data. Imports can fill an untouched empty placeholder,
  but preserve existing notes and notes explicitly cleared by a user.
  Archiving/restoration preserves notes. Acceptance
  writes the submitted note; exclusion deletes it. `notes` advances on content
  changes or creation/deletion of the note, including an empty note. Saving the
  same content is a no-op. A note-only update checks this counter and requires an
  active issue; classification updates that write notes and exclusions check
  both counters. Updating notes does not acknowledge re-triage, and resetting
  classification preserves notes.
- Derived suggestions alone do not change the review version. Pending enrichment
  also has an internal cache revision to reject stale enrichment writes.

Keep `decisions_template.expected_versions` intact when moving an issue between
`new_issues`, `overrides`, `notes`, `exclude_issues`, `ack_retriage` or `manual_pr_links`.
Any explicit per-item or batch version selects the version protocol for the entire
batch, which requires an entry for every selected issue. An explicit empty version
map cannot downgrade to legacy mode. Export additional issues before adding them
to a versioned template; do not fill missing entries using current versions. Per-item
`expected_version`, when present, must agree with that map. Interactive HTTP
mutations carry the `expected_version` of the displayed card or edit drawer.
Inspect is read-only and owns no editable draft. Login binds it from the current
workbench snapshot; Web submissions never fall back to an unversioned draft.
A refreshed page must not attach a newer version to an older draft: pending drafts
show a **Review latest** action when their reviewed version changes. Successful
acceptance or exclusion consumes that draft; re-admission starts a fresh draft.

Queue eligibility and versions are checked by the same command rules for single
and batch writes. A known identity that has moved out of the required queue is a
conflict, while a never-seen identity is not found. Versions are checked in the
write transaction before any batch mutations. Strict
mode rejects the whole batch; `--skip-conflicts` skips the conflicting issue and its
companion PR links. A dry run validates a snapshot, but does not reserve it: applying
always checks again. A dry-run snapshot must include committed WAL contents;
if a consistent snapshot cannot be taken it fails, without copying the main file
alone. Empty snapshots use the same seed initialization as execution. Legacy `base_exported_at`, `if_unmodified_since`, `triaged_at`
and `sync_watermark` remain supported for older clients; new decisions use versions.

Local and remote CLI use the same batch executor. Web mutations use the shared
review command service and the same transactional store rules. Successful Web writes return the complete dataset/pending/excluded snapshot;
the UI applies it only if its revision has not been superseded.
On a conflict the UI refreshes facts, preserves the draft and its old binding, and
requires **Review latest** before resubmission. Only edited fields are retained over
new suggestions. Passive authentication expiry suspends drafts until login; explicit
logout clears them. Failures from old requests cannot reset a newer session.
If a decision commits but loading the response snapshot fails, HTTP returns
`committed: true`; clients suspend workbench controls and reload state rather
than retrying the mutation. An accepted snapshot restores the controls; a delayed
recovery failure cannot clear a newer successful snapshot.
