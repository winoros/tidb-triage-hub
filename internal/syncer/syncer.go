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
	return sy.SyncPRStatusesWithQuota(ctx, 100)
}

func (sy *Syncer) SyncPRStatusesWithQuota(ctx context.Context, minQuota int) (prstatus.FetchStats, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	if minQuota > 0 {
		if _, err := prstatus.CheckGHQuota(ctx, minQuota); err != nil {
			return prstatus.FetchStats{}, err
		}
	}
	return sy.syncPRStatusesLocked(ctx)
}

func (sy *Syncer) SyncSpecificPRs(ctx context.Context, targets []prstatus.PRTarget, minQuota int) (prstatus.FetchStats, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	if minQuota > 0 {
		if _, err := prstatus.CheckGHQuota(ctx, minQuota); err != nil {
			return prstatus.FetchStats{}, err
		}
	}
	return sy.syncSpecificPRsLocked(ctx, targets)
}

func (sy *Syncer) syncSpecificPRsLocked(ctx context.Context, targets []prstatus.PRTarget) (prstatus.FetchStats, error) {
	started := time.Now()
	existing, _ := sy.store.ListOpenPRTargets()
	existingSrc := make(map[string][]string, len(existing))
	for _, t := range existing {
		existingSrc[model.PRKey(t.Repo, t.Number)] = t.TrackSources
	}
	enriched := make([]prstatus.PRTarget, 0, len(targets))
	for _, t := range targets {
		if len(t.TrackSources) == 0 {
			if srcs, ok := existingSrc[model.PRKey(t.Repo, t.Number)]; ok && len(srcs) > 0 {
				t.TrackSources = srcs
			} else {
				t.TrackSources = []string{"manual_cli"}
			}
		}
		enriched = append(enriched, t)
	}
	statuses, stats, err := prstatus.FetchAndClassifyBatch(ctx, enriched, 20)
	if err != nil {
		return prstatus.FetchStats{}, err
	}
	finished := time.Now()
	if err := sy.store.UpsertPRStatuses(statuses, time.Time{}); err != nil {
		return prstatus.FetchStats{}, err
	}
	sy.store.RecordSyncRun("pr_status_targeted", started, finished, stats)
	return stats, nil
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

type IssueSyncOptions struct {
	Full     bool
	SkipPRs  bool
	MinQuota int
}

type IssueSyncResult struct {
	ScriptOutput   string              `json:"script_output"`
	PRStats        prstatus.FetchStats `json:"pr_stats"`
	PendingCount   int                 `json:"pending_count"`
	PendingNumbers []int               `json:"pending_numbers,omitempty"`
}

func (sy *Syncer) SyncIssuesDualStream(ctx context.Context, full bool) (IssueSyncResult, error) {
	minQ := 250
	if full {
		minQ = 500
	}
	return sy.SyncIssuesWithOptions(ctx, IssueSyncOptions{
		Full:     full,
		SkipPRs:  false,
		MinQuota: minQ,
	})
}

func (sy *Syncer) SyncIssuesWithOptions(ctx context.Context, opts IssueSyncOptions) (IssueSyncResult, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()

	if opts.MinQuota > 0 {
		if _, err := prstatus.CheckGHQuota(ctx, opts.MinQuota); err != nil {
			return IssueSyncResult{}, err
		}
	}

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
	if opts.Full {
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

	var prStats prstatus.FetchStats
	if !opts.SkipPRs {
		prStats, err = sy.syncPRStatusesLocked(ctx)
		if err != nil {
			return IssueSyncResult{ScriptOutput: outStr}, err
		}
	}

	pendNums := make([]int, 0, len(pending))
	for _, pc := range pending {
		pendNums = append(pendNums, pc.Number)
	}

	res := IssueSyncResult{
		ScriptOutput:   outStr,
		PRStats:        prStats,
		PendingCount:   len(pending),
		PendingNumbers: pendNums,
	}
	sy.store.RecordSyncRun("issues_dual_stream", started, time.Now(), res)
	return res, nil
}

type TriageApplyResult struct {
	store.BatchTriageResult
	SyncedNewPRs int                  `json:"synced_new_prs"`
	PRStats      *prstatus.FetchStats `json:"pr_stats,omitempty"`
}

func (sy *Syncer) ApplyTriageDecisions(ctx context.Context, dec model.BatchTriageDecisions, syncNewPRs bool, minQuota int) (TriageApplyResult, error) {
	sy.mu.Lock()
	defer sy.mu.Unlock()

	started := time.Now()
	bRes, err := sy.store.ApplyBatchTriageDecisions(dec)
	if err != nil {
		return TriageApplyResult{}, err
	}
	out := TriageApplyResult{BatchTriageResult: bRes}
	if syncNewPRs && len(bRes.MissingPRTargets) > 0 {
		if minQuota > 0 {
			if _, err := prstatus.CheckGHQuota(ctx, minQuota); err != nil {
				return out, fmt.Errorf("triage decisions saved, but pre-flight quota check for %d newly linked PR(s) failed: %w", len(bRes.MissingPRTargets), err)
			}
		}
		stats, err := sy.syncSpecificPRsLocked(ctx, bRes.MissingPRTargets)
		if err != nil {
			return out, fmt.Errorf("triage decisions saved, but syncing %d newly linked PR(s) failed: %w", len(bRes.MissingPRTargets), err)
		}
		out.SyncedNewPRs = stats.TotalFetched
		out.PRStats = &stats
	}
	sy.store.RecordSyncRun("triage_apply", started, time.Now(), out)
	return out, nil
}
