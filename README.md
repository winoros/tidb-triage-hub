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
go test ./...

# Start standalone RW workbench (default http://127.0.0.1:8787)
./bin/triage-hub

# Start in read-only viewer mode
./bin/triage-hub --readonly

# CLI one-shot syncs
./bin/triage-hub --sync-prs-once
./bin/triage-hub --sync-issues-once
```
