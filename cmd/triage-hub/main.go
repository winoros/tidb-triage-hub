package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/jsonutil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/server"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
)

const rootUsage = `tidb-triage-hub — TiDB Optimizer Issue & PR Triage Hub

Usage:
  triage-hub [serve] [flags]      Start the HTTP web dashboard (default)
  triage-hub status [flags]       Show SQLite dataset summary, recent sync runs & gh GraphQL quota
  triage-hub sync-prs [flags]     Refresh open PR statuses (all tracked open PRs or targeted --pr specs)
  triage-hub sync-issues [flags]  Run dual-stream incremental (or --full) issue discovery + PR refresh
  triage-hub pending [flags]      Export untriaged pending candidates & decisions template for AI triage
  triage-hub triage [flags]       Apply AI/human triage decisions JSON into SQLite (and sync new PRs)

Run "triage-hub <command> -h" for command-specific flags.
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runServe(os.Args[2:])
			return
		case "status":
			runStatus(os.Args[2:])
			return
		case "sync-prs":
			runSyncPRs(os.Args[2:])
			return
		case "sync-issues":
			runSyncIssues(os.Args[2:])
			return
		case "pending":
			runPending(os.Args[2:])
			return
		case "triage":
			runTriage(os.Args[2:])
			return
		case "help", "-h", "--help":
			fmt.Fprint(os.Stderr, rootUsage)
			fmt.Fprintln(os.Stderr, "\nDefault `serve` flags:")
			fs := newServeFlagSet()
			fs.SetOutput(os.Stderr)
			fs.PrintDefaults()
			return
		}
	}
	runServe(os.Args[1:])
}

func openStoreAndSyncer(dbPath, seedPath, scriptsDir string) (*store.Store, *syncer.Syncer) {
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open sqlite db: %v", err)
	}
	if seedPath != "" {
		seeded, err := st.BootstrapIfEmpty(seedPath)
		if err != nil {
			log.Fatalf("bootstrap from %s: %v", seedPath, err)
		}
		if seeded {
			cnt, _ := st.IssueCount()
			log.Printf("bootstrapped %d candidate issues from %s", cnt, seedPath)
		}
	}
	workDir := filepath.Join(filepath.Dir(dbPath), "staging")
	sy := syncer.New(st, scriptsDir, workDir)
	return st, sy
}

func printPrettyJSON(v any) {
	b, err := jsonutil.MarshalPrettyJSON(v)
	if err != nil {
		b, _ = json.MarshalIndent(v, "", "  ")
	}
	os.Stdout.Write(b)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		fmt.Println()
	}
}

func newServeFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.String("addr", "", "HTTP listen address (default 127.0.0.1:$PORT or 127.0.0.1:8787)")
	fs.String("port", "", "HTTP listen port override")
	fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	fs.String("web-dir", "web", "Standalone frontend assets directory")
	fs.String("scripts-dir", "scripts", "Standalone sync scripts directory")
	fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	fs.Bool("readonly", false, "Serve dashboard in read-only mode (disable triage edits & sync mutations)")
	fs.Bool("sync-prs-once", false, "Fetch and classify all open PR statuses once and exit")
	fs.Bool("sync-issues-once", false, "Run dual-stream incremental issue + PR sync once and exit")
	return fs
}

func runServe(args []string) {
	fs := newServeFlagSet()
	_ = fs.Parse(args)

	getStr := func(name string) string { return fs.Lookup(name).Value.String() }
	getBool := func(name string) bool { return fs.Lookup(name).Value.(flag.Getter).Get().(bool) }

	dbPath := getStr("db")
	seedPath := getStr("seed-json")
	scriptsDir := getStr("scripts-dir")
	webDir := getStr("web-dir")
	addr := getStr("addr")
	portFlag := getStr("port")
	readOnly := getBool("readonly")
	syncPRsOnce := getBool("sync-prs-once")
	syncIssuesOnce := getBool("sync-issues-once")

	st, sy := openStoreAndSyncer(dbPath, seedPath, scriptsDir)
	defer st.Close()

	if syncIssuesOnce {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		res, err := sy.SyncIssuesDualStream(ctx, false)
		if err != nil {
			log.Fatalf("sync issues failed: %v", err)
		}
		printPrettyJSON(res)
		return
	}

	if syncPRsOnce {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		stats, err := sy.SyncPRStatuses(ctx)
		if err != nil {
			log.Fatalf("sync PRs failed: %v", err)
		}
		printPrettyJSON(stats)
		return
	}

	listenAddr := addr
	if listenAddr == "" {
		p := portFlag
		if p == "" {
			p = os.Getenv("PORT")
		}
		if p == "" {
			p = "8787"
		}
		listenAddr = "127.0.0.1:" + p
	}

	srv := server.New(st, sy, server.Options{
		WebDir:   webDir,
		ReadOnly: readOnly,
	})
	log.Printf("tidb-triage-hub listening on http://%s (web-dir=%s, readonly=%v)", listenAddr, webDir, readOnly)
	if err := http.ListenAndServe(listenAddr, srv.Handler()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}


func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	checkGH := fs.Bool("check-gh", true, "Query `gh api graphql` rateLimit (0 GraphQL point cost)")
	_ = fs.Parse(args)

	st, _ := openStoreAndSyncer(*dbPath, *seedPath, "scripts")
	defer st.Close()

	ds, err := st.BuildDataset(true)
	if err != nil {
		log.Fatalf("build dataset: %v", err)
	}
	pending, err := st.ListPendingCandidates()
	if err != nil {
		log.Fatalf("list pending candidates: %v", err)
	}
	pendNums := make([]int, 0, len(pending))
	for _, pc := range pending {
		pendNums = append(pendNums, pc.Number)
	}
	runs, _ := st.RecentSyncRuns(5)

	out := map[string]any{
		"database": map[string]any{
			"db_path":                         *dbPath,
			"total_candidates":                ds.SyncMeta.TotalCandidates,
			"canonical_candidates":            ds.SyncMeta.CanonicalCandidates,
			"real_bugs":                       ds.SyncMeta.RealBugs,
			"open_pr_count":                   ds.SyncMeta.OpenPRCount,
			"mainline_open_pr_count":          ds.SyncMeta.MainlineOpenPRCount,
			"pending_count":                   len(pending),
			"pending_numbers":                 pendNums,
			"issue_snapshot_utc8":             ds.SyncMeta.SnapshotUTC8,
			"issue_watermark_utc8":            ds.SyncMeta.MaxIssueUpdatedUTC8,
			"pr_snapshot_utc8":                ds.SyncMeta.PRSnapshotUTC8,
			"incremental_gh_search_qualifier": ds.SyncMeta.IncrementalGHSearchQualifier,
		},
		"recent_sync_runs": runs,
	}

	if *checkGH {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rl, err := prstatus.CheckGHQuota(ctx, 0)
		if err != nil {
			out["gh_check_error"] = err.Error()
		} else {
			out["gh_rate_limit"] = rl
		}
	}

	printPrettyJSON(out)
}

func runSyncPRs(args []string) {
	fs := flag.NewFlagSet("sync-prs", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	prSpec := fs.String("pr", "", "Comma-separated PR specs for targeted refresh (e.g. '73064,pingcap/tiflash#11155')")
	minQuota := fs.Int("min-quota", -1, "Minimum GraphQL points required before syncing (default 100 full, 20 targeted)")
	timeout := fs.Duration("timeout", 4*time.Minute, "Command timeout")
	_ = fs.Parse(args)

	st, sy := openStoreAndSyncer(*dbPath, *seedPath, "scripts")
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if strings.TrimSpace(*prSpec) != "" {
		targets, err := prstatus.ParsePRSpecs(*prSpec, store.DefaultRepo)
		if err != nil {
			log.Fatalf("invalid --pr spec: %v", err)
		}
		mq := *minQuota
		if mq < 0 {
			mq = 20
		}
		stats, err := sy.SyncSpecificPRs(ctx, targets, mq)
		if err != nil {
			log.Fatalf("targeted PR sync failed: %v", err)
		}
		printPrettyJSON(stats)
		return
	}

	mq := *minQuota
	if mq < 0 {
		mq = 100
	}
	stats, err := sy.SyncPRStatusesWithQuota(ctx, mq)
	if err != nil {
		log.Fatalf("sync PRs failed: %v", err)
	}
	printPrettyJSON(stats)
}

func runSyncIssues(args []string) {
	fs := flag.NewFlagSet("sync-issues", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	scriptsDir := fs.String("scripts-dir", "scripts", "Standalone sync scripts directory")
	full := fs.Bool("full", false, "Force full candidate re-verification instead of incremental search")
	skipPRs := fs.Bool("skip-prs", false, "Skip full open PR status refresh after syncing issues")
	minQuota := fs.Int("min-quota", -1, "Minimum GraphQL points required before syncing (default 250 incremental, 500 full)")
	timeout := fs.Duration("timeout", 6*time.Minute, "Command timeout")
	_ = fs.Parse(args)

	st, sy := openStoreAndSyncer(*dbPath, *seedPath, *scriptsDir)
	defer st.Close()

	mq := *minQuota
	if mq < 0 {
		if *full {
			mq = 500
		} else {
			mq = 250
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := sy.SyncIssuesWithOptions(ctx, syncer.IssueSyncOptions{
		Full:     *full,
		SkipPRs:  *skipPRs,
		MinQuota: mq,
	})
	if err != nil {
		log.Fatalf("sync issues failed: %v", err)
	}
	printPrettyJSON(res)
	if res.PendingCount > 0 {
		fmt.Fprintf(os.Stderr, "\n[triage-hub] %d untriaged issue(s) awaiting AI/maintainer triage: %v\n", res.PendingCount, res.PendingNumbers)
		fmt.Fprintf(os.Stderr, "  1. Inspect & export template: ./bin/triage-hub pending --out decisions_bundle.json\n")
		fmt.Fprintf(os.Stderr, "  2. Apply triage decisions:    ./bin/triage-hub triage --file decisions.json\n")
	}
}

func runPending(args []string) {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	onlyNum := fs.Int("number", 0, "Filter to a single pending issue number")
	outPath := fs.String("out", "", "Optional output file path (defaults to stdout)")
	_ = fs.Parse(args)

	st, _ := openStoreAndSyncer(*dbPath, *seedPath, "scripts")
	defer st.Close()

	pending, err := st.ListPendingCandidates()
	if err != nil {
		log.Fatalf("list pending candidates: %v", err)
	}
	if *onlyNum > 0 {
		filtered := make([]model.PendingCandidate, 0, 1)
		for _, pc := range pending {
			if pc.Number == *onlyNum {
				filtered = append(filtered, pc)
			}
		}
		pending = filtered
	}

	tmplNew := make(map[string]model.TriagePatchRequest, len(pending))
	for _, pc := range pending {
		tmplNew[fmt.Sprintf("%d", pc.Number)] = model.TriagePatchRequest{
			Cat:              "MEDIUM",
			Severity:         "MAJOR",
			Subsystem:        "logical_rewrite",
			IsBorderlineKept: false,
			Note: &model.LocalizedNote{
				ZhCN: "TODO: 填写中文根因与影响简述",
				En:   "TODO: Fill in concise English root-cause & impact note",
			},
		}
	}

	bundle := map[string]any{
		"pending_count": len(pending),
		"instructions":  "Prepare a JSON object with `new_issues`, `exclude_issues`, and/or `overrides`, then apply via `triage-hub triage --file <decisions.json>` (or `--stdin`). Non-bug categories (ENHANCEMENT / FILTER_*) automatically normalize severity to N/A.",
		"valid_enums": map[string]any{
			"cat":       model.ValidCategories,
			"severity":  model.ValidSeverities,
			"subsystem": model.ValidSubsystems,
		},
		"decisions_template": model.BatchTriageDecisions{
			NewIssues:     tmplNew,
			ExcludeIssues: map[string]string{},
			Overrides:     map[string]model.TriagePatchRequest{},
		},
		"pending_candidates": pending,
	}

	b, err := jsonutil.MarshalPrettyJSON(bundle)
	if err != nil {
		log.Fatalf("marshal pending bundle: %v", err)
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, b, 0o644); err != nil {
			log.Fatalf("write %s: %v", *outPath, err)
		}
		log.Printf("wrote %d pending candidate(s) to %s", len(pending), *outPath)
		return
	}
	os.Stdout.Write(b)
}

func hasPlaceholderNote(n *model.LocalizedNote) bool {
	if n == nil {
		return false
	}
	return strings.Contains(n.ZhCN, "TODO:") || strings.Contains(n.En, "TODO:")
}

func runTriage(args []string) {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	filePath := fs.String("file", "", "Path to triage decisions JSON file")
	useStdin := fs.Bool("stdin", false, "Read triage decisions JSON from stdin")
	syncNewPRs := fs.Bool("sync-new-prs", true, "Automatically fetch & classify open PRs linked to newly accepted issues")
	minQuota := fs.Int("min-quota", 20, "Minimum GraphQL points required when --sync-new-prs triggers")
	timeout := fs.Duration("timeout", 2*time.Minute, "Command timeout")
	_ = fs.Parse(args)

	if (*filePath == "" && !*useStdin) || (*filePath != "" && *useStdin) {
		log.Fatalf("specify exactly one of --file <decisions.json> or --stdin")
	}

	var raw []byte
	var err error
	if *useStdin {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*filePath)
	}
	if err != nil {
		log.Fatalf("read decisions input: %v", err)
	}

	var dec model.BatchTriageDecisions
	if err := json.Unmarshal(raw, &dec); err != nil {
		log.Fatalf("invalid decisions JSON: %v", err)
	}
	if len(dec.NewIssues) == 0 && len(dec.ExcludeIssues) == 0 && len(dec.Overrides) == 0 {
		var wrapped struct {
			DecisionsTemplate *model.BatchTriageDecisions `json:"decisions_template"`
		}
		if json.Unmarshal(raw, &wrapped) == nil && wrapped.DecisionsTemplate != nil {
			dec = *wrapped.DecisionsTemplate
		}
	}
	if len(dec.NewIssues) == 0 && len(dec.ExcludeIssues) == 0 && len(dec.Overrides) == 0 {
		log.Fatalf("decisions JSON contains no entries in new_issues, exclude_issues, or overrides")
	}
	for k, v := range dec.NewIssues {
		if hasPlaceholderNote(v.Note) {
			log.Fatalf("new_issues[%q] still contains placeholder 'TODO:' note; fill in actual triage analysis first", k)
		}
	}
	for k, v := range dec.Overrides {
		if hasPlaceholderNote(v.Note) {
			log.Fatalf("overrides[%q] still contains placeholder 'TODO:' note; fill in actual triage analysis first", k)
		}
	}

	st, sy := openStoreAndSyncer(*dbPath, *seedPath, "scripts")
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := sy.ApplyTriageDecisions(ctx, dec, *syncNewPRs, *minQuota)
	if err != nil {
		log.Fatalf("apply triage decisions: %v", err)
	}
	printPrettyJSON(res)
}
