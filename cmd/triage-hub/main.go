package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/server"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
)

func main() {
	var (
		addr           = flag.String("addr", "", "HTTP listen address (default 127.0.0.1:$PORT or 127.0.0.1:8787)")
		portFlag       = flag.String("port", "", "HTTP listen port override")
		dbPath         = flag.String("db", "data/tidb_triage.db", "SQLite database file path")
		webDir         = flag.String("web-dir", "web", "Standalone frontend assets directory")
		scriptsDir     = flag.String("scripts-dir", "scripts", "Standalone sync scripts directory")
		seedPath       = flag.String("seed-json", "seed/initial_dataset.json", "Initial seed dataset JSON used only when SQLite DB is empty")
		readOnly       = flag.Bool("readonly", false, "Serve dashboard in read-only mode (disable triage edits & sync mutations)")
		syncPRsOnce    = flag.Bool("sync-prs-once", false, "Fetch and classify all open PR statuses once and exit")
		syncIssuesOnce = flag.Bool("sync-issues-once", false, "Run dual-stream incremental issue + PR sync once and exit")
	)
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open sqlite db: %v", err)
	}
	defer st.Close()

	seeded, err := st.BootstrapIfEmpty(*seedPath)
	if err != nil {
		log.Fatalf("bootstrap from %s: %v", *seedPath, err)
	}
	if seeded {
		cnt, _ := st.IssueCount()
		log.Printf("bootstrapped %d candidate issues from %s", cnt, *seedPath)
	}

	workDir := filepath.Join(filepath.Dir(*dbPath), "staging")
	sy := syncer.New(st, *scriptsDir, workDir)

	if *syncIssuesOnce {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		res, err := sy.SyncIssuesDualStream(ctx, false)
		if err != nil {
			log.Fatalf("sync issues failed: %v", err)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}

	if *syncPRsOnce {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		stats, err := sy.SyncPRStatuses(ctx)
		if err != nil {
			log.Fatalf("sync PRs failed: %v", err)
		}
		b, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Println(string(b))
		return
	}

	listenAddr := *addr
	if listenAddr == "" {
		p := *portFlag
		if p == "" {
			p = os.Getenv("PORT")
		}
		if p == "" {
			p = "8787"
		}
		listenAddr = "127.0.0.1:" + p
	}

	srv := server.New(st, sy, server.Options{
		WebDir:   *webDir,
		ReadOnly: *readOnly,
	})
	log.Printf("tidb-triage-hub listening on http://%s (web-dir=%s, readonly=%v)", listenAddr, *webDir, *readOnly)
	if err := http.ListenAndServe(listenAddr, srv.Handler()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
