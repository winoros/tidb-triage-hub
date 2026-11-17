package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

// Read-only transactions use deferred BEGIN with this driver, allowing WAL
// writers to commit while all snapshot queries retain the same read version.
func (s *Store) beginReviewRead() (*sql.Tx, error) {
	return s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
}

// ReviewSnapshot binds every returned candidate to the version of the facts
// actually read. No network work or cache enrichment runs inside this transaction.
type ReviewSnapshot struct {
	ExportedAt string
	Pending    []model.PendingCandidate
	Retriage   []model.RetriageCandidate
}

type WorkbenchSnapshot struct {
	Revision          int64                       `json:"snapshot_revision"`
	Dataset           model.Dataset               `json:"dataset"`
	PendingCandidates []model.PendingCandidate    `json:"pending_candidates"`
	ExcludedIssues    []model.ExcludedIssueRecord `json:"excluded_issues"`
}

// LoadWorkbenchSnapshot prevents a response from mixing an old dataset with
// new inbox queues when another process commits between the individual reads.
func (s *Store) LoadWorkbenchSnapshot() (WorkbenchSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out WorkbenchSnapshot
	tx, err := s.beginReviewRead()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = tx.QueryRow(`SELECT revision FROM workbench_revision WHERE id=1`).Scan(&out.Revision); err != nil {
		return out, err
	}
	if out.Dataset, err = buildDatasetFrom(tx, true); err != nil {
		return out, err
	}
	if out.PendingCandidates, err = listPendingCandidatesFrom(tx); err != nil {
		return out, err
	}
	out.ExcludedIssues, err = listExcludedIssuesFrom(tx)
	return out, err
}

func (s *Store) LoadReviewSnapshot(includePending, includeRetriage bool, retriageNumbers []int) (ReviewSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Legacy clients use this conservative read-start cutoff; new clients use
	// the per-object versions and do not depend on wall-clock ordering.
	snapshot := ReviewSnapshot{ExportedAt: model.NowRFC3339Nano()}
	tx, err := s.beginReviewRead()
	if err != nil {
		return ReviewSnapshot{}, err
	}
	defer tx.Rollback()
	if includePending {
		snapshot.Pending, err = listPendingCandidatesFrom(tx)
		if err != nil {
			return ReviewSnapshot{}, err
		}
	}
	if includeRetriage {
		snapshot.Retriage, err = listRetriageCandidatesFrom(tx, len(retriageNumbers) == 0, retriageNumbers)
		if err != nil {
			return ReviewSnapshot{}, err
		}
	}
	return snapshot, nil
}

// One durable revision orders whole workbench snapshots across HTTP requests,
// processes and restarts. Changes and the revision commit/roll back together;
// readers obtain both from the same SQLite snapshot. Gaps are intentional.
func migrateWorkbenchRevision(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS workbench_revision (
 id INTEGER PRIMARY KEY CHECK (id=1), revision INTEGER NOT NULL
);
INSERT OR IGNORE INTO workbench_revision(id,revision) VALUES(1,1);`); err != nil {
		return err
	}
	for _, table := range []string{"issues", "issue_overrides", "issue_notes", "issue_pr_links", "issue_triage_state", "issue_versions", "pending_candidates", "excluded_issues", "archived_issues", "pull_requests", "pr_details", "kv_meta"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			if _, err := tx.Exec(fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS workbench_%s_%s
AFTER %s ON %s BEGIN UPDATE workbench_revision SET revision=revision+1 WHERE id=1; END;`, table, event, event, table)); err != nil {
				return err
			}
		}
	}
	return nil
}
