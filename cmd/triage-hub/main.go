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
	"strconv"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/jsonutil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/server"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

const rootUsage = `tidb-triage-hub — TiDB Optimizer Issue & PR Triage Hub

Usage:
  triage-hub [serve] [flags]      Start the HTTP web dashboard (default)
  triage-hub status [flags]       Show SQLite dataset summary, queues & gh GraphQL quota
  triage-hub sync-prs [flags]     Refresh open PR statuses (all tracked open PRs or targeted --pr specs)
  triage-hub sync-issues [flags]  Run pure-Go dual-stream incremental (or --full) issue + PR sync
  triage-hub pending [flags]      Export pending / re-triage / note-backfill bundle with Rubric & PR clues
  triage-hub triage [flags]       Validate & apply AI/human triage decisions into SQLite

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

func openStoreAndSyncer(dbPath, seedPath string) (*store.Store, *syncer.Syncer) {
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
	sy := syncer.New(st)
	return st, sy
}

func openStoreAndSyncerMaybeDryRun(dbPath, seedPath string, dryRun bool) (*store.Store, *syncer.Syncer, func()) {
	if dryRun {
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			tmpFile, err := os.CreateTemp("", "triage-hub-dryrun-*.db")
			if err != nil {
				log.Fatalf("create temp sqlite db for dry-run: %v", err)
			}
			tmpPath := tmpFile.Name()
			_ = tmpFile.Close()
			st, sy := openStoreAndSyncer(tmpPath, seedPath)
			cleanup := func() {
				_ = st.Close()
				_ = os.Remove(tmpPath)
				_ = os.Remove(tmpPath + "-wal")
				_ = os.Remove(tmpPath + "-shm")
			}
			return st, sy, cleanup
		}
	}
	st, sy := openStoreAndSyncer(dbPath, seedPath)
	return st, sy, func() { _ = st.Close() }
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

func parseIssueNumCSV(csv string) ([]int, error) {
	var out []int
	seen := make(map[int]bool)
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimPrefix(strings.TrimSpace(tok), "#")
		if tok == "" {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid issue number %q", tok)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

func newServeFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.String("addr", "", "HTTP listen address (default 127.0.0.1:$PORT or 127.0.0.1:8787)")
	fs.String("port", "", "HTTP listen port override")
	fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	fs.String("web-dir", "web", "Standalone frontend assets directory")
	fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	fs.String("scripts-dir", "", "Deprecated (no-op): sync is now pure Go")
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
	webDir := getStr("web-dir")
	addr := getStr("addr")
	portFlag := getStr("port")
	readOnly := getBool("readonly")
	syncPRsOnce := getBool("sync-prs-once")
	syncIssuesOnce := getBool("sync-issues-once")

	st, sy := openStoreAndSyncer(dbPath, seedPath)
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
	skipQuota := fs.Bool("skip-quota", false, "Skip querying `gh api graphql` rateLimit")
	_ = fs.Parse(args)

	st, sy := openStoreAndSyncer(*dbPath, *seedPath)
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
	retriageNums := ds.SyncMeta.RetriageIssueNums
	if retriageNums == nil {
		retriageNums = []int{}
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
			"retriage_count":                  len(retriageNums),
			"retriage_numbers":                retriageNums,
			"excluded_count":                  len(ds.SyncMeta.ExcludedIssueNums),
			"issue_snapshot_utc8":             ds.SyncMeta.SnapshotUTC8,
			"issue_watermark_utc8":            ds.SyncMeta.MaxIssueUpdatedUTC8,
			"pr_snapshot_utc8":                ds.SyncMeta.PRSnapshotUTC8,
			"incremental_gh_search_qualifier": ds.SyncMeta.IncrementalGHSearchQualifier,
		},
		"recent_sync_runs": runs,
	}

	if *checkGH && !*skipQuota {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rl, err := ghgql.CheckQuota(ctx, sy.Runner(), 0)
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

	st, sy := openStoreAndSyncer(*dbPath, *seedPath)
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if strings.TrimSpace(*prSpec) != "" {
		targets, err := prstatus.ParsePRSpecs(*prSpec, store.DefaultRepo)
		if err != nil {
			log.Fatalf("invalid --pr spec: %v", err)
		}
		for i := range targets {
			targets[i].TrackSources = []string{"manual_cli"}
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
	full := fs.Bool("full", false, "Force full candidate re-verification instead of incremental search")
	skipPRs := fs.Bool("skip-prs", false, "Skip full open PR status refresh after syncing issues")
	dryRun := fs.Bool("dry-run", false, "Compute dual-stream sync plan and print summary without modifying SQLite")
	since := fs.String("since", "", "Optional explicit RFC3339 UTC watermark override (e.g. 2026-11-12T00:00:00Z)")
	overlap := fs.Int("overlap", 15, "Safety overlap minutes subtracted from previous snapshot_utc")
	maxIncDays := fs.Int("max-incremental-days", 14, "Auto-switch to --full when snapshot age exceeds N days")
	lookbackDays := fs.Int("untriaged-lookback-days", issuesync.DefaultUntriagedLookbackDays, "Rolling lookback days for Tier-2 untriaged issue discovery")
	minQuota := fs.Int("min-quota", -1, "Minimum GraphQL points required before syncing (default 150 incremental, 350 full)")
	fs.String("scripts-dir", "", "Deprecated (no-op): sync is now pure Go")
	timeout := fs.Duration("timeout", 6*time.Minute, "Command timeout")
	_ = fs.Parse(args)

	_, sy, cleanup := openStoreAndSyncerMaybeDryRun(*dbPath, *seedPath, *dryRun)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := sy.SyncIssuesWithOptions(ctx, syncer.IssueSyncOptions{
		Full:                  *full,
		SkipPRs:               *skipPRs || *dryRun,
		DryRun:                *dryRun,
		Since:                 *since,
		OverlapMinutes:        *overlap,
		MaxIncrementalDays:    *maxIncDays,
		UntriagedLookbackDays: *lookbackDays,
		MinQuota:              *minQuota,
	})
	if err != nil {
		log.Fatalf("sync issues failed: %v", err)
	}
	printPrettyJSON(res)
	if res.PendingCount > 0 || res.RetriageCount > 0 {
		fmt.Fprintf(os.Stderr, "\n[triage-hub] Actionable items — pending: %d %v, retriage: %d %v\n",
			res.PendingCount, res.PendingNumbers, res.RetriageCount, res.RetriageNumbers)
		fmt.Fprintf(os.Stderr, "  1. Inspect bundle: ./bin/triage-hub pending --compact\n")
		fmt.Fprintf(os.Stderr, "  2. Apply triage:   ./bin/triage-hub triage --stdin\n")
	}
}

func runPending(args []string) {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	onlyNum := fs.Int("number", 0, "Filter to a single pending issue number")
	numbersCSV := fs.String("numbers", "", "Comma-separated pending issue numbers to export")
	scopeFilter := fs.String("scope", "", "Filter pending candidates whose matched_scopes contain this substring")
	onlyPending := fs.Bool("only-pending", false, "Export only new pending candidates (omit flagged re-triage items)")
	onlyRetriage := fs.Bool("only-retriage", false, "Export only flagged re-triage candidates")
	reTriageCSV := fs.String("re-triage", "", "Comma-separated existing issue numbers to inspect for recalibration")
	backfillNotes := fs.Int("backfill-notes", 0, "Export top N active issues missing bilingual notes for backfill")
	catFilter := fs.String("cat", "", "Category filter when using --backfill-notes (e.g. HARD, MEDIUM, EASY)")
	compact := fs.Bool("compact", false, "Omit few-shot examples from embedded triage_rubric to save tokens")
	enrichPRs := fs.Bool("enrich-prs", true, "Fetch PR changed file stats via gh GraphQL when missing")
	outPath := fs.String("out", "", "Optional output file path (defaults to stdout)")
	_ = fs.Parse(args)

	st, sy := openStoreAndSyncer(*dbPath, *seedPath)
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Mode A: Backfill notes for existing issues lacking root-cause notes
	if *backfillNotes > 0 {
		recs, err := st.ListBackfillNoteCandidates(*backfillNotes, *catFilter)
		if err != nil {
			log.Fatalf("list backfill candidates: %v", err)
		}
		var prDetailMap map[string]model.PendingPRDetail
		if *enrichPRs && len(recs) <= 30 {
			var allPRs []model.LinkedPRRef
			for _, r := range recs {
				allPRs = append(allPRs, r.PRs...)
			}
			if len(allPRs) > 0 {
				prDetailMap, _ = triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, allPRs)
			}
		}

		type backfillItem struct {
			Number      int                     `json:"number"`
			URL         string                  `json:"url"`
			Title       string                  `json:"title"`
			IsCanonical bool                    `json:"is_canonical"`
			IsCustomer  bool                    `json:"is_customer"`
			Cat         string                  `json:"cat"`
			Severity    string                  `json:"severity"`
			Subsystem   string                  `json:"subsystem"`
			Labels      []string                `json:"labels"`
			PRs         []model.LinkedPRRef     `json:"prs"`
			PRDetails   []model.PendingPRDetail `json:"pr_details,omitempty"`
		}
		items := make([]backfillItem, 0, len(recs))
		tmplOvr := make(map[string]model.TriagePatchRequest, len(recs))
		for _, r := range recs {
			var dets []model.PendingPRDetail
			for _, p := range r.PRs {
				if d, ok := prDetailMap[model.PRKey(p.Repo, p.Num)]; ok {
					dets = append(dets, d)
				}
			}
			items = append(items, backfillItem{
				Number:      r.Number,
				URL:         fmt.Sprintf("https://github.com/pingcap/tidb/issues/%d", r.Number),
				Title:       r.Title,
				IsCanonical: r.IsCanonical,
				IsCustomer:  r.IsCustomer,
				Cat:         r.Cat,
				Severity:    r.Severity,
				Subsystem:   r.Subsystem,
				Labels:      r.Labels,
				PRs:         r.PRs,
				PRDetails:   dets,
			})
			tmplOvr[strconv.Itoa(r.Number)] = model.TriagePatchRequest{
				Cat:              r.Cat,
				Severity:         r.Severity,
				Subsystem:        r.Subsystem,
				IsBorderlineKept: r.IsBorderlineKept,
				ForceKeep:        r.ForceKeep,
				Note: &model.LocalizedNote{
					ZhCN: "TODO: 填写中文根因与影响简述",
					En:   "TODO: Fill in concise English root-cause & impact note",
				},
			}
		}

		bundle := map[string]any{
			"mode":            "backfill_notes",
			"backfill_count":  len(items),
			"instructions":    "Inspect each issue & linked PR diff (`gh issue view <num> --repo pingcap/tidb --comments`, `gh pr diff <pr> --repo pingcap/tidb`), fill in `overrides` notes, and apply via `triage-hub triage --stdin --skip-todo`.",
			"triage_rubric":   triage.DefaultRubric(*compact),
			"backfill_issues": items,
			"decisions_template": model.BatchTriageDecisions{
				Overrides: tmplOvr,
			},
		}
		writeBundleOutput(bundle, *outPath, len(items))
		return
	}

	// Filter sets
	wantPendingNums := make(map[int]bool)
	if *onlyNum > 0 {
		wantPendingNums[*onlyNum] = true
	}
	if strings.TrimSpace(*numbersCSV) != "" {
		nums, err := parseIssueNumCSV(*numbersCSV)
		if err != nil {
			log.Fatalf("--numbers: %v", err)
		}
		for _, n := range nums {
			wantPendingNums[n] = true
		}
	}

	var specificRetriageNums []int
	if strings.TrimSpace(*reTriageCSV) != "" {
		var err error
		specificRetriageNums, err = parseIssueNumCSV(*reTriageCSV)
		if err != nil {
			log.Fatalf("--re-triage: %v", err)
		}
	}

	skipPending := *onlyRetriage || (len(specificRetriageNums) > 0 && len(wantPendingNums) == 0)
	skipRetriage := *onlyPending || (len(wantPendingNums) > 0 && len(specificRetriageNums) == 0)

	var pending []model.PendingCandidate
	if !skipPending {
		var err error
		pending, err = st.ListPendingCandidates()
		if err != nil {
			log.Fatalf("list pending candidates: %v", err)
		}
		if len(wantPendingNums) > 0 || strings.TrimSpace(*scopeFilter) != "" {
			sf := strings.ToLower(strings.TrimSpace(*scopeFilter))
			filtered := make([]model.PendingCandidate, 0, len(pending))
			for _, pc := range pending {
				if len(wantPendingNums) > 0 && !wantPendingNums[pc.Number] {
					continue
				}
				if sf != "" {
					matched := false
					for _, sc := range pc.MatchedScopes {
						if strings.Contains(strings.ToLower(sc), sf) {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}
				filtered = append(filtered, pc)
			}
			pending = filtered
		}
	}

	// Enrich pending candidates if any lack SuggestedTriage or up-to-date PRDetails
	needSavePending := false
	for i := range pending {
		if pending[i].SuggestedTriage == nil || (*enrichPRs && triage.CandidateNeedsPREnrichment(pending[i])) {
			needSavePending = true
			break
		}
	}
	if needSavePending && len(pending) > 0 {
		enriched, _ := triage.EnrichAndAnalyzePending(ctx, sy.Runner(), nil, pending, *enrichPRs)
		pending = enriched
		_ = st.UpdatePendingCandidates(pending)
	}

	var retriageList []model.RetriageCandidate
	if !skipRetriage {
		var err error
		if len(specificRetriageNums) > 0 {
			retriageList, err = st.ListRetriageCandidates(false, specificRetriageNums)
		} else {
			retriageList, err = st.ListRetriageCandidates(true, nil)
		}
		if err != nil {
			log.Fatalf("list retriage candidates: %v", err)
		}
		if len(retriageList) > 0 {
			var allPRs []model.LinkedPRRef
			if *enrichPRs {
				for _, rc := range retriageList {
					allPRs = append(allPRs, rc.PRs...)
				}
			}
			var detMap map[string]model.PendingPRDetail
			if len(allPRs) > 0 {
				detMap, _ = triage.FetchPendingPRDetails(ctx, sy.Runner(), nil, allPRs)
			}
			for i := range retriageList {
				hasActiveOrMerged := false
				for _, p := range retriageList[i].PRs {
					if !p.IsCherryPick && (p.State == "OPEN" || p.State == "DRAFT" || p.State == "MERGED") {
						hasActiveOrMerged = true
						break
					}
				}
				for _, p := range retriageList[i].PRs {
					if p.IsCherryPick || (hasActiveOrMerged && p.State == "CLOSED") {
						continue
					}
					if d, ok := detMap[model.PRKey(p.Repo, p.Num)]; ok {
						retriageList[i].PRDetails = append(retriageList[i].PRDetails, d)
					}
				}
				retriageList[i].Clues = triage.ExtractClues(
					retriageList[i].Title, retriageList[i].Body, retriageList[i].Labels,
					retriageList[i].RecentComments, retriageList[i].PRDetails,
				)
				retriageList[i].SuggestedTriage = triage.SuggestTriage(
					retriageList[i].Title, retriageList[i].Labels, retriageList[i].PRs,
					retriageList[i].PRDetails, retriageList[i].Clues,
				)
			}
		}
	}
	if pending == nil {
		pending = []model.PendingCandidate{}
	}
	if retriageList == nil {
		retriageList = []model.RetriageCandidate{}
	}

	tmplNew := make(map[string]model.TriagePatchRequest, len(pending))
	suggestedExcludesHint := make(map[string]string)
	for _, pc := range pending {
		cat := "MEDIUM"
		sev := "MAJOR"
		subsys := "logical_rewrite"
		borderline := false
		if pc.SuggestedTriage != nil {
			cat = pc.SuggestedTriage.Cat
			sev = pc.SuggestedTriage.Severity
			subsys = pc.SuggestedTriage.Subsystem
			borderline = pc.SuggestedTriage.IsBorderlineKept
			if pc.SuggestedTriage.RecommendedAction == "exclude" && pc.SuggestedTriage.ExcludeReason != "" {
				suggestedExcludesHint[strconv.Itoa(pc.Number)] = pc.SuggestedTriage.ExcludeReason
			}
		}
		tmplNew[strconv.Itoa(pc.Number)] = model.TriagePatchRequest{
			Cat:              cat,
			Severity:         sev,
			Subsystem:        subsys,
			IsBorderlineKept: borderline,
			Note: &model.LocalizedNote{
				ZhCN: "TODO: 填写中文根因与影响简述",
				En:   "TODO: Fill in concise English root-cause & impact note",
			},
		}
	}

	tmplOvr := make(map[string]model.TriagePatchRequest)
	for _, rc := range retriageList {
		if len(specificRetriageNums) > 0 {
			tmplOvr[strconv.Itoa(rc.Number)] = model.TriagePatchRequest{
				Cat:              rc.CurrentCat,
				Severity:         rc.CurrentSeverity,
				Subsystem:        rc.CurrentSubsys,
				IsBorderlineKept: rc.IsBorderlineKept,
				ForceKeep:        rc.ForceKeep,
				Note: &model.LocalizedNote{
					ZhCN: "TODO: 填写重审后的中文根因与影响简述（原备注见 retriage_candidates[].current_note）",
					En:   "TODO: Fill in recalibrated English root-cause & impact note (see retriage_candidates[].current_note)",
				},
			}
		}
	}

	bundle := map[string]any{
		"pending_count":  len(pending),
		"retriage_count": len(retriageList),
		"instructions":   "Review `pending_candidates` and `retriage_candidates` against `triage_rubric`. Submit decisions via `triage-hub triage --stdin` (supports `new_issues`, `exclude_issues`, `overrides`, `ack_retriage`, `manual_pr_links`, `--skip-todo`, and `--dry-run`). Canonical (`sig/planner` + `type/bug`) issues must NEVER be put in `exclude_issues`.",
		"valid_enums": map[string]any{
			"cat":       model.ValidCategories,
			"severity":  model.ValidSeverities,
			"subsystem": model.ValidSubsystems,
		},
		"triage_rubric": triage.DefaultRubric(*compact),
		"decisions_template": model.BatchTriageDecisions{
			NewIssues:     tmplNew,
			ExcludeIssues: map[string]string{},
			Overrides:     tmplOvr,
			AckRetriage:   model.FlexIntList{},
		},
		"pending_candidates":  pending,
		"retriage_candidates": retriageList,
	}
	if len(suggestedExcludesHint) > 0 {
		bundle["suggested_excludes_hint"] = suggestedExcludesHint
	}

	writeBundleOutput(bundle, *outPath, len(pending)+len(retriageList))
}

func writeBundleOutput(bundle any, outPath string, count int) {
	b, err := jsonutil.MarshalPrettyJSON(bundle)
	if err != nil {
		log.Fatalf("marshal bundle: %v", err)
	}
	if outPath != "" {
		if err := os.WriteFile(outPath, b, 0o644); err != nil {
			log.Fatalf("write %s: %v", outPath, err)
		}
		log.Printf("wrote %d item(s) to %s", count, outPath)
		return
	}
	os.Stdout.Write(b)
}

func runTriage(args []string) {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	dbPath := fs.String("db", "data/tidb_triage.db", "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	filePath := fs.String("file", "", "Path to triage decisions JSON file")
	useStdin := fs.Bool("stdin", false, "Read triage decisions JSON from stdin")
	excludeCSV := fs.String("exclude", "", "Quickly exclude comma-separated non-canonical issue numbers without JSON")
	excludeReason := fs.String("reason", "Excluded non-optimizer issue via CLI", "Reason used with --exclude")
	ackCSV := fs.String("ack-retriage", "", "Quickly acknowledge re-triage on comma-separated issue numbers")
	skipTodo := fs.Bool("skip-todo", false, "Skip incomplete entries whose note still starts with 'TODO:'")
	allowEmptyNote := fs.Bool("allow-empty-note", false, "Allow empty notes when applying triage")
	strictNotes := fs.Bool("strict-notes", false, "Fail on soft note length/borderline warnings")
	dryRun := fs.Bool("dry-run", false, "Validate decisions JSON and preview actions without writing to SQLite")
	syncNewPRs := fs.Bool("sync-new-prs", true, "Automatically fetch & classify open PRs linked to newly accepted issues")
	minQuota := fs.Int("min-quota", 20, "Minimum GraphQL points required when --sync-new-prs triggers")
	timeout := fs.Duration("timeout", 2*time.Minute, "Command timeout")
	_ = fs.Parse(args)

	var dec model.BatchTriageDecisions
	hasQuickFlags := strings.TrimSpace(*excludeCSV) != "" || strings.TrimSpace(*ackCSV) != ""
	hasJSONInput := *filePath != "" || *useStdin

	if !hasQuickFlags && !hasJSONInput {
		log.Fatalf("specify --stdin, --file <decisions.json>, --exclude <nums>, or --ack-retriage <nums>")
	}
	if *filePath != "" && *useStdin {
		log.Fatalf("specify at most one of --file or --stdin")
	}

	if hasJSONInput {
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
		if err := json.Unmarshal(raw, &dec); err != nil {
			log.Fatalf("invalid decisions JSON: %v", err)
		}
		if len(dec.NewIssues) == 0 && len(dec.ExcludeIssues) == 0 && len(dec.Overrides) == 0 && len(dec.AckRetriage) == 0 && len(dec.ManualPRLinks) == 0 {
			var wrapped struct {
				DecisionsTemplate *model.BatchTriageDecisions `json:"decisions_template"`
			}
			if json.Unmarshal(raw, &wrapped) == nil && wrapped.DecisionsTemplate != nil {
				dec = *wrapped.DecisionsTemplate
			}
		}
	}

	if strings.TrimSpace(*excludeCSV) != "" {
		nums, err := parseIssueNumCSV(*excludeCSV)
		if err != nil {
			log.Fatalf("--exclude: %v", err)
		}
		if dec.ExcludeIssues == nil {
			dec.ExcludeIssues = make(map[string]string)
		}
		for _, n := range nums {
			dec.ExcludeIssues[strconv.Itoa(n)] = *excludeReason
		}
	}
	if strings.TrimSpace(*ackCSV) != "" {
		nums, err := parseIssueNumCSV(*ackCSV)
		if err != nil {
			log.Fatalf("--ack-retriage: %v", err)
		}
		dec.AckRetriage = append(dec.AckRetriage, nums...)
	}

	cleaned, warnings, err := triage.ValidateAndPrepareDecisions(dec, triage.ValidateOptions{
		SkipTodo:       *skipTodo,
		AllowEmptyNote: *allowEmptyNote,
		StrictNotes:    *strictNotes,
	})
	if err != nil {
		log.Fatalf("triage validation failed: %v", err)
	}

	if len(cleaned.NewIssues) == 0 &&
		len(cleaned.ExcludeIssues) == 0 &&
		len(cleaned.Overrides) == 0 &&
		len(cleaned.AckRetriage) == 0 &&
		len(cleaned.ManualPRLinks) == 0 {
		log.Fatalf("no actionable entries remaining after validation (check if all entries were skipped by --skip-todo)")
	}

	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "[warn] issue #%d (%s): %s\n", w.Issue, w.Field, w.Message)
	}

	if *dryRun {
		dbChecked := false
		if *dbPath != "" {
			if _, statErr := os.Stat(*dbPath); statErr == nil {
				st, openErr := store.Open(*dbPath)
				if openErr != nil {
					log.Fatalf("open sqlite db for dry-run check: %v", openErr)
				}
				valErr := st.ValidateBatchDecisionsReadOnly(cleaned)
				_ = st.Close()
				if valErr != nil {
					log.Fatalf("triage database validation failed: %v", valErr)
				}
				dbChecked = true
			} else if os.IsNotExist(statErr) && *seedPath != "" {
				if _, seedStatErr := os.Stat(*seedPath); seedStatErr == nil {
					st, _, cleanup := openStoreAndSyncerMaybeDryRun(*dbPath, *seedPath, true)
					valErr := st.ValidateBatchDecisionsReadOnly(cleaned)
					cleanup()
					if valErr != nil {
						log.Fatalf("triage database validation failed: %v", valErr)
					}
					dbChecked = true
				}
			}
		}
		printPrettyJSON(map[string]any{
			"dry_run":             true,
			"db_validated":        dbChecked,
			"new_issues_count":    len(cleaned.NewIssues),
			"exclude_count":       len(cleaned.ExcludeIssues),
			"overrides_count":     len(cleaned.Overrides),
			"ack_retriage_count":  len(cleaned.AckRetriage),
			"manual_pr_links":     len(cleaned.ManualPRLinks),
			"validation_warnings": warnings,
			"prepared_decisions":  cleaned,
		})
		return
	}

	st, sy := openStoreAndSyncer(*dbPath, *seedPath)
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := sy.ApplyTriageDecisions(ctx, cleaned, *syncNewPRs, *minQuota)
	if err != nil {
		log.Fatalf("apply triage decisions: %v", err)
	}
	printPrettyJSON(res)
}
