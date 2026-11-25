package store

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

func recordBugCount(tx *sql.Tx, observedAt string) error {
	_, err := tx.Exec(`INSERT OR REPLACE INTO bug_history(day,count)
SELECT date(?, '+8 hours'), COUNT(*) FROM issues i
LEFT JOIN issue_overrides o USING(repo,number)
WHERE i.repo=? AND COALESCE(NULLIF(o.cat,''), i.auto_cat) IN ('EASY','MEDIUM','HARD')`, observedAt, DefaultRepo)
	return err
}

func commitIssueObservation(tx *sql.Tx) error {
	if err := recordBugCount(tx, model.NowRFC3339Nano()); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateBugHistory(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS bug_history(day TEXT NOT NULL PRIMARY KEY,count INTEGER NOT NULL CHECK(count>=0))`); err != nil {
		return err
	}
	// Older sync runs persisted the post-sync count in the human-readable summary.
	rows, err := tx.Query(`SELECT finished_at,summary_json FROM sync_runs WHERE kind='issues_dual_stream' ORDER BY finished_at,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	countPattern := regexp.MustCompile(`(?m)^\s*Candidates\s*:[^\n]*\breal_bugs: ([0-9]+)[,)]`)
	for rows.Next() {
		var stamp, raw string
		if err := rows.Scan(&stamp, &raw); err != nil {
			return err
		}
		var run struct {
			Output string `json:"script_output"`
			DryRun bool   `json:"dry_run"`
		}
		if json.Unmarshal([]byte(raw), &run) != nil || run.DryRun {
			continue
		}
		match := countPattern.FindStringSubmatch(run.Output)
		if len(match) == 0 {
			continue
		}
		count, err := strconv.Atoi(match[1])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO bug_history VALUES(date(?, '+8 hours'),?)`, stamp, count); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var initialized bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM kv_meta WHERE key='sync_meta') OR EXISTS(SELECT 1 FROM issues)`).Scan(&initialized); err != nil {
		return err
	}
	if initialized {
		return recordBugCount(tx, model.NowRFC3339Nano())
	}
	return nil
}

func loadBugHistory(q sqlExecQuerier) ([]model.BugCountPoint, error) {
	rows, err := q.Query("SELECT day,count FROM bug_history ORDER BY day")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []model.BugCountPoint
	for rows.Next() {
		var point model.BugCountPoint
		if err := rows.Scan(&point.Date, &point.Count); err != nil {
			return nil, err
		}
		history = append(history, point)
	}
	return history, rows.Err()
}
