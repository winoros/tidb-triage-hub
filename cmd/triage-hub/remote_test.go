package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/server"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
)

type fakeGraphQLRunner struct{}

var rePullAlias = regexp.MustCompile(`p(\d+):\s*pullRequest\(number:\s*(\d+)\)`)

func (f *fakeGraphQLRunner) Query(_ context.Context, gql string, _ map[string]string) ([]byte, error) {
	if strings.Contains(gql, "...PRStatusFields") {
		matches := rePullAlias.FindAllStringSubmatch(gql, -1)
		repoMap := make(map[string]any, len(matches))
		for _, m := range matches {
			alias := "p" + m[1]
			var num int
			_, _ = fmt.Sscanf(m[2], "%d", &num)
			repoMap[alias] = map[string]any{
				"number":      num,
				"title":       fmt.Sprintf("planner: fix issue via PR #%d", num),
				"url":         fmt.Sprintf("https://github.com/pingcap/tidb/pull/%d", num),
				"baseRefName": "master",
				"state":       "OPEN",
				"isDraft":     false,
				"mergeable":   "MERGEABLE",
				"createdAt":   "2026-11-12T10:00:00Z",
				"updatedAt":   "2026-11-12T12:00:00Z",
				"author":      map[string]any{"login": "winoros"},
				"labels":      map[string]any{"nodes": []any{map[string]any{"name": "sig/planner"}, map[string]any{"name": "lgtm"}}},
			}
		}
		return json.Marshal(map[string]any{
			"data": map[string]any{
				"rateLimit": map[string]any{
					"limit":     5000,
					"cost":      1,
					"remaining": 4888,
					"resetAt":   "2026-11-13T16:00:00Z",
				},
				"repository": repoMap,
			},
		})
	}
	return json.Marshal(map[string]any{
		"data": map[string]any{
			"rateLimit": map[string]any{
				"limit":     5000,
				"cost":      0,
				"remaining": 4888,
				"resetAt":   "2026-11-13T16:00:00Z",
			},
		},
	})
}

func TestMachineBToMachineASplitDeploymentEndToEnd(t *testing.T) {
	// 1. Setup Machine A (Web Dashboard + Single SQLite DB + pure-Go GraphQL runner + Token Auth)
	dirA := t.TempDir()
	dbPathA := filepath.Join(dirA, "machine_a.db")
	stA, err := store.Open(dbPathA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stA.Close() })

	seed := model.Dataset{
		SyncMeta: model.SyncMeta{
			SchemaVersion:      2,
			SnapshotUTC:        "2026-11-12T10:00:00Z",
			SnapshotUTC8:       "2026-11-12 18:00 (UTC+8)",
			MaxIssueUpdatedUTC: "2026-11-12T09:50:00Z",
		},
		Enums: model.DatasetEnums{
			Categories: model.ValidCategories,
			Severities: model.ValidSeverities,
			Subsystems: model.ValidSubsystems,
		},
		Records: []model.IssueRecord{
			{
				Number:      72001,
				Title:       "planner: issue concurrently calibrated on Machine A Web UI",
				Author:      "alice",
				Created:     "2026-11-01",
				CreatedAt:   "2026-11-01T00:00:00Z",
				UpdatedAt:   "2026-11-12T09:50:00Z",
				Labels:      []string{"sig/planner", "type/bug"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				GHSeverity:  "major",
				Subsystem:   "logical_rewrite",
				Note:        &model.LocalizedNote{ZhCN: "初始中文备注", En: "Initial English note"},
			},
		},
	}
	if err := stA.IngestSyncedDataset(seed, []model.PendingCandidate{
		{
			Number:    72101,
			Title:     "planner: pending issue resolved on Web UI while Machine B works",
			Author:    "bob",
			CreatedAt: "2026-11-12T10:10:00Z",
			UpdatedAt: "2026-11-12T10:12:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
		{
			Number:    72102,
			Title:     "planner: pending issue accepted by Machine B with linked PR",
			Author:    "carol",
			CreatedAt: "2026-11-12T10:20:00Z",
			UpdatedAt: "2026-11-12T10:22:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	syA := syncer.NewWithRunner(stA, &fakeGraphQLRunner{})
	srvA := server.New(stA, syA, server.Options{
		WebDir:    filepath.Join(dirA, "missing_web_uses_embedded"),
		AuthToken: "split-token-123",
		DBDisplay: "machineA:/data/tidb_triage.db",
	})
	httpA := httptest.NewServer(srvA.Handler())
	defer httpA.Close()

	// 2. Setup Machine B environment (NO local SQLite DB!)
	dirB := t.TempDir()
	localShouldNotExistDB := filepath.Join(dirB, "should_not_be_created.db")
	backupDBOnB := filepath.Join(dirB, "backups", "remote_backup.db")

	t.Setenv("TRIAGE_HUB_URL", httpA.URL)
	t.Setenv("TRIAGE_HUB_TOKEN", "split-token-123")
	t.Setenv("TRIAGE_HUB_DB", localShouldNotExistDB)

	// Wrong token on Machine B is rejected with 401; missing token is rejected with 403 on pending and 401 on status?check_gh=1
	var badOut bytes.Buffer
	if err := executePending([]string{"--token", "wrong-token", "--compact", "--enrich-prs=false"}, &badOut, &badOut); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error with wrong token, got %v", err)
	}
	if err := executePending([]string{"--token", "", "--compact", "--enrich-prs=false"}, &badOut, &badOut); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 error with missing token on pending, got %v", err)
	}
	if err := executeStatus([]string{"--token", ""}, &badOut); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error with missing token on status (check_gh=1), got %v", err)
	}

	// 3. Machine B runs `triage-hub status` -> reads Machine A status & GraphQL rateLimit
	var statusStdout bytes.Buffer
	if err := executeStatus(nil, &statusStdout); err != nil {
		t.Fatalf("remote status failed: %v", err)
	}
	if !strings.Contains(statusStdout.String(), `"pending_count": 2`) || !strings.Contains(statusStdout.String(), `"remaining": 4888`) {
		t.Fatalf("unexpected remote status output: %s", statusStdout.String())
	}

	// 4. Machine B runs `triage-hub sync-prs --pr 73064` -> triggers PR refresh on Machine A
	var syncPRStdout bytes.Buffer
	if err := executeSyncPRs([]string{"--pr", "73064"}, &syncPRStdout); err != nil {
		t.Fatalf("remote sync-prs failed: %v", err)
	}
	if !strings.Contains(syncPRStdout.String(), `"open_count": 1`) {
		t.Fatalf("unexpected remote sync-prs output: %s", syncPRStdout.String())
	}

	// 5. Machine B exports `triage-hub pending --re-triage 72001 --compact --enrich-prs=false`
	//    Note: also fetch pending candidates directly via default pending call
	var pendStdout, pendStderr bytes.Buffer
	if err := executePending([]string{"--compact", "--enrich-prs=false"}, &pendStdout, &pendStderr); err != nil {
		t.Fatalf("remote pending failed: %v", err)
	}
	var exportedBundle struct {
		ExportedAtUTC     string                     `json:"exported_at_utc"`
		PendingCount      int                        `json:"pending_count"`
		DecisionsTemplate model.BatchTriageDecisions `json:"decisions_template"`
	}
	if err := json.Unmarshal(pendStdout.Bytes(), &exportedBundle); err != nil {
		t.Fatalf("unmarshal pending bundle: %v", err)
	}
	if exportedBundle.PendingCount != 2 || exportedBundle.DecisionsTemplate.BaseExportedAt == "" {
		t.Fatalf("expected pending_count=2 and non-empty BaseExportedAt, got %+v", exportedBundle)
	}
	time.Sleep(2 * time.Millisecond)

	// 6. Concurrent Human Web UI edits on Machine A WHILE Machine B's AI is working:
	//    - Login via POST /api/session/login to get maintainer cookie
	//    - Calibrate #72001 to HARD / CRITICAL
	//    - Resolve pending #72101 as HARD / CRITICAL
	loginReq, _ := http.NewRequest(http.MethodPost, httpA.URL+"/api/session/login", strings.NewReader(`{"token":"split-token-123"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil || loginResp.StatusCode != http.StatusOK {
		t.Fatalf("web login failed: status=%v err=%v", loginResp, err)
	}
	maintCookie := loginResp.Cookies()[0]
	_ = loginResp.Body.Close()

	webPatchReq, _ := http.NewRequest(http.MethodPatch, httpA.URL+"/api/issues/72001/triage", strings.NewReader(`{
		"cat":"HARD","severity":"CRITICAL","subsystem":"logical_rewrite",
		"note":{"zh-CN":"网页端维护者最新人工校准备注","en":"Fresh human calibration note from Machine A Web UI"}
	}`))
	webPatchReq.Header.Set("Content-Type", "application/json")
	webPatchReq.AddCookie(maintCookie)
	webPatchResp, err := http.DefaultClient.Do(webPatchReq)
	if err != nil || webPatchResp.StatusCode != http.StatusOK {
		t.Fatalf("web patch #72001 failed: %v", err)
	}
	_ = webPatchResp.Body.Close()

	webResReq, _ := http.NewRequest(http.MethodPost, httpA.URL+"/api/pending/72101/resolve", strings.NewReader(`{
		"action":"accept","cat":"HARD","severity":"CRITICAL","subsystem":"logical_rewrite",
		"note":{"zh-CN":"网页端维护者已先行入库","en":"Already accepted by maintainer on Machine A Web UI"}
	}`))
	webResReq.Header.Set("Content-Type", "application/json")
	webResReq.AddCookie(maintCookie)
	webResResp, err := http.DefaultClient.Do(webResReq)
	if err != nil || webResResp.StatusCode != http.StatusOK {
		t.Fatalf("web resolve #72101 failed: %v", err)
	}
	_ = webResResp.Body.Close()

	// 7. Machine B submits batch decisions (containing stale #72101 + stale override #72001 + fresh #72102 + manual PR #74500)
	decisionsJSON, _ := json.Marshal(model.BatchTriageDecisions{
		BaseExportedAt: exportedBundle.DecisionsTemplate.BaseExportedAt,
		NewIssues: map[string]model.TriagePatchRequest{
			"72101": {
				Cat:       "EASY",
				Severity:  "MINOR",
				Subsystem: "statistics",
				Note:      &model.LocalizedNote{ZhCN: "机器 B 的过期分拣结果（不应覆盖网页端）", En: "Stale Machine B result that should be skipped without failing"},
			},
			"72102": {
				Cat:       "MEDIUM",
				Severity:  "MAJOR",
				Subsystem: "statistics",
				Note:      &model.LocalizedNote{ZhCN: "统计信息直方图推导缺少空值桶边界检查导致估算偏差", En: "Missing null-bucket boundary check in histogram derivation causes cardinality misestimation"},
			},
		},
		Overrides: map[string]model.TriagePatchRequest{
			"72001": {
				Cat:       "EASY",
				Severity:  "MINOR",
				Subsystem: "logical_rewrite",
				Note:      &model.LocalizedNote{ZhCN: "机器 B 过期覆盖（不应冲掉网页端修改）", En: "Stale Machine B override that must not overwrite fresh Web UI edit"},
			},
		},
		ManualPRLinks: map[string][]string{
			"72102": {"74500"},
		},
	})

	// 7a. Remote --dry-run previews 2 skipped_conflicts cleanly and reports post-conflict planned counts (#14(1))
	var dryOut, dryErr bytes.Buffer
	if err := executeTriage([]string{"--stdin", "--dry-run"}, bytes.NewReader(decisionsJSON), &dryOut, &dryErr); err != nil {
		t.Fatalf("remote triage --dry-run failed: %v", err)
	}
	if !strings.Contains(dryOut.String(), `"skipped_conflicts"`) || !strings.Contains(dryErr.String(), "[conflict-skipped]") {
		t.Fatalf("expected conflict preview in dry-run, stdout=%s stderr=%s", dryOut.String(), dryErr.String())
	}
	if !strings.Contains(dryOut.String(), `"new_issues_count": 1`) || !strings.Contains(dryOut.String(), `"overrides_count": 0`) {
		t.Fatalf("expected dry-run counts to reflect post-conflict planned decisions (new=1, overrides=0), got: %s", dryOut.String())
	}

	// 7b. Live remote `triage` commits #72102 + links manual PR #74500, preserves Web UI #72001 & #72101, and auto-backups to backupDBOnB!
	var liveOut, liveErr bytes.Buffer
	if err := executeTriage([]string{"--stdin", "--backup-path", backupDBOnB}, bytes.NewReader(decisionsJSON), &liveOut, &liveErr); err != nil {
		t.Fatalf("remote live triage failed: %v", err)
	}
	if !strings.Contains(liveErr.String(), "[backup] saved remote SQLite snapshot") {
		t.Fatalf("expected auto-backup confirmation in stderr, got: %s", liveErr.String())
	}

	// Verify Machine B NEVER created a local working SQLite DB
	if _, statErr := os.Stat(localShouldNotExistDB); !os.IsNotExist(statErr) {
		t.Fatalf("expected Machine B never to create local DB %s", localShouldNotExistDB)
	}

	// 8. Verify downloaded backup DB on Machine B is a valid SQLite snapshot matching Machine A's state!
	bakStore, err := store.Open(backupDBOnB)
	if err != nil {
		t.Fatalf("open Machine B downloaded backup DB: %v", err)
	}
	defer bakStore.Close()
	bakDS, err := bakStore.BuildDataset(true)
	if err != nil {
		t.Fatalf("BuildDataset on backup DB: %v", err)
	}
	byNum := make(map[int]model.IssueRecord)
	for _, r := range bakDS.Records {
		byNum[r.Number] = r
	}
	if byNum[72001].Cat != "HARD" || !strings.Contains(byNum[72001].Note.En, "Fresh human calibration") {
		t.Fatalf("expected Web UI calibration on #72001 preserved, got %+v", byNum[72001])
	}
	if byNum[72101].Cat != "HARD" || !strings.Contains(byNum[72101].Note.En, "Already accepted by maintainer") {
		t.Fatalf("expected Web UI pending resolution on #72101 preserved, got %+v", byNum[72101])
	}
	if byNum[72102].Cat != "MEDIUM" || len(byNum[72102].PRs) != 1 || byNum[72102].PRs[0].Num != 74500 {
		t.Fatalf("expected Machine B #72102 accepted with linked PR #74500, got %+v", byNum[72102])
	}
	if _, hasPR74500 := bakDS.PRStatus["pingcap/tidb#74500"]; hasPR74500 {
		t.Fatal("submission unexpectedly fetched PR #74500")
	}
}

type failingPRSyncRunner struct{}

func (f *failingPRSyncRunner) Query(_ context.Context, _ string, _ map[string]string) ([]byte, error) {
	return nil, fmt.Errorf("simulated GitHub GraphQL outage during post-commit PR refresh")
}

func TestRemoteClientURLCanonicalizationBackupMagicAndPostCommitWarn(t *testing.T) {
	// 1. Subcommand -h returns flag.ErrHelp and mustRun ignores it cleanly
	var dummy bytes.Buffer
	err := executePending([]string{"-h"}, &dummy, &dummy)
	mustRun(err)

	// 2. newRemoteClient requires explicit http:// or https:// scheme, strips query/fragment,
	//    and refuses plain http:// + token on non-loopback hosts unless AllowInsecureHTTP is true.
	if _, err := newRemoteClient(remoteConfig{BaseURL: "hub.example.com/?view=prs#inbox"}, 10*time.Second); err == nil {
		t.Fatalf("expected scheme-less remote URL to be rejected")
	}
	if _, err := newRemoteClient(remoteConfig{BaseURL: "127.0.0.1:8787/#prs"}, 10*time.Second); err == nil {
		t.Fatalf("expected scheme-less loopback URL to be rejected")
	}
	if _, err := newRemoteClient(remoteConfig{BaseURL: "http://10.0.0.5:8787", Token: "secret-tok"}, 10*time.Second); err == nil || !strings.Contains(err.Error(), "allow-insecure-http") {
		t.Fatalf("expected plain http:// + token on non-loopback host to be rejected, got %v", err)
	}
	allowInsecRC, err := newRemoteClient(remoteConfig{BaseURL: "http://10.0.0.5:8787", Token: "secret-tok", AllowInsecureHTTP: true}, 10*time.Second)
	if err != nil || allowInsecRC.baseURL != "http://10.0.0.5:8787" {
		t.Fatalf("expected AllowInsecureHTTP=true to allow http://10.0.0.5:8787, got baseURL=%q err=%v", allowInsecRC.baseURL, err)
	}
	pubRC, err := newRemoteClient(remoteConfig{BaseURL: "https://hub.example.com/?view=prs#inbox"}, 10*time.Second)
	if err != nil || pubRC.baseURL != "https://hub.example.com" {
		t.Fatalf("expected https://hub.example.com, got baseURL=%q err=%v", pubRC.baseURL, err)
	}
	locRC, err := newRemoteClient(remoteConfig{BaseURL: "http://127.0.0.1:8787/#prs", Token: "loopback-ok"}, 10*time.Second)
	if err != nil || locRC.baseURL != "http://127.0.0.1:8787" {
		t.Fatalf("expected http://127.0.0.1:8787 with token allowed on loopback, got baseURL=%q err=%v", locRC.baseURL, err)
	}
	upperRC, err := newRemoteClient(remoteConfig{BaseURL: "HTTPS://hub.example.com/subpath/"}, 10*time.Second)
	if err != nil || upperRC.baseURL != "https://hub.example.com/subpath" {
		t.Fatalf("expected https://hub.example.com/subpath, got baseURL=%q err=%v", upperRC.baseURL, err)
	}

	// 3. downloadBackup rejects HTTP 200 non-SQLite HTML responses immediately after 16-byte header check
	//    without clobbering existing backup or streaming full body to disk, AND refuses overwriting active DB.
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 64; i++ {
			if _, wErr := w.Write([]byte(strings.Repeat("<html><body>SSO Login Page</body></html>\n", 256))); wErr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer htmlSrv.Close()

	tmp := t.TempDir()
	existingBak := filepath.Join(tmp, "existing.db")
	sentinel := []byte("keep-existing-backup")
	if err := os.WriteFile(existingBak, sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	rcHTML, err := newRemoteClient(remoteConfig{BaseURL: htmlSrv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rcHTML.downloadBackup(context.Background(), existingBak, ""); err == nil || !strings.Contains(err.Error(), "not a valid SQLite") {
		t.Fatalf("expected SQLite magic header error, got %v", err)
	}
	gotExisting, _ := os.ReadFile(existingBak)
	if !bytes.Equal(gotExisting, sentinel) {
		t.Fatalf("expected existing backup file untouched on invalid download")
	}
	if _, err := rcHTML.downloadBackup(context.Background(), existingBak, existingBak); err == nil || !strings.Contains(err.Error(), "refusing to overwrite local SQLite") {
		t.Fatalf("expected active DB overwrite guard error, got %v", err)
	}
	if _, err := rcHTML.downloadBackup(context.Background(), existingBak+"-wal", existingBak); err == nil || !strings.Contains(err.Error(), "refusing to overwrite local SQLite") {
		t.Fatalf("expected active DB WAL overwrite guard error, got %v", err)
	}

	// 4. Submission is independent of GitHub availability and still downloads auto-backup.
	dbPathA := filepath.Join(tmp, "post_commit_a.db")
	stA, err := store.Open(dbPathA)
	if err != nil {
		t.Fatal(err)
	}
	defer stA.Close()
	if err := stA.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
	}, []model.PendingCandidate{
		{
			Number:    77001,
			Title:     "planner: pending issue with uncached open PR",
			Author:    "alice",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:05:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
			PRs:       []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 77999, State: "OPEN"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	syA := syncer.NewWithRunner(stA, &failingPRSyncRunner{})
	srvA := server.New(stA, syA, server.Options{AuthToken: "tok-warn"})
	httpA := httptest.NewServer(srvA.Handler())
	defer httpA.Close()

	t.Setenv("TRIAGE_HUB_URL", httpA.URL)
	t.Setenv("TRIAGE_HUB_TOKEN", "tok-warn")
	bakDest := filepath.Join(tmp, "warn_backup.db")
	decBytes, _ := json.Marshal(model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"77001": {
				Cat:       "MEDIUM",
				Severity:  "MAJOR",
				Subsystem: "logical_rewrite",
				Note:      &model.LocalizedNote{ZhCN: "测试提交后PR同步降级告警但事务成功且备份照常下载", En: "Test post-commit PR sync warning keeps committed transaction and downloads backup"},
			},
		},
	})
	var outBuf, errBuf bytes.Buffer
	if err := executeTriage([]string{"--stdin", "--backup-path", bakDest}, bytes.NewReader(decBytes), &outBuf, &errBuf); err != nil {
		t.Fatalf("expected post-commit PR sync failure to degrade to warning without failing triage, got %v", err)
	}
	if strings.Contains(outBuf.String(), `"pr_sync_warning"`) || strings.Contains(errBuf.String(), "[warn]") {
		t.Fatalf("submission unexpectedly accessed GitHub: stdout=%s stderr=%s", outBuf.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "[backup] saved remote SQLite snapshot") {
		t.Fatalf("expected auto-backup to still succeed after post-commit PR warning, stderr=%s", errBuf.String())
	}
}

func TestRound2CLISubcommandGuardWarningsAndShortTimeoutBackup(t *testing.T) {
	var outBuf, errBuf bytes.Buffer

	// 1. Unknown non-flag subcommand & unexpected positional arguments are rejected fast
	if err := dispatchCLI([]string{"bogus-subcmd"}, strings.NewReader(""), &outBuf, &errBuf); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("expected unknown command error, got %v", err)
	}
	if err := executeStatus([]string{"--skip-quota", "unexpected-arg"}, &outBuf); err == nil || !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected unexpected positional argument error on status, got %v", err)
	}
	if err := executePending([]string{"unexpected-arg"}, &outBuf, &errBuf); err == nil || !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected unexpected positional argument error on pending, got %v", err)
	}
	if err := executeTriage([]string{"--stdin", "extra.json"}, strings.NewReader("{}"), &outBuf, &errBuf); err == nil || !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected unexpected positional argument error on triage, got %v", err)
	}
	if err := executeServe([]string{"unexpected-arg"}, &outBuf); err == nil || !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected unexpected positional argument error on serve, got %v", err)
	}

	// 2. An existing empty database is seeded only inside the disposable dry-run snapshot.
	tmp := t.TempDir()
	emptyOnDiskDB := filepath.Join(tmp, "existing_empty.db")
	stEmpty, err := store.Open(emptyOnDiskDB)
	if err != nil {
		t.Fatal(err)
	}
	_ = stEmpty.Close()

	stDry, _, cleanupDry, err := openStoreAndSyncerMaybeDryRun(emptyOnDiskDB, "", []byte(`{"records":[{"number":1}]}`), true)
	if err != nil {
		t.Fatalf("openStoreAndSyncerMaybeDryRun failed: %v", err)
	}
	cnt, _ := stDry.IssueCount()
	cleanupDry()
	if cnt != 1 {
		t.Fatalf("expected empty dry-run snapshot to use the same seed as normal startup, got %d issues", cnt)
	}

	// 3. Remote status --skip-quota rejects missing token with 401 (via require_auth=1),
	//    remote triage preserves validation_warnings, AND short --timeout does not abort post-triage backup!
	dbPathA := filepath.Join(tmp, "r2_remote_a.db")
	stA, err := store.Open(dbPathA)
	if err != nil {
		t.Fatal(err)
	}
	defer stA.Close()
	if err := stA.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
	}, []model.PendingCandidate{
		{
			Number:    79001,
			Title:     "planner: pending issue with short note warning",
			Author:    "alice",
			CreatedAt: "2026-11-12T10:00:00Z",
			UpdatedAt: "2026-11-12T10:05:00Z",
			Labels:    []string{"sig/planner", "type/bug"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	srvA := server.New(stA, syncer.NewWithRunner(stA, &fakeGraphQLRunner{}), server.Options{AuthToken: "r2-secret"})
	baseHandler := srvA.Handler()
	slowBackupSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/backup.db" {
			// Exceeds triage --timeout 150ms, proving post-triage backup uses its own 45s bakCtx!
			time.Sleep(260 * time.Millisecond)
		}
		baseHandler.ServeHTTP(w, r)
	}))
	defer slowBackupSrv.Close()

	t.Setenv("TRIAGE_HUB_URL", slowBackupSrv.URL)
	t.Setenv("TRIAGE_HUB_TOKEN", "r2-secret")

	if err := executeStatus([]string{"--skip-quota", "--token", ""}, &outBuf); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 on remote status --skip-quota without token, got %v", err)
	}

	shortNoteDec, _ := json.Marshal(model.BatchTriageDecisions{
		NewIssues: map[string]model.TriagePatchRequest{
			"79001": {
				Cat:       "EASY",
				Severity:  "MINOR",
				Subsystem: "statistics",
				Note:      &model.LocalizedNote{ZhCN: "简短注", En: "Short"},
			},
		},
	})
	outBuf.Reset()
	errBuf.Reset()
	bakPath := filepath.Join(tmp, "slow_backup.db")
	if err := executeTriage([]string{"--stdin", "--timeout", "150ms", "--backup-path", bakPath}, bytes.NewReader(shortNoteDec), &outBuf, &errBuf); err != nil {
		t.Fatalf("remote triage with short --timeout failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), `"validation_warnings"`) {
		t.Fatalf("expected validation_warnings preserved in remote triage stdout, got: %s", outBuf.String())
	}
	if !strings.Contains(errBuf.String(), "[backup] saved remote SQLite snapshot") {
		t.Fatalf("expected post-triage backup to succeed despite short --timeout 150ms, stderr=%s", errBuf.String())
	}
}

func TestConvergenceURLForceQueryTimeoutGraceAndNegativeFlags(t *testing.T) {
	// 1. Bare trailing '?' (ForceQuery) is stripped cleanly from BaseURL & joined endpoints
	for _, rawURL := range []string{"https://hub.example.com?", "https://hub.example.com/?", "https://hub.example.com/subpath/?"} {
		rc, err := newRemoteClient(remoteConfig{BaseURL: rawURL}, 10*time.Second)
		if err != nil {
			t.Fatalf("newRemoteClient(%q) unexpected error: %v", rawURL, err)
		}
		if strings.Contains(rc.baseURL, "?") {
			t.Fatalf("expected ForceQuery '?' stripped from rc.baseURL for %q, got %q", rawURL, rc.baseURL)
		}
	}

	// 2. Negative integer flags fail consistently in both local and remote pending CLI
	var outBuf, errBuf bytes.Buffer
	for _, badArgs := range [][]string{
		{"--number", "-1"},
		{"--backfill-notes", "-3"},
		{"--remote", "http://127.0.0.1:1", "--number", "-1"},
		{"--remote", "http://127.0.0.1:1", "--backfill-notes", "-3"},
	} {
		if err := executePending(badArgs, &outBuf, &errBuf); err == nil {
			t.Fatalf("expected error for pending %v, got nil", badArgs)
		}
	}

	// 3. Remote client applies +15s transport grace over --timeout AND propagates X-Triage-Timeout-Ms on backup!
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "grace.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := server.New(st, syncer.NewWithRunner(st, &fakeGraphQLRunner{}), server.Options{AuthToken: "grace-tok"})
	baseHandler := srv.Handler()
	var gotBackupTimeoutHdr string
	graceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/backup.db" {
			gotBackupTimeoutHdr = r.Header.Get("X-Triage-Timeout-Ms")
			// Sleep longer than --timeout 100ms to prove client transport gives +15s grace!
			time.Sleep(180 * time.Millisecond)
		}
		baseHandler.ServeHTTP(w, r)
	}))
	defer graceSrv.Close()

	t.Setenv("TRIAGE_HUB_URL", graceSrv.URL+"?")
	t.Setenv("TRIAGE_HUB_TOKEN", "grace-tok")

	bakOut := filepath.Join(tmp, "grace_backup.db")
	outBuf.Reset()
	if err := executeBackup([]string{"--timeout", "100ms", "--out", bakOut}, &outBuf); err != nil {
		t.Fatalf("expected remote backup with +15s client grace and trailing '?' URL to succeed, got %v", err)
	}
	if gotBackupTimeoutHdr != "100" {
		t.Fatalf("expected X-Triage-Timeout-Ms=100 propagated on /api/admin/backup.db, got %q", gotBackupTimeoutHdr)
	}

	outBuf.Reset()
	bakOut10m := filepath.Join(tmp, "grace_backup_10m.db")
	if err := executeBackup([]string{"--timeout", "10m", "--out", bakOut10m}, &outBuf); err != nil {
		t.Fatalf("expected remote backup --timeout 10m to succeed, got %v", err)
	}
	if gotBackupTimeoutHdr != "600000" {
		t.Fatalf("expected X-Triage-Timeout-Ms=600000 for --timeout 10m, got %q", gotBackupTimeoutHdr)
	}
}

func TestLocalDryRunWithoutDBAndBackfillNotesIntentPreservesRetriage(t *testing.T) {
	tmp := t.TempDir()

	// 1. Local triage --dry-run with missing --db does NOT seed embedded initial_dataset.json
	missingDB := filepath.Join(tmp, "nonexistent.db")
	t.Setenv("TRIAGE_HUB_URL", "")
	t.Setenv("TRIAGE_HUB_TOKEN", "")
	t.Setenv("TRIAGE_HUB_DB", missingDB)

	dryDecBytes, _ := json.Marshal(model.BatchTriageDecisions{
		Overrides: map[string]model.TriagePatchRequest{
			"999999": {
				Cat:       "HARD",
				Severity:  "MAJOR",
				Subsystem: "logical_rewrite",
				Note:      &model.LocalizedNote{ZhCN: "代价模型外连接基数推导未折算空值过滤导致选错索引", En: "Outer-join cost derivation misses null filter factor causing wrong index choice"},
			},
		},
	})
	var dryOut, dryErr bytes.Buffer
	if err := executeTriage([]string{"--stdin", "--dry-run"}, bytes.NewReader(dryDecBytes), &dryOut, &dryErr); err != nil {
		t.Fatalf("expected offline local --dry-run without DB to succeed without seeding embedded dataset, got %v", err)
	}
	if !strings.Contains(dryOut.String(), `"db_validated": false`) {
		t.Fatalf("expected db_validated=false when local DB does not exist, got %s", dryOut.String())
	}

	// 2. Setup Machine A with an active issue #81001 that is missing notes AND has needs_retriage=true
	dbPathA := filepath.Join(tmp, "notes_a.db")
	stA, err := store.Open(dbPathA)
	if err != nil {
		t.Fatal(err)
	}
	defer stA.Close()
	if err := stA.IngestSyncedDataset(model.Dataset{
		SyncMeta: model.SyncMeta{SchemaVersion: 2, SnapshotUTC: "2026-11-12T10:00:00Z"},
		Records: []model.IssueRecord{
			{
				Number:      81001,
				Title:       "planner: missing bilingual note while flagged for retriage",
				Author:      "alice",
				Created:     "2026-11-01",
				CreatedAt:   "2026-11-01T00:00:00Z",
				UpdatedAt:   "2026-11-12T09:50:00Z",
				Labels:      []string{"sig/planner", "type/bug", "severity/major"},
				IsCanonical: true,
				Cat:         "MEDIUM",
				Severity:    "MAJOR",
				GHSeverity:  "major",
				Subsystem:   "logical_rewrite",
				PRs:         []model.LinkedPRRef{{Repo: "pingcap/tidb", Num: 81999, State: "OPEN"}},
			},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// Trigger needs_retriage on #81001 via PR #81999 transitioning OPEN -> MERGED after baseline
	postBaseTS := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	if err := stA.UpsertPRStatuses([]model.PRStatus{
		{
			Repo:      "pingcap/tidb",
			Num:       81999,
			State:     "MERGED",
			UpdatedAt: postBaseTS,
			MergedAt:  postBaseTS,
		},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	srvA := server.New(stA, syncer.NewWithRunner(stA, &fakeGraphQLRunner{}), server.Options{AuthToken: "notes-tok"})
	httpA := httptest.NewServer(srvA.Handler())
	defer httpA.Close()

	t.Setenv("TRIAGE_HUB_URL", httpA.URL)
	t.Setenv("TRIAGE_HUB_TOKEN", "notes-tok")

	// 2a. `pending --re-triage 81001` leaves decisions_template.overrides empty ({})
	var rtOut, rtErr bytes.Buffer
	if err := executePending([]string{"--re-triage", "81001", "--compact", "--enrich-prs=false"}, &rtOut, &rtErr); err != nil {
		t.Fatalf("pending --re-triage failed: %v", err)
	}
	var rtBundle struct {
		DecisionsTemplate model.BatchTriageDecisions `json:"decisions_template"`
	}
	if err := json.Unmarshal(rtOut.Bytes(), &rtBundle); err != nil {
		t.Fatal(err)
	}
	if len(rtBundle.DecisionsTemplate.Overrides) != 0 {
		t.Fatalf("expected empty decisions_template.overrides for --re-triage, got %+v", rtBundle.DecisionsTemplate.Overrides)
	}

	// 2b. `pending --backfill-notes 5` emits decisions_template.notes (NOT overrides)
	var bfOut, bfErr bytes.Buffer
	if err := executePending([]string{"--backfill-notes", "5", "--compact", "--enrich-prs=false"}, &bfOut, &bfErr); err != nil {
		t.Fatalf("pending --backfill-notes failed: %v", err)
	}
	var bfBundle struct {
		BackfillCount     int                        `json:"backfill_count"`
		DecisionsTemplate model.BatchTriageDecisions `json:"decisions_template"`
	}
	if err := json.Unmarshal(bfOut.Bytes(), &bfBundle); err != nil {
		t.Fatal(err)
	}
	if bfBundle.BackfillCount != 1 || len(bfBundle.DecisionsTemplate.Notes) != 1 || len(bfBundle.DecisionsTemplate.Overrides) != 0 {
		t.Fatalf("expected backfill_count=1 in decisions_template.notes and 0 overrides, got %+v", bfBundle)
	}

	// 2c. Applying `notes` via remote `triage` updates bilingual note WITHOUT clearing needs_retriage,
	//     and still triggers post-triage auto-backup!
	bfBundle.DecisionsTemplate.Notes["81001"] = model.NotePatchRequest{
		ZhCN:              "补充根因说明：外连接谓词下推未保留空值拒绝属性导致结果错误",
		En:                "Backfilled root cause: outer-join predicate pushdown drops null-rejecting check causing wrong result",
		IfUnmodifiedSince: bfBundle.DecisionsTemplate.Notes["81001"].IfUnmodifiedSince,
	}
	wrappedNotesPayload, _ := json.Marshal(map[string]any{
		"decisions_template": bfBundle.DecisionsTemplate,
	})
	bakNotesPath := filepath.Join(tmp, "notes_backup.db")
	var trOut, trErr bytes.Buffer
	if err := executeTriage([]string{"--stdin", "--backup-path", bakNotesPath}, bytes.NewReader(wrappedNotesPayload), &trOut, &trErr); err != nil {
		t.Fatalf("remote triage with notes failed: %v", err)
	}
	if !strings.Contains(trOut.String(), `"noted_issues"`) || !strings.Contains(trErr.String(), "[backup] saved remote SQLite snapshot") {
		t.Fatalf("expected noted_issues and auto-backup on notes triage, stdout=%s stderr=%s", trOut.String(), trErr.String())
	}

	dsAfter, err := stA.BuildDataset(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsAfter.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(dsAfter.Records))
	}
	rec := dsAfter.Records[0]
	if rec.Note == nil || !strings.Contains(rec.Note.En, "Backfilled root cause") {
		t.Fatalf("expected backfilled note saved, got %+v", rec.Note)
	}
	if !rec.NeedsRetriage {
		t.Fatalf("expected notes-only patch NOT to clear needs_retriage on #81001, got NeedsRetriage=false")
	}
	if rec.ManualOverride {
		t.Fatalf("expected notes-only patch NOT to set ManualOverride=true on #81001")
	}
}
