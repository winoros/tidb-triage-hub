# TiDB Optimizer Triage & PR Status Hub

Standalone full-stack workbench (`Go` + pure-Go `SQLite` + zero-dependency bilingual frontend) for **TiDB Optimizer Open Issue Triage** and **In-Flight PR Blocker Tracking**, completely decoupled from `winoros.github.io`.

## Directory Layout

- `cmd/triage-hub/main.go` — Standalone HTTP server & CLI entrypoint
- `internal/store/` — SQLite schema (`issues`, `issue_overrides`, `issue_pr_links`, `pull_requests`, `pending_candidates`, `excluded_issues`, `sync_runs`)
- `internal/prstatus/` — Batched GitHub GraphQL fetcher (`gh api graphql`) + 9-stage `pingcap/tidb` PR blocking classifier
- `internal/syncer/` — Dual-stream $O(\Delta)$ incremental issue + PR status orchestrator
- `internal/server/` — HTTP routes (`/`, `/bootstrap.js`, `/api/session`, `/api/state`, `/api/dataset.json`, triage & sync REST endpoints) with single-point `resolveSession` RBAC hook
- `web/` — Self-contained bilingual (`zh-CN` / `en`) frontend (`index.html`, `i18n/`)
- `scripts/sync_issues.py` — Standalone Dual-Stream incremental issue & 7-repo PR reverse-index sync script
- `seed/initial_dataset.json` — Fallback seed snapshot used only if `data/tidb_triage.db` does not exist yet

## Quick Start

```bash
# Build & test
go build -o bin/triage-hub ./cmd/triage-hub
go test -count=1 ./...

# Start standalone RW workbench (default http://127.0.0.1:8787)
./bin/triage-hub

# Start in read-only viewer mode
./bin/triage-hub --readonly
```

## Manual CLI Sync & AI Triage Workflow

Syncing GitHub Issues & PRs is intentionally driven via CLI / AI Agent commands (using local `gh` authentication, ~`200–350` GraphQL points per full ~200-PR refresh out of the `5,000 points/hour` single-account budget) rather than public homepage buttons. SQLite runs in `WAL` mode (`busy_timeout=15000`), so CLI commands can safely run while the web server is online.

```bash
# 1. Inspect SQLite snapshot watermarks, pending count & current `gh` GraphQL quota (0 point cost)
./bin/triage-hub status

# 2. Refresh all tracked open PR statuses (~200 PRs in ~11 batched GraphQL calls)
./bin/triage-hub sync-prs

#    Or refresh specific PR(s) immediately (~1 GraphQL point)
./bin/triage-hub sync-prs --pr 73064,pingcap/tiflash#11155

# 3. Run dual-stream incremental Issue discovery + PR refresh (use --full for full re-scan, --skip-prs to skip PR step)
./bin/triage-hub sync-issues

# 4. Export untriaged pending candidates + valid enums + JSON template for AI Agent / maintainer triage
./bin/triage-hub pending --out decisions_bundle.json

# 5. Apply AI/maintainer triage decisions (`new_issues`, `exclude_issues`, `overrides`) into SQLite
#    (automatically fetches & classifies any newly linked open PRs unless --sync-new-prs=false)
./bin/triage-hub triage --file decisions.json
```
