package syncer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

type noSubmissionNetwork struct{ t *testing.T }

func (r noSubmissionNetwork) Query(context.Context, string, map[string]string) ([]byte, error) {
	r.t.Fatal("submission accessed GitHub")
	return nil, nil
}

func TestSubmissionDoesNotRefreshReviewedFacts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pc := model.PendingCandidate{Number: 2, Title: "pending", PRs: []model.LinkedPRRef{{Repo: store.DefaultRepo, Num: 96901, State: "OPEN"}}}
	if err := st.IngestSyncedDataset(model.Dataset{Records: []model.IssueRecord{{Number: 1, Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite", PRs: pc.PRs}}}, []model.PendingCandidate{pc}); err != nil {
		t.Fatal(err)
	}
	sy := NewWithRunner(st, noSubmissionNetwork{t})
	snap, err := st.LoadWorkbenchSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sy.ExecuteReviewCommand(context.Background(), ReviewCommand{Action: ReviewOverride, Number: 1, Patch: model.TriagePatchRequest{Cat: "HARD", Severity: "MAJOR", Subsystem: "logical_rewrite", ExpectedVersion: snap.Dataset.Records[0].Version}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Record.PRs[0].State != "OPEN" {
		t.Fatal("command changed reviewed PR facts")
	}
	enabled := true // Deprecated compatibility flag must not bring network side effects back.
	result, err := sy.ExecuteBatchTriage(context.Background(), BatchTriageExecuteRequest{SyncNewPRs: &enabled, AllowEmptyNote: true, Decisions: model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"2": *snap.PendingCandidates[0].Version}, NewIssues: map[string]model.TriagePatchRequest{"2": {Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite"}}}})
	if err != nil || len(result.Applied.AcceptedIssues) != 1 {
		t.Fatalf("batch result=%+v err=%v", result, err)
	}
}
