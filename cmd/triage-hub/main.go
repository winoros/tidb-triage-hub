package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/httputil"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/jsonutil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/server"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
	seedassets "github.com/winoros/tidb-triage-hub/seed"
)

const rootUsage = `tidb-triage-hub — TiDB Optimizer Issue & PR Triage Hub

Usage:
  triage-hub [serve] [flags]      Start the HTTP web dashboard (default)
  triage-hub status [flags]       Show SQLite dataset summary, queues & GitHub GraphQL quota
  triage-hub sync-prs [flags]     Refresh open PR statuses (all tracked open PRs or targeted --pr specs)
  triage-hub sync-issues [flags]  Run pure-Go dual-stream incremental (or --full) issue + PR sync
  triage-hub pending [flags]      Export pending / re-triage / note-backfill bundle with Rubric & PR clues
  triage-hub triage [flags]       Validate & apply AI/human triage decisions (local SQLite or --remote)
  triage-hub backup [flags]       Create an online SQLite backup snapshot (from --remote or local --db)

Remote / Split-Deployment Environment Variables:
  TRIAGE_HUB_URL                  Remote Machine A base URL with explicit http:// or https:// scheme
  TRIAGE_HUB_TOKEN                Shared maintainer token (server auth on ` + "`serve`" + `; Bearer token on CLI)
  TRIAGE_HUB_ALLOW_INSECURE_HTTP  Set to 1 to allow sending token over non-loopback plain http://
  GH_TOKEN / GITHUB_TOKEN         GitHub PAT for pure-Go GraphQL sync without requiring ` + "`gh`" + ` CLI binary

Run "triage-hub <command> -h" for command-specific flags.
`

func main() {
	mustRun(dispatchCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func dispatchCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return executeServe(args[1:], stdout)
		case "status":
			return executeStatus(args[1:], stdout)
		case "sync-prs":
			return executeSyncPRs(args[1:], stdout)
		case "sync-issues":
			return executeSyncIssues(args[1:], stdout, stderr)
		case "pending":
			return executePending(args[1:], stdout, stderr)
		case "triage":
			return executeTriage(args[1:], stdin, stdout, stderr)
		case "backup":
			return executeBackup(args[1:], stdout)
		case "help", "-h", "--help":
			fmt.Fprint(stderr, rootUsage)
			fmt.Fprintln(stderr, "\nDefault `serve` flags:")
			fs := newServeFlagSet()
			fs.SetOutput(stderr)
			fs.PrintDefaults()
			return nil
		default:
			if !strings.HasPrefix(args[0], "-") {
				return fmt.Errorf("unknown command %q (run 'triage-hub --help' for usage)", args[0])
			}
		}
	}
	return executeServe(args, stdout)
}

func mustRun(err error) {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	log.Fatalf("%v", err)
}

func isFlagExplicit(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// resolveSeedFallback returns (seedPath, fallbackBytes). When --seed-json was explicitly passed
// on the CLI, embedded seed fallback is disabled so `--seed-json ""` yields an empty DB and
// `--seed-json /missing.json` fails fast instead of silently seeding embedded data.
func resolveSeedFallback(fs *flag.FlagSet, seedPath string) (string, []byte) {
	if fs != nil && isFlagExplicit(fs, "seed-json") {
		return seedPath, nil
	}
	return seedPath, seedassets.InitialDatasetJSON
}

func openStoreAndSyncerWithFallback(dbPath, seedPath string, fallbackJSON []byte) (*store.Store, *syncer.Syncer, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite db: %w", err)
	}
	seeded, err := st.BootstrapIfEmptyWithFallback(seedPath, fallbackJSON)
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("bootstrap dataset: %w", err)
	}
	if seeded {
		cnt, _ := st.IssueCount()
		src := "embedded seed"
		if seedPath != "" {
			if _, statErr := os.Stat(seedPath); statErr == nil {
				src = seedPath
			}
		}
		log.Printf("bootstrapped %d candidate issues from %s", cnt, src)
	}
	sy := syncer.New(st)
	return st, sy, nil
}

func openStoreAndSyncerMaybeDryRun(dbPath, seedPath string, fallbackJSON []byte, dryRun bool) (*store.Store, *syncer.Syncer, func(), error) {
	if dryRun {
		st, cleanup, err := store.OpenSnapshotForDryRun(dbPath, seedPath, fallbackJSON)
		if err != nil {
			return nil, nil, nil, err
		}
		return st, syncer.New(st), cleanup, nil
	}
	st, sy, err := openStoreAndSyncerWithFallback(dbPath, seedPath, fallbackJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	return st, sy, func() { _ = st.Close() }, nil
}

func writePrettyJSONTo(w io.Writer, v any) {
	b, err := jsonutil.MarshalPrettyJSON(v)
	if err != nil {
		b, _ = json.MarshalIndent(v, "", "  ")
	}
	_, _ = w.Write(b)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		_, _ = fmt.Fprintln(w)
	}
}

func newServeFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.String("addr", strings.TrimSpace(os.Getenv("TRIAGE_HUB_ADDR")), "HTTP listen address (default 127.0.0.1:$PORT or 127.0.0.1:8787)")
	fs.String("port", "", "HTTP listen port override")
	fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "SQLite database file path")
	fs.String("web-dir", "web", "Frontend assets directory (falls back to embedded assets if absent)")
	fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON (falls back to embedded seed if absent)")
	fs.String("scripts-dir", "", "Deprecated (no-op): sync is now pure Go")
	fs.String("auth-token", strings.TrimSpace(os.Getenv("TRIAGE_HUB_TOKEN")), "Shared maintainer token required for RW mutations & remote CLI (env: TRIAGE_HUB_TOKEN)")
	fs.String("github-client-id", os.Getenv("TRIAGE_HUB_GITHUB_CLIENT_ID"), "GitHub OAuth app client ID")
	fs.String("github-client-secret", os.Getenv("TRIAGE_HUB_GITHUB_CLIENT_SECRET"), "GitHub OAuth app client secret")
	fs.String("github-callback-url", os.Getenv("TRIAGE_HUB_GITHUB_CALLBACK_URL"), "GitHub OAuth callback URL (https://host/auth/github/callback)")
	fs.String("github-maintainer-ids", os.Getenv("TRIAGE_HUB_GITHUB_MAINTAINER_IDS"), "Comma-separated additional GitHub user IDs permitted to modify data")
	fs.Int64("github-owner-id", 7846227, "GitHub user ID permitted to modify data (default: winoros)")
	fs.Bool("trust-proxy", envBoolDefault("TRIAGE_HUB_TRUST_PROXY", false), "Trust rightmost X-Forwarded-Proto / X-Forwarded-Host hop from loopback/private reverse proxies (env: TRIAGE_HUB_TRUST_PROXY)")
	fs.String("trusted-proxy-cidrs", envOrDefault("TRIAGE_HUB_TRUSTED_PROXY_CIDRS", ""), "Optional comma-separated proxy CIDRs/IPs trusted when --trust-proxy is set (env: TRIAGE_HUB_TRUSTED_PROXY_CIDRS)")
	fs.Bool("readonly", false, "Serve dashboard in read-only mode (disable all triage edits & sync mutations)")
	fs.Bool("sync-prs-once", false, "Fetch and classify all open PR statuses once and exit")
	fs.Bool("sync-issues-once", false, "Run dual-stream incremental issue + PR sync once and exit")
	fs.Duration("sync-prs-interval", envDurationDefault("TRIAGE_HUB_SYNC_PRS_INTERVAL"), "Optional background open PR status sync interval (e.g. 30m; env: TRIAGE_HUB_SYNC_PRS_INTERVAL)")
	fs.Duration("sync-issues-interval", envDurationDefault("TRIAGE_HUB_SYNC_ISSUES_INTERVAL"), "Optional background incremental issue sync interval (e.g. 2h; env: TRIAGE_HUB_SYNC_ISSUES_INTERVAL)")
	return fs
}

func startBackgroundSyncLoop(ctx context.Context, wg *sync.WaitGroup, sy *syncer.Syncer, prsInterval, issuesInterval time.Duration) {
	if prsInterval > 0 {
		log.Printf("background PR status sync enabled (interval=%s)", prsInterval)
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := time.NewTicker(prsInterval)
			defer tk.Stop()
			minAge := prsInterval / 2
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
					runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
					stats, ran, err := sy.SyncPRStatusesIfStale(runCtx, minAge)
					cancel()
					if err != nil {
						if !errors.Is(err, context.Canceled) {
							log.Printf("[bg-sync-prs] error: %v", err)
						}
					} else if ran {
						log.Printf("[bg-sync-prs] refreshed %d open PRs (%d GraphQL calls, %dms)", stats.OpenCount, stats.GraphQLCalls, stats.DurationMs)
					}
				}
			}
		}()
	}
	if issuesInterval > 0 {
		log.Printf("background incremental issue sync enabled (interval=%s)", issuesInterval)
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := time.NewTicker(issuesInterval)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
					runCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
					res, err := sy.SyncIssuesDualStream(runCtx, false)
					cancel()
					if err != nil {
						if !errors.Is(err, context.Canceled) {
							log.Printf("[bg-sync-issues] error: %v", err)
						}
					} else {
						log.Printf("[bg-sync-issues] mode=%s pending=%d retriage=%d", res.ModeUsed, res.PendingCount, res.RetriageCount)
					}
				}
			}
		}()
	}
}

func executeServe(args []string, stdout io.Writer) error {
	fs := newServeFlagSet()
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for serve: %v", fs.Args())
	}

	getStr := func(name string) string { return fs.Lookup(name).Value.String() }
	getBool := func(name string) bool { return fs.Lookup(name).Value.(flag.Getter).Get().(bool) }
	getDur := func(name string) time.Duration { return fs.Lookup(name).Value.(flag.Getter).Get().(time.Duration) }

	dbPath := getStr("db")
	seedPath, seedFallback := resolveSeedFallback(fs, getStr("seed-json"))
	webDir := getStr("web-dir")
	addr := getStr("addr")
	portFlag := getStr("port")
	authToken := strings.TrimSpace(getStr("auth-token"))
	github := server.GitHubOptions{
		ClientID:     strings.TrimSpace(getStr("github-client-id")),
		ClientSecret: strings.TrimSpace(getStr("github-client-secret")),
		CallbackURL:  strings.TrimSpace(getStr("github-callback-url")),
		OwnerID:      fs.Lookup("github-owner-id").Value.(flag.Getter).Get().(int64),
	}
	maintainerIDs, err := server.ParseGitHubMaintainerIDs(getStr("github-maintainer-ids"))
	if err != nil {
		return fmt.Errorf("--github-maintainer-ids: %w", err)
	}
	github.MaintainerIDs = maintainerIDs
	if err := github.Validate(); err != nil {
		return err
	}
	trustProxy := getBool("trust-proxy")
	trustedProxyCIDRs := strings.TrimSpace(getStr("trusted-proxy-cidrs"))
	if trustedProxyCIDRs != "" {
		if _, err := httputil.ParseCIDRList(trustedProxyCIDRs); err != nil {
			return fmt.Errorf("--trusted-proxy-cidrs: %w", err)
		}
	}
	readOnly := getBool("readonly")
	syncPRsOnce := getBool("sync-prs-once")
	syncIssuesOnce := getBool("sync-issues-once")
	syncPRsInterval := getDur("sync-prs-interval")
	syncIssuesInterval := getDur("sync-issues-interval")

	st, sy, err := openStoreAndSyncerWithFallback(dbPath, seedPath, seedFallback)
	if err != nil {
		return err
	}
	defer st.Close()

	if syncIssuesOnce {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		res, err := sy.SyncIssuesDualStream(ctx, false)
		if err != nil {
			return fmt.Errorf("sync issues failed: %w", err)
		}
		writePrettyJSONTo(stdout, res)
		return nil
	}

	if syncPRsOnce {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		stats, err := sy.SyncPRStatuses(ctx)
		if err != nil {
			return fmt.Errorf("sync PRs failed: %w", err)
		}
		writePrettyJSONTo(stdout, stats)
		return nil
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

	if !readOnly && authToken == "" && !github.Enabled() {
		log.Printf("[auth] listening on %s without authentication configured; HTTP modifications are disabled", listenAddr)
	}

	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()

	bgCtx, bgCancel := context.WithCancel(sigCtx)
	var bgWG sync.WaitGroup
	if !readOnly {
		startBackgroundSyncLoop(bgCtx, &bgWG, sy, syncPRsInterval, syncIssuesInterval)
	}

	srv := server.New(st, sy, server.Options{
		GitHub:            github,
		WebDir:            webDir,
		ReadOnly:          readOnly,
		AuthToken:         authToken,
		DBDisplay:         dbPath,
		TrustProxy:        trustProxy,
		TrustedProxyCIDRs: trustedProxyCIDRs,
	})
	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("tidb-triage-hub listening on http://%s (db=%s, readonly=%v, token-auth=%v)", listenAddr, dbPath, readOnly, authToken != "")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case srvErr := <-errCh:
		bgCancel()
		bgWG.Wait()
		if srvErr != nil {
			return fmt.Errorf("server error: %w", srvErr)
		}
	case <-sigCtx.Done():
		log.Printf("shutting down server...")
		bgCancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = httpSrv.Shutdown(shutCtx)
		shutCancel()
		<-errCh
		bgWG.Wait()
	}
	return nil
}

func executeStatus(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dbPath := fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	checkGH := fs.Bool("check-gh", true, "Query GitHub GraphQL rateLimit (0 GraphQL point cost)")
	skipQuota := fs.Bool("skip-quota", false, "Skip querying GitHub GraphQL rateLimit")
	rem := bindRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for status: %v", fs.Args())
	}

	wantGH := *checkGH && !*skipQuota
	if rem.enabled() {
		rc, err := newRemoteClient(*rem, 30*time.Second)
		if err != nil {
			return err
		}
		out, err := rc.fetchStatus(context.Background(), wantGH)
		if err != nil {
			return err
		}
		writePrettyJSONTo(stdout, out)
		return nil
	}

	sp, fb := resolveSeedFallback(fs, *seedPath)
	st, sy, err := openStoreAndSyncerWithFallback(*dbPath, sp, fb)
	if err != nil {
		return err
	}
	defer st.Close()

	out, err := sy.BuildStatusReport(context.Background(), *dbPath, wantGH)
	if err != nil {
		return err
	}
	writePrettyJSONTo(stdout, out)
	return nil
}

func executeSyncPRs(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sync-prs", flag.ContinueOnError)
	dbPath := fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	prSpec := fs.String("pr", "", "Comma-separated PR specs for targeted refresh (e.g. '73064,pingcap/tiflash#11155')")
	minQuota := fs.Int("min-quota", -1, "Minimum GraphQL points required before syncing (default 100 full, 20 targeted)")
	timeout := fs.Duration("timeout", 4*time.Minute, "Command timeout")
	rem := bindRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for sync-prs: %v", fs.Args())
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be > 0")
	}

	if rem.enabled() {
		rc, err := newRemoteClient(*rem, *timeout)
		if err != nil {
			return err
		}
		stats, err := rc.syncPRs(context.Background(), *prSpec, *minQuota)
		if err != nil {
			return fmt.Errorf("remote sync PRs failed: %w", err)
		}
		writePrettyJSONTo(stdout, stats)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sp, fb := resolveSeedFallback(fs, *seedPath)
	st, sy, err := openStoreAndSyncerWithFallback(*dbPath, sp, fb)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := sy.ExecuteSyncPRs(ctx, *prSpec, *minQuota)
	if err != nil {
		return fmt.Errorf("sync PRs failed: %w", err)
	}
	writePrettyJSONTo(stdout, stats)
	return nil
}

func executeSyncIssues(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sync-issues", flag.ContinueOnError)
	dbPath := fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "SQLite database file path")
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
	rem := bindRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for sync-issues: %v", fs.Args())
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be > 0")
	}

	opts := syncer.IssueSyncOptions{
		Full:                  *full,
		SkipPRs:               *skipPRs || *dryRun,
		DryRun:                *dryRun,
		Since:                 *since,
		OverlapMinutes:        *overlap,
		MaxIncrementalDays:    *maxIncDays,
		UntriagedLookbackDays: *lookbackDays,
		MinQuota:              *minQuota,
	}

	var (
		res syncer.IssueSyncResult
		err error
	)
	if rem.enabled() {
		rc, rcErr := newRemoteClient(*rem, *timeout)
		if rcErr != nil {
			return rcErr
		}
		res, err = rc.syncIssues(context.Background(), opts)
		if err != nil {
			return fmt.Errorf("remote sync issues failed: %w", err)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		sp, fb := resolveSeedFallback(fs, *seedPath)
		_, sy, cleanup, openErr := openStoreAndSyncerMaybeDryRun(*dbPath, sp, fb, *dryRun)
		if openErr != nil {
			return openErr
		}
		defer cleanup()
		res, err = sy.SyncIssuesWithOptions(ctx, opts)
		if err != nil {
			return fmt.Errorf("sync issues failed: %w", err)
		}
	}

	writePrettyJSONTo(stdout, res)
	if res.PendingCount > 0 || res.RetriageCount > 0 {
		fmt.Fprintf(stderr, "\n[triage-hub] Actionable items — pending: %d %v, retriage: %d %v\n",
			res.PendingCount, res.PendingNumbers, res.RetriageCount, res.RetriageNumbers)
		fmt.Fprintf(stderr, "  1. Inspect bundle: ./bin/triage-hub pending --compact\n")
		fmt.Fprintf(stderr, "  2. Apply triage:   ./bin/triage-hub triage --stdin\n")
	}
	return nil
}

func executePending(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pending", flag.ContinueOnError)
	dbPath := fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "SQLite database file path")
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
	enrichPRs := fs.Bool("enrich-prs", true, "Fetch PR changed file stats via GitHub GraphQL when missing")
	outPath := fs.String("out", "", "Optional output file path (defaults to stdout)")
	timeout := fs.Duration("timeout", 90*time.Second, "Command timeout")
	rem := bindRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for pending: %v", fs.Args())
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be > 0")
	}
	if *onlyNum < 0 {
		return fmt.Errorf("--number must be >= 0")
	}
	if *backfillNotes < 0 {
		return fmt.Errorf("--backfill-notes must be >= 0")
	}

	var numbers []int
	if strings.TrimSpace(*numbersCSV) != "" {
		var err error
		numbers, err = model.ParseIssueNumCSV(*numbersCSV)
		if err != nil {
			return fmt.Errorf("--numbers: %w", err)
		}
	}
	var reTriageNums []int
	if strings.TrimSpace(*reTriageCSV) != "" {
		var err error
		reTriageNums, err = model.ParseIssueNumCSV(*reTriageCSV)
		if err != nil {
			return fmt.Errorf("--re-triage: %w", err)
		}
	}

	opts := syncer.PendingBundleOptions{
		OnlyNum:       *onlyNum,
		Numbers:       numbers,
		ScopeFilter:   *scopeFilter,
		OnlyPending:   *onlyPending,
		OnlyRetriage:  *onlyRetriage,
		ReTriageNums:  reTriageNums,
		BackfillNotes: *backfillNotes,
		CatFilter:     *catFilter,
		Compact:       *compact,
		EnrichPRs:     *enrichPRs,
	}

	var (
		bundle map[string]any
		count  int
		err    error
	)
	if rem.enabled() {
		rc, rcErr := newRemoteClient(*rem, *timeout)
		if rcErr != nil {
			return rcErr
		}
		bundle, count, err = rc.fetchPendingBundle(context.Background(), opts)
		if err != nil {
			return err
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		sp, fb := resolveSeedFallback(fs, *seedPath)
		st, sy, openErr := openStoreAndSyncerWithFallback(*dbPath, sp, fb)
		if openErr != nil {
			return openErr
		}
		defer st.Close()
		bundle, count, err = sy.BuildPendingBundle(ctx, opts)
		if err != nil {
			return err
		}
	}

	b, err := jsonutil.MarshalPrettyJSON(bundle)
	if err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, b, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *outPath, err)
		}
		fmt.Fprintf(stderr, "wrote %d item(s) to %s\n", count, *outPath)
		return nil
	}
	_, _ = stdout.Write(b)
	return nil
}

func executeTriage(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	dbPath := fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "SQLite database file path")
	seedPath := fs.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
	filePath := fs.String("file", "", "Path to triage decisions JSON file")
	useStdin := fs.Bool("stdin", false, "Read triage decisions JSON from stdin")
	excludeCSV := fs.String("exclude", "", "Quickly exclude comma-separated non-canonical issue numbers without JSON")
	excludeReason := fs.String("reason", "Excluded non-optimizer issue via CLI", "Reason used with --exclude")
	ackCSV := fs.String("ack-retriage", "", "Quickly acknowledge re-triage on comma-separated issue numbers")
	skipTodo := fs.Bool("skip-todo", false, "Skip incomplete entries whose note still starts with 'TODO:'")
	allowEmptyNote := fs.Bool("allow-empty-note", false, "Allow empty notes when applying triage")
	strictNotes := fs.Bool("strict-notes", false, "Fail on soft note length/borderline warnings")
	skipConflicts := fs.Bool("skip-conflicts", false, "Skip items modified concurrently (e.g. via Web UI) instead of failing batch (defaults to true with --remote)")
	dryRun := fs.Bool("dry-run", false, "Validate decisions JSON and preview actions without writing to SQLite")
	syncNewPRs := fs.Bool("sync-new-prs", false, "Deprecated: submissions do not fetch GitHub; refresh PRs before exporting decisions")
	minQuota := fs.Int("min-quota", 20, "Deprecated: retained for command-line compatibility")
	autoBackup := fs.Bool("auto-backup", envBoolDefault("TRIAGE_HUB_AUTO_BACKUP", true), "Automatically download a local SQLite backup after remote triage succeeds (env: TRIAGE_HUB_AUTO_BACKUP)")
	backupPath := fs.String("backup-path", envOrDefault("TRIAGE_HUB_BACKUP_PATH", "data/remote_backup.db"), "Destination path for automatic post-triage remote SQLite backup")
	timeout := fs.Duration("timeout", 2*time.Minute, "Command timeout")
	rem := bindRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for triage: %v (use --file <path>)", fs.Args())
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be > 0")
	}

	effectiveSkipConflicts := *skipConflicts
	if rem.enabled() && !isFlagExplicit(fs, "skip-conflicts") {
		effectiveSkipConflicts = true
	}

	var dec model.BatchTriageDecisions
	hasQuickFlags := strings.TrimSpace(*excludeCSV) != "" || strings.TrimSpace(*ackCSV) != ""
	hasJSONInput := *filePath != "" || *useStdin

	if !hasQuickFlags && !hasJSONInput {
		return fmt.Errorf("specify --stdin, --file <decisions.json>, --exclude <nums>, or --ack-retriage <nums>")
	}
	if *filePath != "" && *useStdin {
		return fmt.Errorf("specify at most one of --file or --stdin")
	}

	if hasJSONInput {
		var raw []byte
		var err error
		if *useStdin {
			raw, err = io.ReadAll(stdin)
		} else {
			raw, err = os.ReadFile(*filePath)
		}
		if err != nil {
			return fmt.Errorf("read decisions input: %w", err)
		}
		if err := json.Unmarshal(raw, &dec); err != nil {
			return fmt.Errorf("invalid decisions JSON: %w", err)
		}
		if len(dec.NewIssues) == 0 && len(dec.ExcludeIssues) == 0 && len(dec.Overrides) == 0 && len(dec.Notes) == 0 && len(dec.AckRetriage) == 0 && len(dec.ManualPRLinks) == 0 {
			var wrapped struct {
				DecisionsTemplate *model.BatchTriageDecisions `json:"decisions_template"`
			}
			if json.Unmarshal(raw, &wrapped) == nil && wrapped.DecisionsTemplate != nil {
				dec = *wrapped.DecisionsTemplate
			}
		}
	}

	if strings.TrimSpace(*excludeCSV) != "" {
		nums, err := model.ParseIssueNumCSV(*excludeCSV)
		if err != nil {
			return fmt.Errorf("--exclude: %w", err)
		}
		if dec.ExcludeIssues == nil {
			dec.ExcludeIssues = make(map[string]string)
		}
		for _, n := range nums {
			dec.ExcludeIssues[strconv.Itoa(n)] = *excludeReason
		}
	}
	if strings.TrimSpace(*ackCSV) != "" {
		nums, err := model.ParseIssueNumCSV(*ackCSV)
		if err != nil {
			return fmt.Errorf("--ack-retriage: %w", err)
		}
		dec.AckRetriage = append(dec.AckRetriage, nums...)
	}

	// Local pre-flight syntax & rubric validation (fast-fail before network or SQLite)
	req := syncer.BatchTriageExecuteRequest{
		Decisions: dec, SkipTodo: *skipTodo, AllowEmptyNote: *allowEmptyNote,
		StrictNotes: *strictNotes, SkipConflicts: effectiveSkipConflicts,
		DryRun: *dryRun, SyncNewPRs: syncNewPRs, MinQuota: minQuota,
	}

	prepared, err := syncer.PrepareBatchTriage(req)
	if err != nil {
		return fmt.Errorf("triage validation failed: %w", err)
	}
	for _, w := range prepared.SyntaxPreview().ValidationWarnings {
		fmt.Fprintf(stderr, "[warn] issue #%d (%s): %s\n", w.Issue, w.Field, w.Message)
	}
	if rem.enabled() {
		rc, err := newRemoteClient(*rem, *timeout)
		if err != nil {
			return err
		}
		out, err := rc.postTriageDecisions(context.Background(), req)
		if err != nil {
			return fmt.Errorf("apply remote triage decisions: %w", err)
		}
		logSkippedConflictsFromMap(stderr, out)
		if prWarn, ok := out["pr_sync_warning"].(string); ok && prWarn != "" {
			fmt.Fprintf(stderr, "[warn] %s\n", prWarn)
		}
		writePrettyJSONTo(stdout, out)

		if !*dryRun && *autoBackup && strings.TrimSpace(*backupPath) != "" && hasCommittedRemoteMutations(out) {
			bakTimeout := *timeout
			if bakTimeout < 2*time.Minute {
				bakTimeout = 2 * time.Minute
			}
			bakRC, bakRCErr := newRemoteClient(*rem, bakTimeout)
			if bakRCErr != nil {
				bakRC = rc
			}
			nBytes, bakErr := bakRC.downloadBackup(context.Background(), *backupPath, *dbPath)
			if bakErr != nil {
				fmt.Fprintf(stderr, "[warn] post-triage remote backup failed: %v\n", bakErr)
			} else {
				fmt.Fprintf(stderr, "[backup] saved remote SQLite snapshot (%d bytes) to %s\n", nBytes, *backupPath)
			}
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *dryRun {
		dbExists := false
		if resolvedDB := store.ResolveLocalDBPath(*dbPath); resolvedDB != "" {
			if _, statErr := os.Stat(resolvedDB); statErr == nil {
				dbExists = true
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("stat sqlite db %s: %w", resolvedDB, statErr)
			}
		}
		explicitSeedPath := ""
		if isFlagExplicit(fs, "seed-json") {
			explicitSeedPath = strings.TrimSpace(*seedPath)
		}
		if dbExists || explicitSeedPath != "" {
			sp, fb := resolveSeedFallback(fs, *seedPath)
			_, sy, cleanup, openErr := openStoreAndSyncerMaybeDryRun(*dbPath, sp, fb, true)
			if openErr != nil {
				return fmt.Errorf("open dataset for dry-run check: %w", openErr)
			}
			out, execErr := sy.ExecutePreparedBatchTriage(ctx, prepared)
			cleanup()
			if execErr != nil {
				return fmt.Errorf("triage database validation failed: %w", execErr)
			}
			result := out.Preview

			if len(result.SkippedConflicts) > 0 {
				for _, c := range result.SkippedConflicts {
					fmt.Fprintf(stderr, "[conflict-skipped] issue #%d (%s): %s\n", c.Issue, c.Action, c.Message)
				}
			}
			writePrettyJSONTo(stdout, result)
			return nil
		}
		out := prepared.SyntaxPreview()
		writePrettyJSONTo(stdout, out)
		return nil
	}

	sp, fb := resolveSeedFallback(fs, *seedPath)
	st, sy, err := openStoreAndSyncerWithFallback(*dbPath, sp, fb)
	if err != nil {
		return err
	}
	defer st.Close()

	out, err := sy.ExecutePreparedBatchTriage(ctx, prepared)
	if err != nil {
		return fmt.Errorf("apply triage decisions: %w", err)
	}
	res := out.Applied

	for _, c := range res.SkippedConflicts {
		fmt.Fprintf(stderr, "[conflict-skipped] issue #%d (%s): %s\n", c.Issue, c.Action, c.Message)
	}
	writePrettyJSONTo(stdout, res)
	return nil
}

func logSkippedConflictsFromMap(stderr io.Writer, out map[string]any) {
	raw, ok := out["skipped_conflicts"]
	if !ok || raw == nil {
		return
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var conflicts []model.TriageConflict
	if json.Unmarshal(b, &conflicts) == nil {
		for _, c := range conflicts {
			fmt.Fprintf(stderr, "[conflict-skipped] issue #%d (%s): %s\n", c.Issue, c.Action, c.Message)
		}
	}
}

func executeBackup(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dbPath := fs.String("db", envOrDefault("TRIAGE_HUB_DB", "data/tidb_triage.db"), "Local SQLite database file path (when --remote is unset)")
	outPath := fs.String("out", "", "Destination SQLite snapshot path (default data/remote_backup.db for remote, data/tidb_triage_backup.db for local)")
	timeout := fs.Duration("timeout", 60*time.Second, "Backup download timeout")
	rem := bindRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument(s) for backup: %v", fs.Args())
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be > 0")
	}

	dest := strings.TrimSpace(*outPath)
	if rem.enabled() {
		if dest == "" {
			dest = envOrDefault("TRIAGE_HUB_BACKUP_PATH", "data/remote_backup.db")
		}
		rc, err := newRemoteClient(*rem, *timeout)
		if err != nil {
			return err
		}
		nBytes, err := rc.downloadBackup(context.Background(), dest, *dbPath)
		if err != nil {
			return err
		}
		writePrettyJSONTo(stdout, map[string]any{
			"mode":       "remote",
			"source_url": rc.baseURL,
			"backup_db":  dest,
			"size_bytes": nBytes,
		})
		return nil
	}

	if resolvedLocal := store.ResolveLocalDBPath(*dbPath); resolvedLocal != "" {
		if _, err := os.Stat(resolvedLocal); err != nil {
			return fmt.Errorf("stat source sqlite db %s: %w", resolvedLocal, err)
		}
	} else if _, err := os.Stat(*dbPath); err != nil {
		return fmt.Errorf("stat source sqlite db %s: %w", *dbPath, err)
	}
	if dest == "" {
		dest = "data/tidb_triage_backup.db"
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite db %s: %w", *dbPath, err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := st.BackupToContext(ctx, dest); err != nil {
		return err
	}
	info, _ := os.Stat(dest)
	var sz int64
	if info != nil {
		sz = info.Size()
	}
	writePrettyJSONTo(stdout, map[string]any{
		"mode":       "local",
		"source_db":  *dbPath,
		"backup_db":  dest,
		"size_bytes": sz,
	})
	return nil
}
