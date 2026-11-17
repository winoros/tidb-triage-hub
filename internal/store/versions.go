package store

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

// Keep versions in a separate, durable row so delete/recreate transitions cannot
// make an old review valid again. Triggers cover both CLI and server connections,
// and update the counters in the same transaction as the reviewed facts.
func (s *Store) migrateReviewVersions(tx *sql.Tx) error {
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS issue_versions (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  review_version INTEGER NOT NULL DEFAULT 1,
  note_version INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(repo, number)
);
INSERT OR IGNORE INTO issue_versions(repo, number)
SELECT repo, number FROM issues
UNION SELECT repo, number FROM pending_candidates
UNION SELECT repo, number FROM excluded_issues
UNION SELECT repo, number FROM archived_issues;
`); err != nil {
		return err
	}
	// Cache-only pending writes increment their internal revision, but keep
	// updated_at stable; only material changes advance the public review version.
	tables := []struct {
		name, repo, number, counter string
		columns                     []string
	}{
		{"issues", "repo", "number", "review_version", []string{"title", "body", "author", "assignees_json", "created_at", "updated_at", "labels_json", "is_canonical", "is_customer", "gh_severity", "auto_cat", "auto_severity", "auto_subsystem", "auto_is_borderline", "auto_force_keep"}},
		{"issue_overrides", "repo", "number", "review_version", []string{"cat", "severity", "subsystem", "is_borderline_kept", "force_keep", "has_force_keep_override", "updated_at"}},
		{"issue_notes", "repo", "number", "note_version", []string{"note_zh", "note_en"}},
		{"issue_pr_links", "issue_repo", "issue_num", "review_version", []string{"state", "title", "is_cherry_pick", "merged_at", "ack_at", "source", "label", "url"}},
		{"issue_triage_state", "repo", "number", "review_version", []string{"triaged_at", "baseline_labels_json", "baseline_gh_severity", "baseline_is_customer", "baseline_open_prs_json", "baseline_merged_prs_json", "comment_watermark_utc", "recent_comments_json", "needs_retriage", "retriage_reasons_json", "retriage_details_json"}},
		{"pending_candidates", "repo", "number", "review_version", []string{"updated_at"}},
		{"excluded_issues", "repo", "number", "review_version", []string{"title", "reason", "excluded_labels_json", "source", "excluded_at"}},
		{"archived_issues", "repo", "number", "review_version", []string{"archive_reason", "archived_at"}},
	}
	for _, table := range tables {
		var changed []string
		for _, col := range table.columns {
			changed = append(changed, "OLD."+col+" IS NOT NEW."+col)
		}
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			row, when := "NEW", ""
			if event == "DELETE" {
				row = "OLD"
			}
			if event == "UPDATE" {
				when = "WHEN (" + strings.Join(changed, " OR ") + ")"
			}
			statement := fmt.Sprintf(`CREATE TRIGGER version_%s_%s_%s
AFTER %s ON %s %s BEGIN
  INSERT INTO issue_versions(repo, number) VALUES (%s.%s, %s.%s)
  ON CONFLICT(repo, number) DO UPDATE SET %s = %s + 1;
END;`, table.name, table.counter, strings.ToLower(event), event, table.name, when,
				row, table.repo, row, table.number, table.counter, table.counter)
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create review version trigger: %w", err)
			}
		}
	}
	return nil
}

func issueVersion(q sqlExecQuerier, number int) (model.ReviewVersion, error) {
	var v model.ReviewVersion
	err := q.QueryRow(`SELECT review_version, note_version FROM issue_versions WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&v.Review, &v.Notes)
	if err == sql.ErrNoRows {
		return v, fmt.Errorf("issue #%d: %w", number, ErrIssueNotFound)
	}
	return v, err
}

func checkReviewVersion(q sqlExecQuerier, number int, expected *model.ReviewVersion, review, notes bool) error {
	if expected == nil {
		return nil // Legacy timestamp guards remain supported for older clients.
	}
	if expected.Review <= 0 || expected.Notes <= 0 {
		return fmt.Errorf("%w: issue #%d: expected_version must contain positive review and notes counters", ErrInvalidInput, number)
	}
	current, err := issueVersion(q, number)
	if err != nil {
		return err
	}
	if (review && current.Review != expected.Review) || (notes && current.Notes != expected.Notes) {
		return fmt.Errorf("%w: issue #%d version changed (expected %+v, current %+v); reload and review again", ErrOverrideConflict, number, *expected, current)
	}
	return nil
}

// Check all preconditions before any batch writes, including companion manual
// links. A batch checks its input snapshot once, not again after its own writes.
func prepareBatchVersions(q sqlExecQuerier, dec model.BatchTriageDecisions, owners map[int]string, manualNums []int, opts BatchApplyOptions) (model.BatchTriageDecisions, map[int]bool, []model.TriageConflict, error) {
	all := maps.Clone(owners)
	manual := make(map[int]bool)
	for _, n := range manualNums {
		manual[n] = true
		if all[n] == "" {
			all[n] = "manual_pr_links"
		}
	}
	if _, err := sortedNumericKeys(dec.ExpectedVersions); err != nil {
		return dec, nil, nil, err
	}
	nums := make([]int, 0, len(all))
	for n := range all {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	skipped := make(map[int]bool)
	var conflicts []model.TriageConflict
	for _, n := range nums {
		action := all[n]
		if dec.ExpectedVersions == nil {
			continue
		}
		v, ok := dec.ExpectedVersions[strconv.Itoa(n)]
		if !ok {
			return dec, nil, nil, fmt.Errorf("%w: issue #%d: missing expected_versions entry", ErrInvalidInput, n)
		}
		expected := &v
		patch := dec.Overrides[strconv.Itoa(n)]
		err := checkReviewCommand(q, n, expected, action, action == "overrides" && patch.Note != nil)
		if err == nil && action == "notes" && manual[n] {
			err = checkReviewCommand(q, n, expected, "manual_pr_links", false)
		}
		if err != nil {
			if !errors.Is(err, ErrOverrideConflict) {
				return dec, nil, nil, err
			}
			if !opts.SkipConflicts {
				return dec, nil, nil, fmt.Errorf("%w: %s: %v", ErrBatchConflict, action, err)
			}
			skipped[n] = true
			conflicts = append(conflicts, model.TriageConflict{Issue: n, Action: action, Kind: "version_changed", Message: err.Error()})
			if manual[n] && action != "manual_pr_links" {
				conflicts = append(conflicts, model.TriageConflict{Issue: n, Action: "manual_pr_links", Kind: "companion_skipped", Message: "primary review version changed; skipped companion links"})
			}
			continue
		}
	}
	if dec.ExpectedVersions != nil {
		dec.BaseExportedAt = "" // Wall-clock fallback is for legacy batches only.
	}
	return dec, skipped, conflicts, nil
}
