package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

type gatedReviewRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedReviewRunner) Query(ctx context.Context, query string, args map[string]string) ([]byte, error) {
	if strings.Contains(query, "files(first:") {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return (&stubBundleRunner{}).Query(ctx, query, args)
}

func TestReviewExportReadsConsistentSnapshotAfterEnrichment(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pending := model.PendingCandidate{Number: 2, Title: "planner pending", Body: "old SQL", UpdatedAt: "2020-01-01T00:00:00Z", Labels: []string{"sig/planner", "type/bug"}, PRs: []model.LinkedPRRef{{Repo: store.DefaultRepo, Num: 96901, State: "OPEN"}}}
	err = st.IngestSyncedDataset(model.Dataset{SyncMeta: model.SyncMeta{SnapshotUTC: "2020-01-01T00:00:00Z"}, Records: []model.IssueRecord{{Number: 1, Title: "planner issue", UpdatedAt: "2020-01-01T00:00:00Z", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite"}}}, []model.PendingCandidate{pending})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	runner := &gatedReviewRunner{started: make(chan struct{}), release: make(chan struct{})}
	sy := NewWithRunner(st, runner)
	type result struct {
		bundle map[string]any
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		bundle, _, err := sy.BuildPendingBundle(context.Background(), PendingBundleOptions{Numbers: []int{2}, ReTriageNums: []int{1}, EnrichPRs: true})
		resultCh <- result{bundle, err}
	}()
	<-runner.started
	// Commit through a second DB connection while export is outside its read transaction.
	editErr := writer.ApplyIssueOverride(1, model.TriagePatchRequest{Cat: "HARD", Severity: "CRITICAL", Subsystem: "logical_rewrite"})
	pending.Body = "new SQL evidence"
	_, syncErr := writer.ApplyIssueSync(issuesync.SyncPlan{UpsertPending: []model.PendingCandidate{pending}})
	close(runner.release)
	res := <-resultCh
	if editErr != nil || syncErr != nil || res.err != nil {
		t.Fatalf("edit=%v sync=%v export=%v", editErr, syncErr, res.err)
	}
	ret := res.bundle["retriage_candidates"].([]model.RetriageCandidate)
	pend := res.bundle["pending_candidates"].([]model.PendingCandidate)
	if len(ret) != 1 || ret[0].CurrentCat != "HARD" || ret[0].CurrentSeverity != "CRITICAL" {
		t.Fatalf("exported stale classification: %+v", ret)
	}
	if len(pend) != 1 || pend[0].Body != "new SQL evidence" || len(pend[0].PRDetails) != 1 {
		t.Fatalf("shared PR evidence or concurrent issue facts missing: %+v", pend)
	}
	tmpl := res.bundle["decisions_template"].(model.BatchTriageDecisions)
	if tmpl.ExpectedVersions["1"] != *ret[0].Version || tmpl.ExpectedVersions["2"] != *pend[0].Version {
		t.Fatal("template versions do not match exported facts")
	}
	ds, _ := st.BuildDataset(true)
	if *ds.Records[0].Version != *ret[0].Version {
		t.Fatal("snapshot version is not the persisted version")
	}

	// A later edit must conflict even if a client supplies a future export time.
	err = writer.ApplyIssueOverride(1, model.TriagePatchRequest{Cat: "EASY", Severity: "MINOR", Subsystem: "logical_rewrite", ExpectedVersion: ret[0].Version})
	if err != nil {
		t.Fatal(err)
	}
	tmpl.NewIssues = nil
	tmpl.AckRetriage = model.FlexIntList{1}
	tmpl.BaseExportedAt = "2999-01-01T00:00:00Z"
	noPRSync := false
	for _, dry := range []bool{true, false} {
		_, err := sy.ExecuteBatchTriage(context.Background(), BatchTriageExecuteRequest{Decisions: tmpl, DryRun: dry, SyncNewPRs: &noPRSync})
		if !errors.Is(err, store.ErrBatchConflict) {
			t.Fatalf("dry=%v accepted stale ack: %v", dry, err)
		}
	}
	out, err := sy.ExecuteBatchTriage(context.Background(), BatchTriageExecuteRequest{Decisions: tmpl, SkipConflicts: true, SyncNewPRs: &noPRSync})
	if err != nil {
		t.Fatal(err)
	}
	applied := out.Applied
	if len(applied.AcknowledgedIssues) != 0 || len(applied.SkippedConflicts) != 1 {
		t.Fatalf("stale ack not skipped: %+v", applied)
	}
}

func TestReviewCommandAndBatchShareVersionSemantics(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "command.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.IngestSyncedDataset(model.Dataset{SyncMeta: model.SyncMeta{SnapshotUTC: "2020-01-01T00:00:00Z"}, Records: []model.IssueRecord{{Number: 1, Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite"}}}, nil); err != nil {
		t.Fatal(err)
	}
	sy := NewWithRunner(st, &stubBundleRunner{})
	ds, _ := st.BuildDataset(true)
	before := ds.Records[0].Version
	out, err := sy.ExecuteReviewCommand(context.Background(), ReviewCommand{Action: ReviewOverride, Number: 1, Patch: model.TriagePatchRequest{Cat: "HARD", Severity: "MAJOR", Subsystem: "logical_rewrite", ExpectedVersion: before}})
	if err != nil || out.Record == nil || out.Record.Cat != "HARD" {
		t.Fatalf("command result: %+v, %v", out, err)
	}
	noPRSync := false
	_, err = sy.ExecuteBatchTriage(context.Background(), BatchTriageExecuteRequest{Decisions: model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"1": *before}, AckRetriage: model.FlexIntList{1}}, SyncNewPRs: &noPRSync})
	if !errors.Is(err, store.ErrBatchConflict) {
		t.Fatalf("batch did not see command version change: %v", err)
	}
	before = out.Record.Version
	_, err = sy.ExecuteBatchTriage(context.Background(), BatchTriageExecuteRequest{Decisions: model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"1": *before}, AckRetriage: model.FlexIntList{1}}, SyncNewPRs: &noPRSync})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sy.ExecuteReviewCommand(context.Background(), ReviewCommand{Action: ReviewReset, Number: 1, Guard: model.ReviewPrecondition{ExpectedVersion: before}})
	if !errors.Is(err, store.ErrOverrideConflict) {
		t.Fatalf("command did not see batch version change: %v", err)
	}
}

func TestVersionedBatchCannotDowngradeMissingPrecondition(t *testing.T) {
	for _, action := range []string{"ack", "exclude", "manual", "override"} {
		t.Run(action, func(t *testing.T) {
			dec := model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{}, BaseExportedAt: "2999-01-01T00:00:00Z"}
			switch action {
			case "ack":
				dec.AckRetriage = model.FlexIntList{1}
			case "exclude":
				dec.ExcludeIssues = map[string]string{"1": "reason"}
			case "manual":
				dec.ManualPRLinks = map[string][]string{"1": {"12345"}}
			case "override":
				dec.Overrides = map[string]model.TriagePatchRequest{"1": {Cat: "HARD", Severity: "MAJOR", Subsystem: "logical_rewrite"}}
			}
			if _, err := PrepareBatchTriage(BatchTriageExecuteRequest{Decisions: dec}); !errors.Is(err, ErrBatchInputInvalid) {
				t.Fatalf("missing version accepted: %v", err)
			}
		})
	}
}
