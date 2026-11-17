package syncer

import (
	"context"
	"fmt"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/store"
)

type ReviewAction string

const (
	ReviewOverride ReviewAction = "override"
	ReviewReset    ReviewAction = "reset"
	ReviewResolve  ReviewAction = "resolve_pending"
	ReviewAck      ReviewAction = "ack"
	ReviewAckBatch ReviewAction = "ack_batch"
	ReviewExclude  ReviewAction = "exclude"
	ReviewRestore  ReviewAction = "restore"
)

// ReviewCommand is a single interactive intent. HTTP adapters only decode this
// intent; mutation preconditions and result snapshots live here.
// External facts are refreshed before review, never as a side effect of submission.
// Batch commands share the same store mutation primitives and version protocol.
type ReviewCommand struct {
	Action           ReviewAction
	Number           int
	Actor            string
	Guard            model.ReviewPrecondition
	Patch            model.TriagePatchRequest
	Pending          model.PendingResolveRequest
	Reason           string
	Numbers          []int
	ExpectedVersions map[string]model.ReviewVersion
	BaseExportedAt   string
	Watermarks       map[string]string // Legacy batch-ack clients only.
}

type ReviewCommandResult struct {
	store.WorkbenchSnapshot
	Record       *model.IssueRecord `json:"record,omitempty"`
	Acknowledged []int              `json:"acknowledged"`
}

func (sy *Syncer) ExecuteReviewCommand(ctx context.Context, cmd ReviewCommand) (ReviewCommandResult, error) {
	var out ReviewCommandResult
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if cmd.Action != ReviewAckBatch && cmd.Number <= 0 {
		return out, fmt.Errorf("%w: invalid issue number", ErrBatchInputInvalid)
	}
	var err error
	switch cmd.Action {
	case ReviewOverride:
		err = sy.store.ApplyIssueOverride(cmd.Number, cmd.Patch)
	case ReviewReset:
		err = sy.store.ResetIssueOverrideWithPrecondition(cmd.Number, cmd.Guard)
	case ReviewResolve:
		err = sy.store.ResolvePendingCandidate(cmd.Number, cmd.Pending)
	case ReviewAck:
		err = sy.store.CommitRetriageAcknowledgement(cmd.Number, cmd.Actor, cmd.Guard)
	case ReviewAckBatch:
		if len(cmd.Numbers) == 0 {
			return out, fmt.Errorf("%w: numbers list cannot be empty", ErrBatchInputInvalid)
		}
		out.Acknowledged, err = sy.store.AcknowledgeBatchRetriageWithVersions(cmd.Numbers, cmd.Actor, cmd.BaseExportedAt, cmd.Watermarks, cmd.ExpectedVersions)
	case ReviewExclude:
		err = sy.store.ExcludeExistingIssueWithPrecondition(cmd.Number, cmd.Reason, false, cmd.Guard)
	case ReviewRestore:
		err = sy.store.RestoreExcludedIssueWithPrecondition(cmd.Number, cmd.Guard)
	default:
		return out, fmt.Errorf("%w: unknown review action %q", ErrBatchInputInvalid, cmd.Action)
	}
	if err != nil {
		return out, err
	}
	out.WorkbenchSnapshot, err = sy.store.LoadWorkbenchSnapshot()
	if err != nil {
		return out, &CommittedSnapshotError{Cause: err}
	}
	for _, r := range out.Dataset.Records {
		if r.Number == cmd.Number {
			out.Record = &r
			break
		}
	}
	return out, nil
}

// CommittedSnapshotError distinguishes a failed response refresh from a failed
// command. Retrying the mutation is unsafe; clients should only reload state.
type CommittedSnapshotError struct{ Cause error }

func (e *CommittedSnapshotError) Error() string {
	return "decision committed, but loading the updated snapshot failed: " + e.Cause.Error()
}
func (e *CommittedSnapshotError) Unwrap() error { return e.Cause }
