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
- `seed/initial_dataset.json` — Initial seed snapshot used only if `data/tidb_triage.db` does not exist yet

## Quick Start

```bash
# Build & run unit tests
go build -o bin/triage-hub ./cmd/triage-hub
go test -count=1 ./...

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
