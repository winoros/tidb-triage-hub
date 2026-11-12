package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/jsonutil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

type Syncer struct {
	mu         sync.Mutex
	store      *store.Store
	scriptsDir string
	workDir    string
}

func New(st *store.Store, scriptsDir, workDir string) *Syncer {
	return &Syncer{
		store:      st,
		scriptsDir: scriptsDir,
		workDir:    workDir,
	}
}

func (sy *Syncer) SyncPRStatuses(ctx context.Context) (prstatus.FetchStats, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	return sy.syncPRStatusesLocked(ctx)
}

func (sy *Syncer) syncPRStatusesLocked(ctx context.Context) (prstatus.FetchStats, error) {
	started := time.Now()
	targets, err := sy.store.ListOpenPRTargets()
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	statuses, stats, err := prstatus.FetchAndClassifyBatch(ctx, targets, 20)
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	finished := time.Now()
	if err := sy.store.UpsertPRStatuses(statuses, finished); err != nil {
		return prstatus.FetchStats{}, err
	}
	sy.store.RecordSyncRun("pr_status", started, finished, stats)
	return stats, nil
}

type IssueSyncResult struct {
	ScriptOutput string              `json:"script_output"`
	PRStats      prstatus.FetchStats `json:"pr_stats"`
	PendingCount int                 `json:"pending_count"`
}

func (sy *Syncer) SyncIssuesDualStream(ctx context.Context, full bool) (IssueSyncResult, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()

	started := time.Now()
	if err := os.MkdirAll(sy.workDir, 0o755); err != nil {
		return IssueSyncResult{}, err
	}

	stagingDataPath := filepath.Join(sy.workDir, "staging_data.json")
	decisionsPath := filepath.Join(sy.workDir, "staging_decisions.json")
	pendingPath := filepath.Join(sy.workDir, "staging_pending.json")
	_ = os.Remove(pendingPath)

	curDS, err := sy.store.BuildDataset(false)
	if err != nil {
		return IssueSyncResult{}, err
	}
	curBytes, err := jsonutil.MarshalPrettyJSON(curDS)
	if err != nil {
		return IssueSyncResult{}, err
	}
	if err := os.WriteFile(stagingDataPath, curBytes, 0o644); err != nil {
		return IssueSyncResult{}, err
	}

	decisions, err := sy.store.ExportDecisionsForSync()
	if err != nil {
		return IssueSyncResult{}, err
	}
	decBytes, err := jsonutil.MarshalPrettyJSON(decisions)
	if err != nil {
		return IssueSyncResult{}, err
	}
	if err := os.WriteFile(decisionsPath, decBytes, 0o644); err != nil {
		return IssueSyncResult{}, err
	}

	scriptPath := filepath.Join(sy.scriptsDir, "sync_issues.py")
	args := []string{
		scriptPath,
		"--data-json", stagingDataPath,
		"--decisions", decisionsPath,
		"--pending-file", pendingPath,
		"--allow-pending",
	}
	if full {
		args = append(args, "--full")
	}

	cmd := exec.CommandContext(ctx, "python3", args...)
	outBytes, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(outBytes))
	if err != nil {
		return IssueSyncResult{ScriptOutput: outStr}, fmt.Errorf("sync_issues.py failed: %w\n%s", err, outStr)
	}

	refreshedRaw, err := os.ReadFile(stagingDataPath)
	if err != nil {
		return IssueSyncResult{ScriptOutput: outStr}, err
	}
	var refreshedDS model.Dataset
	if err := json.Unmarshal(refreshedRaw, &refreshedDS); err != nil {
		return IssueSyncResult{ScriptOutput: outStr}, err
	}

	var pending []model.PendingCandidate
	if pendRaw, err := os.ReadFile(pendingPath); err == nil {
		var bundle struct {
			PendingCandidates []model.PendingCandidate `json:"pending_candidates"`
		}
		if json.Unmarshal(pendRaw, &bundle) == nil {
			pending = bundle.PendingCandidates
		}
	}

	if err := sy.store.IngestSyncedDataset(refreshedDS, pending); err != nil {
		return IssueSyncResult{ScriptOutput: outStr}, err
	}

	prStats, err := sy.syncPRStatusesLocked(ctx)
	if err != nil {
		return IssueSyncResult{ScriptOutput: outStr}, err
	}

	res := IssueSyncResult{
		ScriptOutput: outStr,
		PRStats:      prStats,
		PendingCount: len(pending),
	}
	sy.store.RecordSyncRun("issues_dual_stream", started, time.Now(), res)
	return res, nil
}
