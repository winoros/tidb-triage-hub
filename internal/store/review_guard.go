package store

import (
	"fmt"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

// Versioned commands bind both the reviewed data and the operation's target.
// A durable identity that has left the required queue is a conflict, even when
// the particular data dimension (for example notes) did not change.
func checkReviewCommand(q sqlExecQuerier, number int, expected *model.ReviewVersion, action string, writesNote bool) error {
	review, notes := true, writesNote
	var active, pending, excluded bool
	switch action {
	case "new_issues":
		pending, notes = true, true
	case "exclude_issues":
		active, pending, notes = true, true, true
	case "restore":
		excluded = true
	case "notes":
		active, review, notes = true, false, true
	case "overrides", "reset", "ack_retriage", "manual_pr_links":
		active = true
	default:
		return fmt.Errorf("%w: unknown review action %q", ErrInvalidInput, action)
	}
	if err := checkReviewVersion(q, number, expected, review, notes); err != nil {
		return err
	}
	if expected == nil {
		return nil
	} // Legacy entry points retain their timestamp/eligibility rules.
	var inActive, inPending, inExcluded bool
	if err := q.QueryRow(`SELECT
 EXISTS(SELECT 1 FROM issues WHERE repo=? AND number=?),
 EXISTS(SELECT 1 FROM pending_candidates WHERE repo=? AND number=?),
 EXISTS(SELECT 1 FROM excluded_issues WHERE repo=? AND number=?)`,
		DefaultRepo, number, DefaultRepo, number, DefaultRepo, number).Scan(&inActive, &inPending, &inExcluded); err != nil {
		return err
	}
	if (active && inActive) || (pending && inPending) || (excluded && inExcluded) {
		return nil
	}
	return fmt.Errorf("%w: issue #%d is no longer eligible for %s; reload and review again", ErrOverrideConflict, number, action)
}
