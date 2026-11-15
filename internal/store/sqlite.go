package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

const DefaultRepo = "pingcap/tidb"

var (
	ErrCanonicalCannotExclude = errors.New("canonical sig/planner + type/bug issue cannot be excluded")
	ErrIssueNotFound          = errors.New("issue not found")
	ErrOverrideConflict       = errors.New("issue triage state was modified concurrently")
	ErrBatchConflict          = errors.New("batch triage conflict")
)

type sqlExecQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type Store struct {
	mu       sync.Mutex
	backupMu sync.Mutex
	db       *sql.DB
	dbPath   string
}

func sqliteFileURI(absPath string, rawQuery string) string {
	slashPath := filepath.ToSlash(absPath)
	omitHost := strings.HasPrefix(slashPath, "/")
	if !omitHost {
		slashPath = "/" + slashPath
	}
	u := url.URL{
		Scheme:   "file",
		OmitHost: omitHost,
		Path:     slashPath,
		RawQuery: rawQuery,
	}
	return u.String()
}

func resolveMainDBFilePath(db *sql.DB) string {
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err == nil && name == "main" {
			file = strings.TrimSpace(file)
			if file == "" {
				return ""
			}
			if abs, aErr := filepath.Abs(file); aErr == nil {
				return abs
			}
			return file
		}
	}
	return ""
}

func Open(dbPath string) (*Store, error) {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == "" {
		return nil, fmt.Errorf("sqlite database path is empty")
	}
	var (
		absPath string
		dsn     string
	)
	switch {
	case trimmed == ":memory:":
		dsn = "file::memory:?_pragma=busy_timeout(15000)"
	case strings.HasPrefix(trimmed, "file:"):
		dsn = trimmed
		if u, uErr := url.Parse(trimmed); uErr == nil {
			modeMem := strings.EqualFold(u.Query().Get("mode"), "memory") || strings.HasPrefix(trimmed, "file::memory:")
			if !modeMem && u.Path != "" && u.Path != ":memory:" {
				if p, aErr := filepath.Abs(filepath.FromSlash(u.Path)); aErr == nil {
					absPath = p
					_ = os.MkdirAll(filepath.Dir(absPath), 0o755)
				}
			}
		}
	default:
		p, err := filepath.Abs(trimmed)
		if err != nil {
			return nil, err
		}
		absPath = p
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return nil, err
		}
		dsn = sqliteFileURI(absPath, "_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dbPath: absPath}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if mainFile := resolveMainDBFilePath(db); mainFile != "" {
		s.dbPath = mainFile
	} else {
		s.dbPath = ""
	}
	return s, nil
}

func (s *Store) DBPath() string {
	return s.dbPath
}

func (s *Store) Close() error {
	return s.db.Close()
}

// BackupTo creates an online, self-contained SQLite snapshot at destPath using VACUUM INTO.
func (s *Store) BackupTo(destPath string) error {
	return s.BackupToContext(context.Background(), destPath)
}

// BackupToContext creates an online, self-contained SQLite snapshot at destPath using VACUUM INTO,
// honoring ctx cancellation and preventing live-DB self-overwrite or fixed-.tmp races.
func (s *Store) BackupToContext(ctx context.Context, destPath string) error {
	destPath = strings.TrimSpace(destPath)
	if destPath == "" {
		return fmt.Errorf("backup destination path is empty")
	}
	absDest, err := filepath.Abs(destPath)
	if err != nil {
		return err
	}
	if s.dbPath != "" {
		if filepath.Clean(absDest) == filepath.Clean(s.dbPath) {
			return fmt.Errorf("backup destination %s cannot be the active SQLite database", absDest)
		}
		if srcInfo, err1 := os.Stat(s.dbPath); err1 == nil {
			if dstInfo, err2 := os.Stat(absDest); err2 == nil && os.SameFile(srcInfo, dstInfo) {
				return fmt.Errorf("backup destination %s refers to the active SQLite database", absDest)
			}
		}
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(absDest), 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(absDest), ".triage-backup-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	_ = os.Remove(tmpPath)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	escaped := strings.ReplaceAll(tmpPath, "'", "''")
	var vacErr error
	if s.dbPath != "" {
		vacDB, openErr := sql.Open("sqlite", sqliteFileURI(s.dbPath, "_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)"))
		if openErr != nil {
			return openErr
		}
		vacDB.SetMaxOpenConns(1)
		_, vacErr = vacDB.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escaped))
		_ = vacDB.Close()
	} else {
		s.mu.Lock()
		_, vacErr = s.db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escaped))
		s.mu.Unlock()
	}
	if vacErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sqlite vacuum into %s: %w", absDest, vacErr)
	}
	_ = os.Chmod(tmpPath, 0o600)
	if err := os.Rename(tmpPath, absDest); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Remove(absDest + "-wal")
	_ = os.Remove(absDest + "-shm")
	return nil
}

func ensureColumn(db *sql.DB, table, col, colDef string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if strings.EqualFold(name, col) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, colDef))
	return err
}

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS kv_meta (
  key TEXT PRIMARY KEY,
  val_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS issues (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  author TEXT NOT NULL,
  assignees_json TEXT NOT NULL,
  created TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  labels_json TEXT NOT NULL,
  is_canonical INTEGER NOT NULL,
  is_customer INTEGER NOT NULL,
  gh_severity TEXT NOT NULL,
  auto_cat TEXT NOT NULL,
  auto_severity TEXT NOT NULL,
  auto_subsystem TEXT NOT NULL,
  auto_is_borderline INTEGER NOT NULL,
  auto_force_keep INTEGER NOT NULL DEFAULT 0,
  auto_note_zh TEXT NOT NULL,
  auto_note_en TEXT NOT NULL,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS issue_overrides (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  cat TEXT,
  severity TEXT,
  subsystem TEXT,
  is_borderline_kept INTEGER,
  force_keep INTEGER NOT NULL DEFAULT 0,
  has_force_keep_override INTEGER NOT NULL DEFAULT 0,
  note_zh TEXT,
  note_en TEXT,
  has_note_override INTEGER NOT NULL DEFAULT 0,
  is_new_decision INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS issue_pr_links (
  issue_repo TEXT NOT NULL,
  issue_num INTEGER NOT NULL,
  pr_repo TEXT NOT NULL,
  pr_num INTEGER NOT NULL,
  label TEXT NOT NULL,
  url TEXT NOT NULL,
  state TEXT NOT NULL,
  title TEXT NOT NULL,
  is_cherry_pick INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'timeline',
  PRIMARY KEY (issue_repo, issue_num, pr_repo, pr_num)
);

CREATE INDEX IF NOT EXISTS idx_issue_pr_links_pr ON issue_pr_links(pr_repo, pr_num);

CREATE TABLE IF NOT EXISTS pull_requests (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  state TEXT NOT NULL,
  is_cherry_pick INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  status_json TEXT NOT NULL,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS pending_candidates (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  payload_json TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS excluded_issues (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL,
  excluded_labels_json TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL DEFAULT 'triage',
  excluded_at TEXT NOT NULL,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS archived_issues (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  title TEXT NOT NULL,
  auto_cat TEXT NOT NULL,
  auto_severity TEXT NOT NULL,
  auto_subsystem TEXT NOT NULL,
  auto_is_borderline INTEGER NOT NULL,
  auto_force_keep INTEGER NOT NULL DEFAULT 0,
  auto_note_zh TEXT NOT NULL,
  auto_note_en TEXT NOT NULL,
  archive_reason TEXT NOT NULL,
  archived_at TEXT NOT NULL,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS issue_triage_state (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  triaged_at TEXT NOT NULL,
  triaged_by TEXT NOT NULL DEFAULT 'seed',
  baseline_labels_json TEXT NOT NULL DEFAULT '[]',
  baseline_gh_severity TEXT NOT NULL DEFAULT 'unlabeled',
  baseline_is_customer INTEGER NOT NULL DEFAULT 0,
  baseline_open_prs_json TEXT NOT NULL DEFAULT '[]',
  baseline_merged_prs_json TEXT NOT NULL DEFAULT '[]',
  unverified_open_prs_json TEXT NOT NULL DEFAULT '[]',
  comment_watermark_utc TEXT NOT NULL DEFAULT '',
  recent_comments_json TEXT NOT NULL DEFAULT '[]',
  auto_baseline_json TEXT NOT NULL DEFAULT '',
  needs_retriage INTEGER NOT NULL DEFAULT 0,
  retriage_reasons_json TEXT NOT NULL DEFAULT '[]',
  retriage_details_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS pr_annotations (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  starred INTEGER NOT NULL DEFAULT 0,
  snoozed_until TEXT NOT NULL DEFAULT '',
  note_zh TEXT NOT NULL DEFAULT '',
  note_en TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS sync_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL,
  summary_json TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Idempotent column migrations for existing databases
	colMigrations := []struct {
		table, col, def string
	}{
		{"issues", "body", "TEXT NOT NULL DEFAULT ''"},
		{"issues", "auto_force_keep", "INTEGER NOT NULL DEFAULT 0"},
		{"issue_overrides", "has_force_keep_override", "INTEGER NOT NULL DEFAULT 0"},
		{"issue_pr_links", "source", "TEXT NOT NULL DEFAULT 'timeline'"},
		{"pending_candidates", "revision", "INTEGER NOT NULL DEFAULT 1"},
		{"issue_triage_state", "auto_baseline_json", "TEXT NOT NULL DEFAULT ''"},
		{"issue_triage_state", "unverified_open_prs_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"excluded_issues", "title", "TEXT NOT NULL DEFAULT ''"},
		{"excluded_issues", "excluded_labels_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"excluded_issues", "source", "TEXT NOT NULL DEFAULT 'triage'"},
	}
	for _, m := range colMigrations {
		if err := ensureColumn(s.db, m.table, m.col, m.def); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.col, err)
		}
	}

	// Fold any legacy `is_new_decision = 1` rows from issue_overrides into issues.auto_* (or archived_issues if already closed) and remove them
	if _, err := s.db.Exec(`
UPDATE issues
SET
  auto_cat = COALESCE((SELECT NULLIF(cat, '') FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_cat),
  auto_severity = COALESCE((SELECT NULLIF(severity, '') FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_severity),
  auto_subsystem = COALESCE((SELECT NULLIF(subsystem, '') FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_subsystem),
  auto_is_borderline = COALESCE((SELECT is_borderline_kept FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_is_borderline),
  auto_force_keep = COALESCE((SELECT force_keep FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_force_keep),
  auto_note_zh = COALESCE((SELECT CASE WHEN has_note_override = 1 THEN COALESCE(note_zh, '') ELSE NULL END FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_note_zh),
  auto_note_en = COALESCE((SELECT CASE WHEN has_note_override = 1 THEN COALESCE(note_en, '') ELSE NULL END FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1), auto_note_en)
WHERE EXISTS (SELECT 1 FROM issue_overrides o WHERE o.repo = issues.repo AND o.number = issues.number AND o.is_new_decision = 1)
`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`
INSERT INTO archived_issues(
  repo, number, title, auto_cat, auto_severity, auto_subsystem,
  auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en, archive_reason, archived_at
)
SELECT
  o.repo, o.number, '', COALESCE(NULLIF(o.cat, ''), 'FILTER_NON_OPT_OR_FIXED'),
  COALESCE(NULLIF(o.severity, ''), 'MODERATE'), COALESCE(NULLIF(o.subsystem, ''), 'other'),
  COALESCE(o.is_borderline_kept, 0), COALESCE(o.force_keep, 0),
  COALESCE(o.note_zh, ''), COALESCE(o.note_en, ''), 'legacy_new_decision', o.updated_at
FROM issue_overrides o
WHERE o.is_new_decision = 1
  AND NOT EXISTS (SELECT 1 FROM issues i WHERE i.repo = o.repo AND i.number = o.number)
  AND NOT EXISTS (SELECT 1 FROM excluded_issues e WHERE e.repo = o.repo AND e.number = o.number)
ON CONFLICT(repo, number) DO NOTHING
`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM issue_overrides WHERE is_new_decision = 1`); err != nil {
		return err
	}

	// Preserve existing positive force_keep overrides created before has_force_keep_override existed
	if _, err := s.db.Exec(`
UPDATE issue_overrides
SET has_force_keep_override = 1
WHERE force_keep != 0 AND has_force_keep_override = 0
`); err != nil {
		return err
	}

	// Upgrade historical static PR links (e.g. #65653 -> #67129 with empty updated_at) to source='manual'
	if _, err := s.db.Exec(`
UPDATE issue_pr_links
SET source = 'manual'
WHERE source != 'manual'
  AND (updated_at = '' OR (issue_repo = 'pingcap/tidb' AND issue_num = 65653 AND pr_repo = 'pingcap/tidb' AND pr_num = 67129))
`); err != nil {
		return err
	}

	// Normalize any legacy second-precision excluded_at timestamps (YYYY-MM-DDTHH:MM:SSZ)
	// to fixed 9-digit nanosecond UTC so SQLite lexicographical ORDER BY matches chronological order.
	if _, err := s.db.Exec(`
UPDATE excluded_issues
SET excluded_at = substr(excluded_at, 1, 19) || '.000000000Z'
WHERE length(excluded_at) = 20 AND substr(excluded_at, 20, 1) = 'Z'
`); err != nil {
		return err
	}

	return s.backfillTriageBaselines(s.db)
}

type savedTriageBaseline struct {
	TriagedAt             string `json:"triaged_at"`
	TriagedBy             string `json:"triaged_by"`
	BaselineLabelsJSON    string `json:"baseline_labels_json"`
	BaselineGHSeverity    string `json:"baseline_gh_severity"`
	BaselineIsCustomer    int    `json:"baseline_is_customer"`
	BaselineOpenPRsJSON   string `json:"baseline_open_prs_json"`
	BaselineMergedPRsJSON string `json:"baseline_merged_prs_json"`
	UnverifiedOpenPRsJSON string `json:"unverified_open_prs_json,omitempty"`
	CommentWatermarkUTC   string `json:"comment_watermark_utc"`
}

func computeUnverifiedOpenPRKeys(q sqlExecQuerier, prs []model.LinkedPRRef, markAllUncached bool) []string {
	if len(prs) == 0 {
		return []string{}
	}
	seen := make(map[string]bool)
	var out []string
	for _, p := range prs {
		if p.Repo == "" || p.Num <= 0 || p.IsCherryPick {
			continue
		}
		if p.State != "OPEN" && p.State != "DRAFT" {
			continue
		}
		if !markAllUncached && strings.TrimSpace(p.UpdatedAt) != "" {
			continue
		}
		key := model.PRKey(p.Repo, p.Num)
		if seen[key] {
			continue
		}
		var cnt int
		if err := q.QueryRow(`SELECT COUNT(*) FROM pull_requests WHERE repo = ? AND number = ?`, p.Repo, p.Num).Scan(&cnt); err == nil && cnt > 0 {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return nonNilSlice(out)
}

func isIncomingPRSnapshotStale(curState, curUpdAt, nextState, nextUpdAt string) bool {
	curUpdAt = strings.TrimSpace(curUpdAt)
	nextUpdAt = strings.TrimSpace(nextUpdAt)
	if curUpdAt == "" || nextUpdAt == "" {
		return false
	}
	cmp := model.CompareRFC3339(nextUpdAt, curUpdAt)
	if cmp < 0 {
		return true
	}
	// GitHub PR updatedAt has 1-second resolution; MERGED is terminal on GitHub and cannot regress.
	if cmp == 0 && curState == "MERGED" && nextState != "MERGED" {
		return true
	}
	return false
}

func (s *Store) backfillTriageBaselines(q sqlExecQuerier) error {
	type missingRow struct {
		number     int
		updatedAt  string
		labelsJSON string
		ghSeverity string
		isCustomer int
		triagedAt  string
	}
	var missing []missingRow
	rows, err := q.Query(`
SELECT i.number, i.updated_at, i.labels_json, i.gh_severity, i.is_customer, COALESCE(o.updated_at, '')
FROM issues i
LEFT JOIN issue_triage_state ts ON i.repo = ts.repo AND i.number = ts.number
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
WHERE i.repo = ? AND ts.number IS NULL`, DefaultRepo)
	if err != nil {
		return err
	}
	for rows.Next() {
		var m missingRow
		if err := rows.Scan(&m.number, &m.updatedAt, &m.labelsJSON, &m.ghSeverity, &m.isCustomer, &m.triagedAt); err != nil {
			_ = rows.Close()
			return err
		}
		missing = append(missing, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(missing) == 0 {
		return nil
	}

	var snapUTC string
	var rawMeta string
	if err := q.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'sync_meta'`).Scan(&rawMeta); err == nil {
		var meta model.SyncMeta
		if json.Unmarshal([]byte(rawMeta), &meta) == nil {
			snapUTC = meta.SnapshotUTC
		}
	}
	if snapUTC == "" {
		snapUTC = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

	linksByIssue := make(map[int][]model.LinkedPRRef)
	lRows, err := q.Query(`
SELECT issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at
FROM issue_pr_links
WHERE issue_repo = ?`, DefaultRepo)
	if err != nil {
		return err
	}
	for lRows.Next() {
		var issNum, isCP int
		var p model.LinkedPRRef
		if err := lRows.Scan(&issNum, &p.Repo, &p.Num, &p.Label, &p.URL, &p.State, &p.Title, &isCP, &p.UpdatedAt); err != nil {
			_ = lRows.Close()
			return err
		}
		p.IsCherryPick = isCP != 0
		linksByIssue[issNum] = append(linksByIssue[issNum], p)
	}
	if err := lRows.Err(); err != nil {
		_ = lRows.Close()
		return err
	}
	_ = lRows.Close()

	for _, m := range missing {
		openKeys, mergedKeys := triage.ExtractMainlinePRKeySets(linksByIssue[m.number])
		unverifiedKeys := computeUnverifiedOpenPRKeys(q, linksByIssue[m.number], false)
		openJSON, _ := json.Marshal(nonNilSlice(openKeys))
		mergedJSON, _ := json.Marshal(nonNilSlice(mergedKeys))
		unverifiedJSON, _ := json.Marshal(unverifiedKeys)
		triagedAt := m.triagedAt
		if triagedAt == "" {
			triagedAt = snapUTC
		}
		watermark := model.NormalizeRFC3339UTC(m.updatedAt)
		if watermark == "" {
			watermark = model.NormalizeRFC3339UTC(snapUTC)
		}
		baseSnap, _ := json.Marshal(savedTriageBaseline{
			TriagedAt:             triagedAt,
			TriagedBy:             "seed",
			BaselineLabelsJSON:    m.labelsJSON,
			BaselineGHSeverity:    m.ghSeverity,
			BaselineIsCustomer:    m.isCustomer,
			BaselineOpenPRsJSON:   string(openJSON),
			BaselineMergedPRsJSON: string(mergedJSON),
			UnverifiedOpenPRsJSON: string(unverifiedJSON),
			CommentWatermarkUTC:   watermark,
		})
		if _, err := q.Exec(`
INSERT INTO issue_triage_state(
  repo, number, triaged_at, triaged_by,
  baseline_labels_json, baseline_gh_severity, baseline_is_customer,
  baseline_open_prs_json, baseline_merged_prs_json, unverified_open_prs_json,
  comment_watermark_utc, recent_comments_json, auto_baseline_json,
  needs_retriage, retriage_reasons_json, retriage_details_json
) VALUES (?, ?, ?, 'seed', ?, ?, ?, ?, ?, ?, ?, '[]', ?, 0, '[]', '[]')
ON CONFLICT(repo, number) DO NOTHING`,
			DefaultRepo, m.number, triagedAt,
			m.labelsJSON, m.ghSeverity, m.isCustomer,
			string(openJSON), string(mergedJSON), string(unverifiedJSON), watermark, string(baseSnap),
		); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) IssueCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&n)
	return n, err
}

func (s *Store) BootstrapIfEmpty(dataJSONPath string) (bool, error) {
	return s.BootstrapIfEmptyWithFallback(dataJSONPath, nil)
}

func (s *Store) BootstrapIfEmptyWithFallback(dataJSONPath string, fallbackJSON []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cnt int
	if err := s.db.QueryRow(`
SELECT
  (SELECT COUNT(*) FROM issues) +
  (SELECT COUNT(*) FROM pending_candidates) +
  (SELECT COUNT(*) FROM excluded_issues) +
  (SELECT COUNT(*) FROM archived_issues) +
  (SELECT COUNT(*) FROM pull_requests) +
  (SELECT COUNT(*) FROM pr_annotations) +
  (SELECT COUNT(*) FROM sync_runs) +
  (SELECT COUNT(*) FROM kv_meta)`).Scan(&cnt); err != nil {
		return false, err
	}
	if cnt > 0 {
		return false, nil
	}
	var raw []byte
	var err error
	if strings.TrimSpace(dataJSONPath) != "" {
		raw, err = os.ReadFile(dataJSONPath)
		if err != nil && os.IsNotExist(err) && len(fallbackJSON) > 0 {
			raw = fallbackJSON
			err = nil
		}
	} else if len(fallbackJSON) > 0 {
		raw = fallbackJSON
	} else {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var ds model.Dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		return false, err
	}
	if err := s.ingestDatasetLocked(ds, nil, true); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) IngestSyncedDataset(ds model.Dataset, pending []model.PendingCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ingestDatasetLocked(ds, pending, false)
}

func (s *Store) ingestDatasetLocked(ds model.Dataset, pending []model.PendingCandidate, replacePRStatus bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	metaBytes, err := json.Marshal(ds.SyncMeta)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO kv_meta(key, val_json) VALUES('sync_meta', ?) ON CONFLICT(key) DO UPDATE SET val_json=excluded.val_json`, string(metaBytes)); err != nil {
		return err
	}

	keepSet := make(map[int]bool, len(ds.Records))
	for _, r := range ds.Records {
		keepSet[r.Number] = true
	}

	// Remove issues absent from ds.Records (archiving their baselines first)
	existingRows, err := tx.Query(`SELECT number, title, auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en FROM issues WHERE repo = ?`, DefaultRepo)
	if err != nil {
		return err
	}
	type staleRow struct {
		num                             int
		title, cat, sev, sub, nZh, nEn  string
		bdr, fk                         int
	}
	var stale []staleRow
	for existingRows.Next() {
		var sr staleRow
		if err := existingRows.Scan(&sr.num, &sr.title, &sr.cat, &sr.sev, &sr.sub, &sr.bdr, &sr.fk, &sr.nZh, &sr.nEn); err != nil {
			_ = existingRows.Close()
			return err
		}
		if !keepSet[sr.num] {
			stale = append(stale, sr)
		}
	}
	_ = existingRows.Close()

	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	for _, sr := range stale {
		_, _ = tx.Exec(`
INSERT INTO archived_issues(repo, number, title, auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en, archive_reason, archived_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'dataset_sync_removed', ?)
ON CONFLICT(repo, number) DO UPDATE SET archived_at=excluded.archived_at`,
			DefaultRepo, sr.num, sr.title, sr.cat, sr.sev, sr.sub, sr.bdr, sr.fk, sr.nZh, sr.nEn, nowISO)
		_, _ = tx.Exec(`DELETE FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, sr.num)
		_, _ = tx.Exec(`DELETE FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ?`, DefaultRepo, sr.num)
		_, _ = tx.Exec(`UPDATE issue_triage_state SET needs_retriage = 0, retriage_reasons_json = '[]', retriage_details_json = '[]' WHERE repo = ? AND number = ?`, DefaultRepo, sr.num)
	}

	// Upsert issues WITHOUT clobbering auto_* on already-existing rows!
	insIssue, err := tx.Prepare(`
INSERT INTO issues(
  repo, number, title, author, assignees_json, created, created_at, updated_at,
  labels_json, is_canonical, is_customer, gh_severity,
  auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title,
  author=excluded.author,
  assignees_json=excluded.assignees_json,
  updated_at=excluded.updated_at,
  labels_json=excluded.labels_json,
  is_canonical=excluded.is_canonical,
  is_customer=excluded.is_customer,
  gh_severity=excluded.gh_severity`)
	if err != nil {
		return err
	}
	defer insIssue.Close()

	insLink, err := tx.Prepare(`
INSERT INTO issue_pr_links(
  issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at, source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET
  label=excluded.label, url=excluded.url, state=excluded.state,
  title=excluded.title, is_cherry_pick=excluded.is_cherry_pick,
  updated_at=CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE issue_pr_links.updated_at END,
  source=CASE WHEN issue_pr_links.source = 'manual' THEN 'manual' ELSE excluded.source END`)
	if err != nil {
		return err
	}
	defer insLink.Close()

	for _, r := range ds.Records {
		asg, _ := json.Marshal(nonNilSlice(r.Assignees))
		lbs, _ := json.Marshal(nonNilSlice(r.Labels))
		nZh, nEn := "", ""
		if r.Note != nil {
			nZh = r.Note.ZhCN
			nEn = r.Note.En
		}
		if _, err := insIssue.Exec(
			DefaultRepo, r.Number, r.Title, r.Author, string(asg), r.Created, r.CreatedAt, r.UpdatedAt,
			string(lbs), boolToInt(r.IsCanonical), boolToInt(r.IsCustomer), r.GHSeverity,
			r.Cat, r.Severity, r.Subsystem, boolToInt(r.IsBorderlineKept), boolToInt(r.ForceKeep), nZh, nEn,
		); err != nil {
			return err
		}

		// Delete stale timeline links for this issue that disappeared in r.PRs
		livePRKeys := make(map[string]bool, len(r.PRs))
		for _, p := range r.PRs {
			livePRKeys[model.PRKey(p.Repo, p.Num)] = true
			src := p.Source
			if src == "" {
				src = model.LinkSourceTimeline
			}
			if p.UpdatedAt == "" || (r.Number == 65653 && p.Repo == "pingcap/tidb" && p.Num == 67129) {
				src = model.LinkSourceManual
			}
			if _, err := insLink.Exec(
				DefaultRepo, r.Number, p.Repo, p.Num, p.Label, p.URL, p.State, p.Title, boolToInt(p.IsCherryPick), p.UpdatedAt, src,
			); err != nil {
				return err
			}
		}
		curLRows, err := tx.Query(`SELECT pr_repo, pr_num, source FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ?`, DefaultRepo, r.Number)
		if err == nil {
			var toDel [][2]any
			for curLRows.Next() {
				var prRepo, src string
				var prNum int
				if curLRows.Scan(&prRepo, &prNum, &src) == nil {
					if src == model.LinkSourceTimeline && !livePRKeys[model.PRKey(prRepo, prNum)] {
						toDel = append(toDel, [2]any{prRepo, prNum})
					}
				}
			}
			_ = curLRows.Close()
			for _, d := range toDel {
				_, _ = tx.Exec(`DELETE FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ? AND pr_repo = ? AND pr_num = ?`,
					DefaultRepo, r.Number, d[0], d[1])
			}
		}
	}

	nowNanoISO := model.NowRFC3339Nano()
	for _, exNum := range ds.SyncMeta.ExcludedIssueNums {
		if _, err := tx.Exec(`
INSERT INTO excluded_issues(repo, number, reason, source, excluded_at)
VALUES (?, ?, 'Excluded from optimizer candidate scope', 'seed_removed', ?)
ON CONFLICT(repo, number) DO NOTHING`, DefaultRepo, exNum, nowNanoISO); err != nil {
			return err
		}
	}
	for _, r := range ds.Records {
		_, _ = tx.Exec(`DELETE FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, r.Number)
	}

	nextPendSet := make(map[int]bool, len(pending))
	for _, pc := range pending {
		nextPendSet[pc.Number] = true
	}
	var removedFromPend []int
	if oldPRows, qErr := tx.Query(`SELECT number FROM pending_candidates WHERE repo = ?`, DefaultRepo); qErr == nil {
		for oldPRows.Next() {
			var n int
			if oldPRows.Scan(&n) == nil && !nextPendSet[n] {
				removedFromPend = append(removedFromPend, n)
			}
		}
		_ = oldPRows.Close()
	}
	if len(removedFromPend) > 0 {
		_ = addRemovedPendingNumsInTx(tx, removedFromPend...)
	}
	if _, err := tx.Exec(`DELETE FROM pending_candidates WHERE repo = ?`, DefaultRepo); err != nil {
		return err
	}
	for _, pc := range pending {
		b, err := json.Marshal(pc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO pending_candidates(repo, number, payload_json, updated_at) VALUES (?, ?, ?, ?)`,
			DefaultRepo, pc.Number, string(b), pc.UpdatedAt); err != nil {
			return err
		}
	}

	if replacePRStatus && len(ds.PRStatus) > 0 {
		for _, st := range ds.PRStatus {
			b, err := json.Marshal(st)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`
INSERT INTO pull_requests(repo, number, state, is_cherry_pick, updated_at, status_json)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  state=excluded.state, is_cherry_pick=excluded.is_cherry_pick,
  updated_at=excluded.updated_at, status_json=excluded.status_json`,
				st.Repo, st.Num, st.State, boolToInt(st.IsCherryPick), st.UpdatedAt, string(b)); err != nil {
				return err
			}
		}
	}

	if err := s.backfillTriageBaselines(tx); err != nil {
		return err
	}

	return tx.Commit()
}

// queryBoardEligibleOpenPRKeys returns open/draft PRs linked to at least one active issue
// whose effective category is NOT `FILTER_NON_OPT_OR_FIXED`.
func queryBoardEligibleOpenPRKeys(q sqlExecQuerier) (map[string]bool, error) {
	rows, err := q.Query(`
SELECT DISTINCT l.pr_repo, l.pr_num
FROM issue_pr_links l
JOIN issues i ON l.issue_repo = i.repo AND l.issue_num = i.number
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
WHERE l.state IN ('OPEN', 'DRAFT')
  AND COALESCE(NULLIF(o.cat, ''), i.auto_cat) != 'FILTER_NON_OPT_OR_FIXED'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var repo string
		var num int
		if err := rows.Scan(&repo, &num); err != nil {
			return nil, err
		}
		out[model.PRKey(repo, num)] = true
	}
	return out, rows.Err()
}

func (s *Store) ListOpenPRTargets() ([]prstatus.PRTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listOpenPRTargetsLocked()
}

func (s *Store) listOpenPRTargetsLocked() ([]prstatus.PRTarget, error) {
	eligibleLinked, err := queryBoardEligibleOpenPRKeys(s.db)
	if err != nil {
		return nil, err
	}

	type targetEntry struct {
		repo    string
		num     int
		sources map[string]bool
	}
	byKey := make(map[string]*targetEntry)
	ensure := func(repo string, num int) *targetEntry {
		k := model.PRKey(repo, num)
		e, ok := byKey[k]
		if !ok {
			e = &targetEntry{repo: repo, num: num, sources: make(map[string]bool)}
			byKey[k] = e
		}
		return e
	}

	for k := range eligibleLinked {
		parts := strings.SplitN(k, "#", 2)
		if len(parts) == 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
				ensure(parts[0], n).sources["issue_linked"] = true
			}
		}
	}

	pRows, err := s.db.Query(`
SELECT repo, number, status_json
FROM pull_requests
WHERE state IN ('OPEN', 'DRAFT')`)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()
	for pRows.Next() {
		var repo, raw string
		var num int
		if err := pRows.Scan(&repo, &num, &raw); err != nil {
			return nil, err
		}
		key := model.PRKey(repo, num)
		var st model.PRStatus
		var standaloneSrcs []string
		if json.Unmarshal([]byte(raw), &st) == nil {
			for _, src := range st.TrackSources {
				if src != "" && src != "issue_linked" {
					standaloneSrcs = append(standaloneSrcs, src)
				}
			}
		}
		if !eligibleLinked[key] && len(standaloneSrcs) == 0 {
			continue
		}
		e := ensure(repo, num)
		if eligibleLinked[key] {
			e.sources["issue_linked"] = true
		}
		for _, src := range standaloneSrcs {
			e.sources[src] = true
		}
	}
	if err := pRows.Err(); err != nil {
		return nil, err
	}

	out := make([]prstatus.PRTarget, 0, len(byKey))
	for _, e := range byKey {
		srcs := make([]string, 0, len(e.sources))
		for src := range e.sources {
			srcs = append(srcs, src)
		}
		sort.Strings(srcs)
		out = append(out, prstatus.PRTarget{
			Repo:         e.repo,
			Number:       e.num,
			TrackSources: srcs,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number > out[j].Number
	})
	return out, nil
}

type verifiedPRStateLite struct {
	state string
	isCP  bool
}

func reconcileUnverifiedPRsInTx(tx *sql.Tx, issueNums []int, verified map[string]verifiedPRStateLite) error {
	if len(issueNums) == 0 || len(verified) == 0 {
		return nil
	}
	reconcileSet := func(openJSON, mergedJSON, unverifiedJSON string) (string, string, string, bool) {
		var unverified []string
		_ = json.Unmarshal([]byte(unverifiedJSON), &unverified)
		if len(unverified) == 0 {
			return openJSON, mergedJSON, unverifiedJSON, false
		}
		var openList, mergedList []string
		_ = json.Unmarshal([]byte(openJSON), &openList)
		_ = json.Unmarshal([]byte(mergedJSON), &mergedList)
		openSet := make(map[string]bool, len(openList))
		for _, k := range openList {
			if k != "" {
				openSet[k] = true
			}
		}
		mergedSet := make(map[string]bool, len(mergedList))
		for _, k := range mergedList {
			if k != "" {
				mergedSet[k] = true
			}
		}

		var remUnverified []string
		changed := false
		for _, k := range unverified {
			if k == "" {
				continue
			}
			vp, ok := verified[k]
			if !ok {
				remUnverified = append(remUnverified, k)
				continue
			}
			changed = true
			if openSet[k] {
				switch {
				case vp.isCP:
					delete(openSet, k)
				case vp.state == "MERGED":
					delete(openSet, k)
					mergedSet[k] = true
				case vp.state == "CLOSED":
					delete(openSet, k)
				}
			}
		}
		if !changed {
			return openJSON, mergedJSON, unverifiedJSON, false
		}
		nextOpen := make([]string, 0, len(openSet))
		for k := range openSet {
			nextOpen = append(nextOpen, k)
		}
		sort.Strings(nextOpen)
		nextMerged := make([]string, 0, len(mergedSet))
		for k := range mergedSet {
			nextMerged = append(nextMerged, k)
		}
		sort.Strings(nextMerged)
		sort.Strings(remUnverified)
		ob, _ := json.Marshal(nonNilSlice(nextOpen))
		mb, _ := json.Marshal(nonNilSlice(nextMerged))
		ub, _ := json.Marshal(nonNilSlice(remUnverified))
		return string(ob), string(mb), string(ub), true
	}

	for _, num := range issueNums {
		var openJSON, mergedJSON, unverifiedJSON, autoBase string
		err := tx.QueryRow(`
SELECT baseline_open_prs_json, baseline_merged_prs_json, unverified_open_prs_json, auto_baseline_json
FROM issue_triage_state
WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&openJSON, &mergedJSON, &unverifiedJSON, &autoBase)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		nextOpen, nextMerged, nextUnverified, mainChanged := reconcileSet(openJSON, mergedJSON, unverifiedJSON)
		autoChanged := false
		if autoBase != "" {
			var saved savedTriageBaseline
			if json.Unmarshal([]byte(autoBase), &saved) == nil {
				ao, am, au, ac := reconcileSet(saved.BaselineOpenPRsJSON, saved.BaselineMergedPRsJSON, saved.UnverifiedOpenPRsJSON)
				if ac {
					saved.BaselineOpenPRsJSON = ao
					saved.BaselineMergedPRsJSON = am
					saved.UnverifiedOpenPRsJSON = au
					if b, mErr := json.Marshal(saved); mErr == nil {
						autoBase = string(b)
						autoChanged = true
					}
				}
			}
		}
		if !mainChanged && !autoChanged {
			continue
		}
		if _, err := tx.Exec(`
UPDATE issue_triage_state
SET baseline_open_prs_json = ?, baseline_merged_prs_json = ?, unverified_open_prs_json = ?, auto_baseline_json = ?
WHERE repo = ? AND number = ?`,
			nextOpen, nextMerged, nextUnverified, autoBase, DefaultRepo, num); err != nil {
			return err
		}
	}
	return nil
}

func mergeTrackSources(existing, incoming []string) ([]string, bool) {
	seen := make(map[string]bool, len(existing)+len(incoming))
	for _, s := range existing {
		if s != "" {
			seen[s] = true
		}
	}
	added := false
	for _, s := range incoming {
		if s != "" && !seen[s] {
			seen[s] = true
			added = true
		}
	}
	if !added {
		return existing, false
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, true
}

func (s *Store) UpsertPRStatuses(statuses []model.PRStatus, syncTime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmtPR, err := tx.Prepare(`
INSERT INTO pull_requests(repo, number, state, is_cherry_pick, updated_at, status_json)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  state=excluded.state, is_cherry_pick=excluded.is_cherry_pick,
  updated_at=excluded.updated_at, status_json=excluded.status_json`)
	if err != nil {
		return err
	}
	defer stmtPR.Close()

	stmtLinkOne, err := tx.Prepare(`
UPDATE issue_pr_links
SET state = ?, title = ?, is_cherry_pick = ?, updated_at = CASE WHEN ? != '' THEN ? ELSE updated_at END
WHERE issue_repo = ? AND issue_num = ? AND pr_repo = ? AND pr_num = ?`)
	if err != nil {
		return err
	}
	defer stmtLinkOne.Close()

	affectedIssues := make(map[int]bool)
	verifiedMap := make(map[string]verifiedPRStateLite, len(statuses))

	for _, st := range statuses {
		effSt := st
		var curState, curUpdAt, curRaw string
		var curIsCP int
		rowErr := tx.QueryRow(`SELECT state, is_cherry_pick, updated_at, status_json FROM pull_requests WHERE repo = ? AND number = ?`,
			st.Repo, st.Num).Scan(&curState, &curIsCP, &curUpdAt, &curRaw)
		if rowErr != nil && rowErr != sql.ErrNoRows {
			return rowErr
		}
		if rowErr == nil {
			var curObj model.PRStatus
			_ = json.Unmarshal([]byte(curRaw), &curObj)
			if isIncomingPRSnapshotStale(curState, curUpdAt, st.State, st.UpdatedAt) {
				// Incoming snapshot is older than the already-committed row: preserve fresher state,
				// only merging any newly discovered TrackSources if present.
				if mergedSrcs, changed := mergeTrackSources(curObj.TrackSources, st.TrackSources); changed {
					curObj.TrackSources = mergedSrcs
					if mb, mErr := json.Marshal(curObj); mErr == nil {
						if _, uErr := tx.Exec(`UPDATE pull_requests SET status_json = ? WHERE repo = ? AND number = ?`,
							string(mb), st.Repo, st.Num); uErr != nil {
							return uErr
						}
					}
				}
				verifiedMap[model.PRKey(st.Repo, st.Num)] = verifiedPRStateLite{state: curState, isCP: curIsCP != 0}
				continue
			}
			if mergedSrcs, changed := mergeTrackSources(curObj.TrackSources, st.TrackSources); changed {
				effSt.TrackSources = mergedSrcs
			} else if len(effSt.TrackSources) == 0 && len(curObj.TrackSources) > 0 {
				effSt.TrackSources = curObj.TrackSources
			}
		}

		b, err := json.Marshal(effSt)
		if err != nil {
			return err
		}
		if _, err := stmtPR.Exec(effSt.Repo, effSt.Num, effSt.State, boolToInt(effSt.IsCherryPick), effSt.UpdatedAt, string(b)); err != nil {
			return err
		}
		verifiedMap[model.PRKey(effSt.Repo, effSt.Num)] = verifiedPRStateLite{state: effSt.State, isCP: effSt.IsCherryPick}

		type linkRow struct {
			issueNum int
			state    string
			updAt    string
		}
		var links []linkRow
		lRows, err := tx.Query(`SELECT issue_num, state, updated_at FROM issue_pr_links WHERE issue_repo = ? AND pr_repo = ? AND pr_num = ?`,
			DefaultRepo, effSt.Repo, effSt.Num)
		if err != nil {
			return err
		}
		for lRows.Next() {
			var lr linkRow
			if sErr := lRows.Scan(&lr.issueNum, &lr.state, &lr.updAt); sErr != nil {
				_ = lRows.Close()
				return sErr
			}
			links = append(links, lr)
		}
		if rErr := lRows.Err(); rErr != nil {
			_ = lRows.Close()
			return rErr
		}
		_ = lRows.Close()

		for _, lr := range links {
			affectedIssues[lr.issueNum] = true
			if isIncomingPRSnapshotStale(lr.state, lr.updAt, effSt.State, effSt.UpdatedAt) {
				continue
			}
			if _, err := stmtLinkOne.Exec(
				effSt.State, effSt.Title, boolToInt(effSt.IsCherryPick), effSt.UpdatedAt, effSt.UpdatedAt,
				DefaultRepo, lr.issueNum, effSt.Repo, effSt.Num,
			); err != nil {
				return err
			}
		}
	}

	if len(affectedIssues) > 0 {
		nums := make([]int, 0, len(affectedIssues))
		for n := range affectedIssues {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		if err := reconcileUnverifiedPRsInTx(tx, nums, verifiedMap); err != nil {
			return err
		}
		if err := s.reevaluateIssueRetriageInTx(tx, nums); err != nil {
			return err
		}
	}

	if !syncTime.IsZero() {
		isoUTC := syncTime.UTC().Format("2006-01-02T15:04:05Z")
		prMeta, _ := json.Marshal(map[string]string{
			"pr_snapshot_utc":  isoUTC,
			"pr_snapshot_utc8": prstatus.ToUTC8Display(isoUTC) + " (UTC+8)",
		})
		if _, err := tx.Exec(`INSERT INTO kv_meta(key, val_json) VALUES('pr_sync_meta', ?) ON CONFLICT(key) DO UPDATE SET val_json=excluded.val_json`, string(prMeta)); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) preserveAutoBaselineBeforeOverrideInTx(tx *sql.Tx, number int) error {
	var autoBase string
	var cur savedTriageBaseline
	err := tx.QueryRow(`
SELECT
  auto_baseline_json,
  triaged_at, triaged_by,
  baseline_labels_json, baseline_gh_severity, baseline_is_customer,
  baseline_open_prs_json, baseline_merged_prs_json, unverified_open_prs_json,
  comment_watermark_utc
FROM issue_triage_state
WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(
		&autoBase,
		&cur.TriagedAt, &cur.TriagedBy,
		&cur.BaselineLabelsJSON, &cur.BaselineGHSeverity, &cur.BaselineIsCustomer,
		&cur.BaselineOpenPRsJSON, &cur.BaselineMergedPRsJSON, &cur.UnverifiedOpenPRsJSON,
		&cur.CommentWatermarkUTC,
	)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if autoBase != "" {
		return nil
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE issue_triage_state SET auto_baseline_json = ? WHERE repo = ? AND number = ?`,
		string(b), DefaultRepo, number)
	return err
}

func (s *Store) applyIssueOverrideInTx(tx *sql.Tx, number int, req model.TriagePatchRequest, nowISO string) error {
	if rawIf := strings.TrimSpace(req.IfUnmodifiedSince); rawIf != "" {
		norm, ok := model.NormalizeRFC3339NanoFixed(rawIf)
		if !ok {
			return fmt.Errorf("invalid `if_unmodified_since` timestamp %q (expected RFC3339 UTC)", req.IfUnmodifiedSince)
		}
		req.IfUnmodifiedSince = norm
	}
	req.Severity = model.NormalizeSeverityForCategory(req.Cat, req.Severity, "MODERATE")
	if err := model.ValidateTriageFields(req.Cat, req.Severity, req.Subsystem); err != nil {
		return err
	}
	if err := triage.ValidateSingleNoteSyntax(number, req.Note); err != nil {
		return err
	}
	if !model.IsRealBugCategory(req.Cat) {
		req.IsBorderlineKept = false
	}

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("issue #%d not found: %w", number, ErrIssueNotFound)
	}
	if req.IfUnmodifiedSince != "" {
		if conflict, msg := checkOverrideConflict(tx, number, req, ""); conflict {
			return fmt.Errorf("%w: %s", ErrOverrideConflict, msg)
		}
	}

	if err := s.preserveAutoBaselineBeforeOverrideInTx(tx, number); err != nil {
		return err
	}

	hasBdr := 0
	var bdrVal sql.NullInt64
	if !model.IsRealBugCategory(req.Cat) {
		hasBdr = 1
		bdrVal = sql.NullInt64{Int64: 0, Valid: true}
	} else if req.HasBorderlineField() {
		hasBdr = 1
		bdrVal = sql.NullInt64{Int64: int64(boolToInt(req.IsBorderlineKept)), Valid: true}
	}

	hasFK := boolToInt(req.HasForceKeepField())
	fkVal := boolToInt(req.ForceKeep)

	nZh, nEn := "", ""
	hasNote := 0
	if req.Note != nil {
		nZh = strings.TrimSpace(req.Note.ZhCN)
		nEn = strings.TrimSpace(req.Note.En)
		hasNote = 1
	}

	if _, err := tx.Exec(`
INSERT INTO issue_overrides(
  repo, number, cat, severity, subsystem, is_borderline_kept,
  force_keep, has_force_keep_override,
  note_zh, note_en, has_note_override, is_new_decision, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  cat=excluded.cat,
  severity=excluded.severity,
  subsystem=excluded.subsystem,
  is_borderline_kept=CASE WHEN ? = 1 THEN excluded.is_borderline_kept ELSE issue_overrides.is_borderline_kept END,
  force_keep=CASE WHEN excluded.has_force_keep_override = 1 THEN excluded.force_keep ELSE issue_overrides.force_keep END,
  has_force_keep_override=CASE WHEN excluded.has_force_keep_override = 1 THEN 1 ELSE issue_overrides.has_force_keep_override END,
  note_zh=CASE WHEN excluded.has_note_override = 1 THEN excluded.note_zh ELSE issue_overrides.note_zh END,
  note_en=CASE WHEN excluded.has_note_override = 1 THEN excluded.note_en ELSE issue_overrides.note_en END,
  has_note_override=CASE WHEN excluded.has_note_override = 1 THEN 1 ELSE issue_overrides.has_note_override END,
  is_new_decision=0,
  updated_at=excluded.updated_at`,
		DefaultRepo, number, req.Cat, req.Severity, req.Subsystem, bdrVal,
		fkVal, hasFK,
		nZh, nEn, hasNote, nowISO,
		hasBdr,
	); err != nil {
		return err
	}

	return s.snapshotBaselineInTx(tx, number, "override", nowISO)
}

func (s *Store) ApplyIssueOverride(number int, req model.TriagePatchRequest) ([]prstatus.PRTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyIssueOverrideLocked(number, req)
}

func (s *Store) applyIssueOverrideLocked(number int, req model.TriagePatchRequest) ([]prstatus.PRTarget, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	nowISO := model.NowRFC3339Nano()
	if err := s.applyIssueOverrideInTx(tx, number, req, nowISO); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.listMissingOpenPRTargetsLocked()
}

func (s *Store) SaveIssueOverride(number int, req model.TriagePatchRequest) (model.IssueRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.applyIssueOverrideLocked(number, req); err != nil {
		return model.IssueRecord{}, err
	}

	ds, err := s.buildDatasetLocked(true)
	if err != nil {
		return model.IssueRecord{}, err
	}
	for _, r := range ds.Records {
		if r.Number == number {
			return r, nil
		}
	}
	return model.IssueRecord{}, fmt.Errorf("issue #%d missing after update: %w", number, ErrIssueNotFound)
}

func (s *Store) ResetIssueOverride(number int) ([]prstatus.PRTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetIssueOverrideLocked(number)
}

func (s *Store) resetIssueOverrideLocked(number int) ([]prstatus.PRTarget, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("issue #%d not found: %w", number, ErrIssueNotFound)
	}

	if _, err := tx.Exec(`DELETE FROM issue_overrides WHERE repo = ? AND number = ?`, DefaultRepo, number); err != nil {
		return nil, err
	}

	nowISO := model.NowRFC3339Nano()
	restoredBaseline := false
	var autoBase string
	if err := tx.QueryRow(`SELECT auto_baseline_json FROM issue_triage_state WHERE repo = ? AND number = ?`,
		DefaultRepo, number).Scan(&autoBase); err == nil && autoBase != "" {
		var saved savedTriageBaseline
		if json.Unmarshal([]byte(autoBase), &saved) == nil && saved.BaselineLabelsJSON != "" {
			unver := saved.UnverifiedOpenPRsJSON
			if strings.TrimSpace(unver) == "" {
				unver = "[]"
			}
			if _, err := tx.Exec(`
UPDATE issue_triage_state
SET
  triaged_at = ?,
  triaged_by = ?,
  baseline_labels_json = ?,
  baseline_gh_severity = ?,
  baseline_is_customer = ?,
  baseline_open_prs_json = ?,
  baseline_merged_prs_json = ?,
  unverified_open_prs_json = ?,
  comment_watermark_utc = ?
WHERE repo = ? AND number = ?`,
				nowISO, saved.TriagedBy,
				saved.BaselineLabelsJSON, saved.BaselineGHSeverity, saved.BaselineIsCustomer,
				saved.BaselineOpenPRsJSON, saved.BaselineMergedPRsJSON, unver,
				saved.CommentWatermarkUTC,
				DefaultRepo, number,
			); err != nil {
				return nil, err
			}
			restoredBaseline = true
		}
	}
	if !restoredBaseline {
		_, _ = tx.Exec(`UPDATE issue_triage_state SET triaged_at = ? WHERE repo = ? AND number = ?`, nowISO, DefaultRepo, number)
	}

	if err := s.reevaluateIssueRetriageInTx(tx, []int{number}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.listMissingOpenPRTargetsLocked()
}

func (s *Store) DeleteIssueOverride(number int) (model.IssueRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.resetIssueOverrideLocked(number); err != nil {
		return model.IssueRecord{}, err
	}

	ds, err := s.buildDatasetLocked(true)
	if err != nil {
		return model.IssueRecord{}, err
	}
	for _, r := range ds.Records {
		if r.Number == number {
			return r, nil
		}
	}
	return model.IssueRecord{}, fmt.Errorf("issue #%d not found: %w", number, ErrIssueNotFound)
}

func (s *Store) ListPendingCandidateNumbers() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT number FROM pending_candidates WHERE repo = ? ORDER BY number DESC`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]int, 0)
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) ListRetriageNumbers() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
SELECT i.number
FROM issues i
JOIN issue_triage_state ts ON i.repo = ts.repo AND i.number = ts.number
WHERE i.repo = ? AND ts.needs_retriage = 1
ORDER BY i.number DESC`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]int, 0)
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) ListPendingCandidates() ([]model.PendingCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT payload_json, revision FROM pending_candidates WHERE repo = ? ORDER BY number DESC`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.PendingCandidate, 0)
	for rows.Next() {
		var raw string
		var rev int
		if err := rows.Scan(&raw, &rev); err != nil {
			return nil, err
		}
		var pc model.PendingCandidate
		if err := json.Unmarshal([]byte(raw), &pc); err != nil {
			return nil, err
		}
		pc.Revision = rev
		out = append(out, pc)
	}
	return out, rows.Err()
}

func (s *Store) ResolvePendingCandidate(number int, req model.PendingResolveRequest) error {
	_, err := s.ResolvePendingCandidateWithTargets(number, req)
	return err
}

func (s *Store) ResolvePendingCandidateWithTargets(number int, req model.PendingResolveRequest) ([]prstatus.PRTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resolvePendingCandidateLocked(number, req); err != nil {
		return nil, err
	}
	if req.Action != "accept" {
		return nil, nil
	}
	return s.listMissingOpenPRTargetsLocked()
}

func isCanonicalLabelList(labels []string) bool {
	hasPlanner := false
	hasBug := false
	for _, lb := range labels {
		if lb == "sig/planner" {
			hasPlanner = true
		}
		if lb == "type/bug" {
			hasBug = true
		}
	}
	return hasPlanner && hasBug
}

func (s *Store) resolvePendingCandidateLocked(number int, req model.PendingResolveRequest) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	nowISO := model.NowRFC3339Nano()
	if err := s.resolvePendingCandidateInTx(tx, number, req, nowISO); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) resolvePendingCandidateInTx(tx *sql.Tx, number int, req model.PendingResolveRequest, nowISO string) error {
	if norm, ok := model.NormalizeRFC3339NanoFixed(nowISO); ok {
		nowISO = norm
	} else if strings.TrimSpace(nowISO) == "" {
		nowISO = model.NowRFC3339Nano()
	}
	var raw string
	err := tx.QueryRow(`SELECT payload_json FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&raw)
	if err == sql.ErrNoRows {
		return fmt.Errorf("pending candidate #%d not found: %w", number, ErrIssueNotFound)
	}
	if err != nil {
		return err
	}
	var pc model.PendingCandidate
	if err := json.Unmarshal([]byte(raw), &pc); err != nil {
		return err
	}

	isCan := isCanonicalLabelList(pc.Labels)

	if req.Action == "exclude" {
		if isCan {
			return fmt.Errorf("%w: #%d must be accepted with a FILTER_* category", ErrCanonicalCannotExclude, number)
		}
		reason := strings.TrimSpace(req.ExcludeReason)
		if reason == "" {
			reason = "Excluded non-optimizer issue via triage"
		}
		lbsJSON, _ := json.Marshal(nonNilSlice(pc.Labels))
		if _, err := tx.Exec(`
INSERT INTO excluded_issues(repo, number, title, reason, excluded_labels_json, source, excluded_at)
VALUES (?, ?, ?, ?, ?, 'pending_triage', ?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title,
  reason=excluded.reason,
  excluded_labels_json=excluded.excluded_labels_json,
  source=excluded.source,
  excluded_at=excluded.excluded_at`,
			DefaultRepo, number, pc.Title, reason, string(lbsJSON), nowISO); err != nil {
			return err
		}
		_, _ = tx.Exec(`DELETE FROM archived_issues WHERE repo = ? AND number = ?`, DefaultRepo, number)
		if _, err := tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number); err != nil {
			return err
		}
		_ = removeRecheckNumInTx(tx, number)
		return nil
	}

	if req.Action != "accept" {
		return fmt.Errorf("invalid action %q", req.Action)
	}
	req.Severity = model.NormalizeSeverityForCategory(req.Cat, req.Severity, "MAJOR")
	if err := model.ValidateTriageFields(req.Cat, req.Severity, req.Subsystem); err != nil {
		return err
	}
	if err := triage.ValidateSingleNoteSyntax(number, req.Note); err != nil {
		return err
	}
	if !model.IsRealBugCategory(req.Cat) {
		req.IsBorderlineKept = false
	}

	ghSev := "unlabeled"
	isCust := false
	for _, lb := range pc.Labels {
		if strings.HasPrefix(lb, "severity/") && ghSev == "unlabeled" {
			ghSev = strings.SplitN(lb, "/", 2)[1]
		}
		if lb == "report/customer" {
			isCust = true
		}
	}

	nZh, nEn := "", ""
	if req.Note != nil {
		nZh = strings.TrimSpace(req.Note.ZhCN)
		nEn = strings.TrimSpace(req.Note.En)
	}
	asgJSON, _ := json.Marshal(nonNilSlice(pc.Assignees))
	lbsJSON, _ := json.Marshal(nonNilSlice(pc.Labels))
	cmtsJSON, _ := json.Marshal(nonNilSlice(pc.RecentComments))
	created := pc.CreatedAt
	if len(created) >= 10 {
		created = created[:10]
	}

	if _, err := tx.Exec(`
INSERT INTO issues(
  repo, number, title, body, author, assignees_json, created, created_at, updated_at,
  labels_json, is_canonical, is_customer, gh_severity,
  auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title,
  body=CASE WHEN excluded.body != '' THEN excluded.body ELSE issues.body END,
  author=excluded.author,
  assignees_json=excluded.assignees_json,
  updated_at=excluded.updated_at,
  labels_json=excluded.labels_json,
  is_canonical=excluded.is_canonical,
  is_customer=excluded.is_customer,
  gh_severity=excluded.gh_severity,
  auto_cat=excluded.auto_cat,
  auto_severity=excluded.auto_severity,
  auto_subsystem=excluded.auto_subsystem,
  auto_is_borderline=excluded.auto_is_borderline,
  auto_force_keep=excluded.auto_force_keep,
  auto_note_zh=excluded.auto_note_zh,
  auto_note_en=excluded.auto_note_en`,
		DefaultRepo, pc.Number, pc.Title, pc.Body, pc.Author, string(asgJSON), created, pc.CreatedAt, pc.UpdatedAt,
		string(lbsJSON), boolToInt(isCan), boolToInt(isCust), ghSev,
		req.Cat, req.Severity, req.Subsystem, boolToInt(req.IsBorderlineKept), boolToInt(req.ForceKeep), nZh, nEn,
	); err != nil {
		return err
	}

	hydratedPRs := make([]model.LinkedPRRef, 0, len(pc.PRs))
	for _, p := range pc.PRs {
		hp := p
		var cachedState, cachedUpdAt, cachedRaw string
		var cachedIsCP int
		if tx.QueryRow(`SELECT state, is_cherry_pick, updated_at, status_json FROM pull_requests WHERE repo = ? AND number = ?`,
			p.Repo, p.Num).Scan(&cachedState, &cachedIsCP, &cachedUpdAt, &cachedRaw) == nil {
			if !isIncomingPRSnapshotStale(hp.State, hp.UpdatedAt, cachedState, cachedUpdAt) {
				hp.State = cachedState
				hp.IsCherryPick = cachedIsCP != 0
				if cachedUpdAt != "" {
					hp.UpdatedAt = cachedUpdAt
				}
				var stObj model.PRStatus
				if json.Unmarshal([]byte(cachedRaw), &stObj) == nil && stObj.Title != "" {
					hp.Title = stObj.Title
				}
			}
		}
		hydratedPRs = append(hydratedPRs, hp)

		src := model.LinkSourceTimeline
		switch hp.Source {
		case model.LinkSourceManual:
			src = model.LinkSourceManual
		case model.LinkSourcePRRef:
			src = model.LinkSourcePRRef
		}
		if _, err := tx.Exec(`
INSERT INTO issue_pr_links(issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET
  state=excluded.state,
  title=excluded.title,
  is_cherry_pick=excluded.is_cherry_pick,
  updated_at=CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE issue_pr_links.updated_at END,
  source=CASE
    WHEN issue_pr_links.source = 'manual' OR excluded.source = 'manual' THEN 'manual'
    WHEN issue_pr_links.source = 'timeline' OR excluded.source = 'timeline' THEN 'timeline'
    ELSE excluded.source
  END`,
			DefaultRepo, pc.Number, hp.Repo, hp.Num, hp.Label, hp.URL, hp.State, hp.Title, boolToInt(hp.IsCherryPick), hp.UpdatedAt, src,
		); err != nil {
			return err
		}
	}

	// Clean up any stale excluded_issues or archived_issues or old overrides
	_, _ = tx.Exec(`DELETE FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number)
	_, _ = tx.Exec(`DELETE FROM archived_issues WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number)
	_, _ = tx.Exec(`DELETE FROM issue_overrides WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number)
	if _, err := tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number); err != nil {
		return err
	}
	_ = removeRecheckNumInTx(tx, pc.Number)

	// Initialize baseline in issue_triage_state using GitHub timestamps (never wall-clock nowISO)
	openKeys, mergedKeys := triage.ExtractMainlinePRKeySets(hydratedPRs)
	unverifiedKeys := computeUnverifiedOpenPRKeys(tx, hydratedPRs, true)
	openJSON, _ := json.Marshal(nonNilSlice(openKeys))
	mergedJSON, _ := json.Marshal(nonNilSlice(mergedKeys))
	unverifiedJSON, _ := json.Marshal(unverifiedKeys)
	watermark := model.NormalizeRFC3339UTC(pc.UpdatedAt)
	for _, c := range pc.RecentComments {
		watermark = model.MaxRFC3339(watermark, c.CreatedAt)
	}
	baseSnap, _ := json.Marshal(savedTriageBaseline{
		TriagedAt:             nowISO,
		TriagedBy:             "triage",
		BaselineLabelsJSON:    string(lbsJSON),
		BaselineGHSeverity:    ghSev,
		BaselineIsCustomer:    boolToInt(isCust),
		BaselineOpenPRsJSON:   string(openJSON),
		BaselineMergedPRsJSON: string(mergedJSON),
		UnverifiedOpenPRsJSON: string(unverifiedJSON),
		CommentWatermarkUTC:   watermark,
	})
	if _, err := tx.Exec(`
INSERT INTO issue_triage_state(
  repo, number, triaged_at, triaged_by,
  baseline_labels_json, baseline_gh_severity, baseline_is_customer,
  baseline_open_prs_json, baseline_merged_prs_json, unverified_open_prs_json,
  comment_watermark_utc, recent_comments_json, auto_baseline_json,
  needs_retriage, retriage_reasons_json, retriage_details_json
) VALUES (?, ?, ?, 'triage', ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '[]', '[]')
ON CONFLICT(repo, number) DO UPDATE SET
  triaged_at=excluded.triaged_at,
  triaged_by=excluded.triaged_by,
  baseline_labels_json=excluded.baseline_labels_json,
  baseline_gh_severity=excluded.baseline_gh_severity,
  baseline_is_customer=excluded.baseline_is_customer,
  baseline_open_prs_json=excluded.baseline_open_prs_json,
  baseline_merged_prs_json=excluded.baseline_merged_prs_json,
  unverified_open_prs_json=excluded.unverified_open_prs_json,
  comment_watermark_utc=excluded.comment_watermark_utc,
  recent_comments_json=excluded.recent_comments_json,
  auto_baseline_json=excluded.auto_baseline_json,
  needs_retriage=0,
  retriage_reasons_json='[]',
  retriage_details_json='[]'`,
		DefaultRepo, pc.Number, nowISO,
		string(lbsJSON), ghSev, boolToInt(isCust),
		string(openJSON), string(mergedJSON), string(unverifiedJSON),
		watermark, string(cmtsJSON), string(baseSnap),
	); err != nil {
		return err
	}

	reasonNote := nEn
	if reasonNote == "" {
		reasonNote = req.Cat
	}
	addedItem := &model.DeltaAddedIssue{
		Number:    pc.Number,
		Cat:       req.Cat,
		Severity:  req.Severity,
		Subsystem: req.Subsystem,
		Reason:    reasonNote,
	}
	s.recordLocalAcceptedDeltaInTx(tx, addedItem)
	return nil
}

// recordLocalAcceptedDeltaInTx records a locally triaged issue into NewOpenCandidatesAdded
// WITHOUT advancing SnapshotUTC or MaxIssueUpdatedUTC (which must only advance on real GitHub syncs).
func (s *Store) recordLocalAcceptedDeltaInTx(tx *sql.Tx, added *model.DeltaAddedIssue) {
	if added == nil {
		return
	}
	var raw string
	if err := tx.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'sync_meta'`).Scan(&raw); err != nil {
		return
	}
	var meta model.SyncMeta
	if json.Unmarshal([]byte(raw), &meta) != nil {
		return
	}
	if meta.DeltaSincePrevSnapshot == nil {
		meta.DeltaSincePrevSnapshot = &model.SnapshotDelta{
			PrevSnapshotDate:          meta.SnapshotUTC,
			ClosedIssuesRemoved:       []model.DeltaClosedIssue{},
			RelabeledGraduatedRemoved: []model.DeltaGraduatedIssue{},
			NewOpenCandidatesAdded:    []model.DeltaAddedIssue{},
		}
	}
	already := false
	for i, x := range meta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded {
		if x.Number == added.Number {
			meta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded[i] = *added
			already = true
			break
		}
	}
	if !already {
		meta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded = append(meta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded, *added)
	}
	if b, err := json.Marshal(meta); err == nil {
		_, _ = tx.Exec(`INSERT INTO kv_meta(key, val_json) VALUES('sync_meta', ?) ON CONFLICT(key) DO UPDATE SET val_json=excluded.val_json`, string(b))
	}
}

type SyncRunRecord struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Summary    any    `json:"summary"`
}

func (s *Store) RecordSyncRun(kind string, started, finished time.Time, summary any) {
	b, _ := json.Marshal(summary)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO sync_runs(kind, started_at, finished_at, summary_json) VALUES (?, ?, ?, ?)`,
		kind, started.UTC().Format(time.RFC3339), finished.UTC().Format(time.RFC3339), string(b))
	_, _ = s.db.Exec(`DELETE FROM sync_runs WHERE id NOT IN (SELECT id FROM sync_runs ORDER BY id DESC LIMIT 200)`)
}

func (s *Store) RecentSyncRuns(limit int) ([]SyncRunRecord, error) {
	if limit <= 0 {
		limit = 5
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, kind, started_at, finished_at, summary_json FROM sync_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SyncRunRecord, 0, limit)
	for rows.Next() {
		var r SyncRunRecord
		var raw string
		if err := rows.Scan(&r.ID, &r.Kind, &r.StartedAt, &r.FinishedAt, &raw); err != nil {
			return nil, err
		}
		var parsed any
		if json.Unmarshal([]byte(raw), &parsed) == nil {
			r.Summary = parsed
		} else {
			r.Summary = raw
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type BatchTriageResult struct {
	AcceptedIssues     []int                  `json:"accepted_issues"`
	ExcludedIssues     []int                  `json:"excluded_issues"`
	OverriddenIssues   []int                  `json:"overridden_issues"`
	AcknowledgedIssues []int                  `json:"acknowledged_issues,omitempty"`
	ManualPRsLinked    int                    `json:"manual_prs_linked,omitempty"`
	SkippedConflicts   []model.TriageConflict `json:"skipped_conflicts,omitempty"`
	RemainingPending   int                    `json:"remaining_pending"`
	RemainingRetriage  int                    `json:"remaining_retriage"`
	MissingPRTargets   []prstatus.PRTarget    `json:"-"`
	ManualLinkedIssues []int                  `json:"-"`
}

type BatchApplyOptions struct {
	SkipConflicts bool
}

func sortedNumericKeys[V any](m map[string]V) ([]int, error) {
	nums := make([]int, 0, len(m))
	seen := make(map[int]string, len(m))
	for k := range m {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(k), "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid issue number key %q", k)
		}
		if prevKey, dup := seen[n]; dup {
			return nil, fmt.Errorf("duplicate issue key %q and %q", prevKey, k)
		}
		seen[n] = k
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] > nums[j] })
	return nums, nil
}

func isIssueKnownNonPending(q sqlExecQuerier, num int) (inIssues, inExcluded, inArchived bool) {
	var c int
	if q.QueryRow(`SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&c) == nil && c > 0 {
		inIssues = true
	}
	c = 0
	if q.QueryRow(`SELECT COUNT(*) FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&c) == nil && c > 0 {
		inExcluded = true
	}
	c = 0
	if q.QueryRow(`SELECT COUNT(*) FROM archived_issues WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&c) == nil && c > 0 {
		inArchived = true
	}
	return
}

func queryIssueLastTriageMod(q sqlExecQuerier, num int) string {
	var ovrUpdated, triagedAt string
	_ = q.QueryRow(`SELECT COALESCE((SELECT updated_at FROM issue_overrides WHERE repo = ? AND number = ?), '')`, DefaultRepo, num).Scan(&ovrUpdated)
	_ = q.QueryRow(`SELECT COALESCE((SELECT triaged_at FROM issue_triage_state WHERE repo = ? AND number = ?), '')`, DefaultRepo, num).Scan(&triagedAt)
	return model.MaxRFC3339(ovrUpdated, triagedAt)
}

func checkOverrideConflict(q sqlExecQuerier, num int, patch model.TriagePatchRequest, baseExportedAt string) (bool, string) {
	curMod := queryIssueLastTriageMod(q, num)
	if patch.IfUnmodifiedSince != "" && curMod != "" && model.CompareRFC3339(curMod, patch.IfUnmodifiedSince) > 0 {
		return true, fmt.Sprintf("issue #%d was modified at %s (expected unmodified since %s)", num, curMod, patch.IfUnmodifiedSince)
	}
	if patch.IfUnmodifiedSince == "" && baseExportedAt != "" && curMod != "" && model.CompareRFC3339(curMod, baseExportedAt) > 0 {
		return true, fmt.Sprintf("issue #%d triage state was updated at %s after export %s", num, curMod, baseExportedAt)
	}
	return false, ""
}

func lookupByIssueNum[V any](m map[string]V, n int) (V, bool) {
	for _, k := range []string{strconv.Itoa(n), "#" + strconv.Itoa(n)} {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func planBatchDecisions(q sqlExecQuerier, dec model.BatchTriageDecisions, opts BatchApplyOptions) (model.BatchTriageDecisions, []model.TriageConflict, error) {
	newNums, err := sortedNumericKeys(dec.NewIssues)
	if err != nil {
		return model.BatchTriageDecisions{}, nil, err
	}
	exNums, err := sortedNumericKeys(dec.ExcludeIssues)
	if err != nil {
		return model.BatchTriageDecisions{}, nil, err
	}
	ovrNums, err := sortedNumericKeys(dec.Overrides)
	if err != nil {
		return model.BatchTriageDecisions{}, nil, err
	}
	manNums, err := sortedNumericKeys(dec.ManualPRLinks)
	if err != nil {
		return model.BatchTriageDecisions{}, nil, err
	}

	planned := model.BatchTriageDecisions{
		BaseExportedAt: dec.BaseExportedAt,
		NewIssues:      make(map[string]model.TriagePatchRequest),
		ExcludeIssues:  make(map[string]string),
		Overrides:      make(map[string]model.TriagePatchRequest),
		ManualPRLinks:  make(map[string][]string),
	}

	var conflicts []model.TriageConflict
	declaredNewSet := make(map[int]bool, len(newNums))
	acceptedSet := make(map[int]bool, len(newNums))
	skippedIssueSet := make(map[int]bool)
	removedPendingSet, _ := loadRemovedPendingNums(q)
	conflictErr := func(msg string) error {
		return fmt.Errorf("%w: %s", ErrBatchConflict, msg)
	}

	// 1. Plan new_issues
	for _, num := range newNums {
		declaredNewSet[num] = true
		var cnt int
		if err := q.QueryRow(`SELECT COUNT(*) FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&cnt); err != nil {
			return model.BatchTriageDecisions{}, nil, err
		}
		if cnt == 0 {
			inIss, inEx, inArch := isIssueKnownNonPending(q, num)
			wasRemoved := removedPendingSet[num]
			if inIss || inEx || inArch || wasRemoved {
				if opts.SkipConflicts {
					skippedIssueSet[num] = true
					conflicts = append(conflicts, model.TriageConflict{
						Issue:   num,
						Action:  "new_issues",
						Kind:    "already_resolved",
						Message: fmt.Sprintf("pending candidate #%d was already resolved or removed concurrently; skipped", num),
					})
					continue
				}
				return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("accept new issue #%d: pending candidate #%d was already resolved or removed concurrently", num, num))
			}
			return model.BatchTriageDecisions{}, nil, fmt.Errorf("accept new issue #%d: pending candidate #%d not found: %w", num, num, ErrIssueNotFound)
		}
		patch, _ := lookupByIssueNum(dec.NewIssues, num)
		planned.NewIssues[strconv.Itoa(num)] = patch
		acceptedSet[num] = true
	}

	// 2. Plan exclude_issues
	for _, num := range exNums {
		if declaredNewSet[num] {
			return model.BatchTriageDecisions{}, nil, fmt.Errorf("issue #%d cannot appear in both new_issues and exclude_issues", num)
		}
		reason, _ := lookupByIssueNum(dec.ExcludeIssues, num)
		var raw string
		if err := q.QueryRow(`SELECT payload_json FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&raw); err == nil {
			var pc model.PendingCandidate
			if json.Unmarshal([]byte(raw), &pc) == nil && isCanonicalLabelList(pc.Labels) {
				return model.BatchTriageDecisions{}, nil, fmt.Errorf("%w: #%d must be accepted with a FILTER_* category", ErrCanonicalCannotExclude, num)
			}
			planned.ExcludeIssues[strconv.Itoa(num)] = reason
			continue
		}

		inIss, inEx, inArch := isIssueKnownNonPending(q, num)
		wasRemoved := removedPendingSet[num]
		if !inIss {
			if inEx {
				if opts.SkipConflicts {
					skippedIssueSet[num] = true
					conflicts = append(conflicts, model.TriageConflict{
						Issue:   num,
						Action:  "exclude_issues",
						Kind:    "already_resolved",
						Message: fmt.Sprintf("issue #%d is already excluded; skipped", num),
					})
					continue
				}
				if dec.BaseExportedAt != "" {
					var liveExAt string
					_ = q.QueryRow(`SELECT excluded_at FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&liveExAt)
					if liveExAt != "" && model.CompareRFC3339(liveExAt, dec.BaseExportedAt) > 0 {
						return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("exclude issue #%d: issue was already excluded at %s after export %s", num, liveExAt, dec.BaseExportedAt))
					}
				}
				planned.ExcludeIssues[strconv.Itoa(num)] = reason
				continue
			}
			if inArch || wasRemoved {
				if opts.SkipConflicts {
					skippedIssueSet[num] = true
					conflicts = append(conflicts, model.TriageConflict{
						Issue:   num,
						Action:  "exclude_issues",
						Kind:    "issue_not_active",
						Message: fmt.Sprintf("issue #%d is no longer active (closed or removed from pending); skipped exclude", num),
					})
					continue
				}
				return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("exclude issue #%d: issue is already closed or removed", num))
			}
			return model.BatchTriageDecisions{}, nil, fmt.Errorf("exclude issue #%d: issue not found in database: %w", num, ErrIssueNotFound)
		}

		var isCan int
		if err := q.QueryRow(`SELECT is_canonical FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&isCan); err == nil && isCan != 0 {
			return model.BatchTriageDecisions{}, nil, fmt.Errorf("%w: #%d (classify as FILTER_* instead)", ErrCanonicalCannotExclude, num)
		}
		if dec.BaseExportedAt != "" {
			curMod := queryIssueLastTriageMod(q, num)
			if curMod != "" && model.CompareRFC3339(curMod, dec.BaseExportedAt) > 0 {
				msg := fmt.Sprintf("issue #%d was modified at %s after export %s", num, curMod, dec.BaseExportedAt)
				if opts.SkipConflicts {
					skippedIssueSet[num] = true
					conflicts = append(conflicts, model.TriageConflict{
						Issue:   num,
						Action:  "exclude_issues",
						Kind:    "concurrent_override",
						Message: msg + "; skipped exclude",
					})
					continue
				}
				return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("exclude issue #%d: %s", num, msg))
			}
		}
		planned.ExcludeIssues[strconv.Itoa(num)] = reason
	}

	// 3. Plan overrides (evaluated BEFORE manual_pr_links so conflict-skipped overrides also skip companion manual_pr_links)
	plannedOverridesSet := make(map[int]bool, len(ovrNums))
	for _, num := range ovrNums {
		if declaredNewSet[num] {
			return model.BatchTriageDecisions{}, nil, fmt.Errorf("issue #%d cannot appear in both new_issues and overrides", num)
		}
		inIss, inEx, inArch := isIssueKnownNonPending(q, num)
		if !inIss {
			if inEx || inArch {
				if opts.SkipConflicts {
					skippedIssueSet[num] = true
					conflicts = append(conflicts, model.TriageConflict{
						Issue:   num,
						Action:  "overrides",
						Kind:    "issue_not_active",
						Message: fmt.Sprintf("issue #%d is no longer active (excluded or closed); skipped override", num),
					})
					continue
				}
				return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("override issue #%d: issue is no longer active (excluded or closed)", num))
			}
			return model.BatchTriageDecisions{}, nil, fmt.Errorf("override issue #%d: issue not found in database: %w", num, ErrIssueNotFound)
		}
		patch, _ := lookupByIssueNum(dec.Overrides, num)
		if conflict, msg := checkOverrideConflict(q, num, patch, dec.BaseExportedAt); conflict {
			if opts.SkipConflicts {
				skippedIssueSet[num] = true
				conflicts = append(conflicts, model.TriageConflict{
					Issue:   num,
					Action:  "overrides",
					Kind:    "concurrent_override",
					Message: msg + "; skipped override",
				})
				continue
			}
			return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("override issue #%d: %s", num, msg))
		}
		planned.Overrides[strconv.Itoa(num)] = patch
		plannedOverridesSet[num] = true
	}

	// 4. Plan manual_pr_links
	for _, num := range manNums {
		if skippedIssueSet[num] {
			conflicts = append(conflicts, model.TriageConflict{
				Issue:   num,
				Action:  "manual_pr_links",
				Kind:    "companion_skipped",
				Message: fmt.Sprintf("issue #%d primary triage action was skipped due to conflict; skipped manual_pr_links", num),
			})
			continue
		}
		if !acceptedSet[num] {
			inIss, inEx, inArch := isIssueKnownNonPending(q, num)
			if !inIss {
				if inEx || inArch {
					if opts.SkipConflicts {
						conflicts = append(conflicts, model.TriageConflict{
							Issue:   num,
							Action:  "manual_pr_links",
							Kind:    "issue_not_active",
							Message: fmt.Sprintf("issue #%d is no longer active (excluded or closed); skipped manual_pr_links", num),
						})
						continue
					}
					return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("manual_pr_links issue #%d: issue is no longer active", num))
				}
				return model.BatchTriageDecisions{}, nil, fmt.Errorf("manual_pr_links issue #%d not found: %w", num, ErrIssueNotFound)
			}
		}
		if specs, ok := lookupByIssueNum(dec.ManualPRLinks, num); ok && len(specs) > 0 {
			planned.ManualPRLinks[strconv.Itoa(num)] = specs
		}
	}

	// 5. Plan ack_retriage
	seenAck := make(map[int]bool, len(dec.AckRetriage))
	for _, num := range dec.AckRetriage {
		if num <= 0 || seenAck[num] {
			continue
		}
		seenAck[num] = true
		if skippedIssueSet[num] {
			conflicts = append(conflicts, model.TriageConflict{
				Issue:   num,
				Action:  "ack_retriage",
				Kind:    "companion_skipped",
				Message: fmt.Sprintf("issue #%d primary triage action was skipped due to conflict; skipped ack_retriage", num),
			})
			continue
		}
		if !acceptedSet[num] {
			inIss, inEx, inArch := isIssueKnownNonPending(q, num)
			if !inIss {
				if inEx || inArch {
					if opts.SkipConflicts {
						conflicts = append(conflicts, model.TriageConflict{
							Issue:   num,
							Action:  "ack_retriage",
							Kind:    "issue_not_active",
							Message: fmt.Sprintf("issue #%d is no longer active (excluded or closed); skipped ack_retriage", num),
						})
						continue
					}
					return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("ack_retriage issue #%d: issue is no longer active", num))
				}
				return model.BatchTriageDecisions{}, nil, fmt.Errorf("ack_retriage issue #%d not found: %w", num, ErrIssueNotFound)
			}
			if !plannedOverridesSet[num] && dec.BaseExportedAt != "" {
				curMod := queryIssueLastTriageMod(q, num)
				if curMod != "" && model.CompareRFC3339(curMod, dec.BaseExportedAt) > 0 {
					msg := fmt.Sprintf("issue #%d triage state was updated at %s after export %s", num, curMod, dec.BaseExportedAt)
					if opts.SkipConflicts {
						conflicts = append(conflicts, model.TriageConflict{
							Issue:   num,
							Action:  "ack_retriage",
							Kind:    "concurrent_override",
							Message: msg + "; skipped ack_retriage",
						})
						continue
					}
					return model.BatchTriageDecisions{}, nil, conflictErr(fmt.Sprintf("ack_retriage issue #%d: %s", num, msg))
				}
			}
		}
		planned.AckRetriage = append(planned.AckRetriage, num)
	}

	return planned, conflicts, nil
}

// PlanBatchDecisionsWithOptions validates database invariants and filters out conflict-skipped
// items (when opts.SkipConflicts is true) without mutating SQLite.
func (s *Store) PlanBatchDecisionsWithOptions(dec model.BatchTriageDecisions, opts BatchApplyOptions) (model.BatchTriageDecisions, []model.TriageConflict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return planBatchDecisions(s.db, dec, opts)
}

// ValidateBatchDecisionsReadOnly validates database-state invariants (pending existence,
// canonical exclusion prohibition, target issue existence) without mutating the database.
func (s *Store) ValidateBatchDecisionsReadOnly(dec model.BatchTriageDecisions) error {
	_, err := s.ValidateBatchDecisionsWithOptions(dec, BatchApplyOptions{SkipConflicts: false})
	return err
}

func (s *Store) ValidateBatchDecisionsWithOptions(dec model.BatchTriageDecisions, opts BatchApplyOptions) ([]model.TriageConflict, error) {
	_, conflicts, err := s.PlanBatchDecisionsWithOptions(dec, opts)
	return conflicts, err
}

func (s *Store) ApplyBatchTriageDecisions(dec model.BatchTriageDecisions) (BatchTriageResult, error) {
	return s.ApplyBatchTriageDecisionsWithOptions(dec, BatchApplyOptions{SkipConflicts: false})
}

func (s *Store) ApplyBatchTriageDecisionsWithOptions(dec model.BatchTriageDecisions, opts BatchApplyOptions) (BatchTriageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return BatchTriageResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	planned, conflicts, err := planBatchDecisions(tx, dec, opts)
	if err != nil {
		return BatchTriageResult{}, err
	}

	newNums, _ := sortedNumericKeys(planned.NewIssues)
	exNums, _ := sortedNumericKeys(planned.ExcludeIssues)
	ovrNums, _ := sortedNumericKeys(planned.Overrides)
	manNums, _ := sortedNumericKeys(planned.ManualPRLinks)

	nowISO := model.NowRFC3339Nano()
	res := BatchTriageResult{
		SkippedConflicts:   conflicts,
		ManualLinkedIssues: manNums,
	}
	acceptedSet := make(map[int]bool, len(newNums))

	// 1. Accept new issues
	for _, num := range newNums {
		patch := planned.NewIssues[strconv.Itoa(num)]
		req := model.PendingResolveRequest{
			Action:           "accept",
			Cat:              patch.Cat,
			Severity:         patch.Severity,
			Subsystem:        patch.Subsystem,
			IsBorderlineKept: patch.IsBorderlineKept,
			ForceKeep:        patch.ForceKeep,
			Note:             patch.Note,
		}
		if err := s.resolvePendingCandidateInTx(tx, num, req, nowISO); err != nil {
			return BatchTriageResult{}, fmt.Errorf("accept new issue #%d: %w", num, err)
		}
		acceptedSet[num] = true
		res.AcceptedIssues = append(res.AcceptedIssues, num)
	}

	// 2. Exclude issues
	for _, num := range exNums {
		reason := strings.TrimSpace(planned.ExcludeIssues[strconv.Itoa(num)])
		if reason == "" {
			reason = "Excluded non-optimizer issue via CLI triage"
		}
		var hasPend int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&hasPend)
		if hasPend > 0 {
			if err := s.resolvePendingCandidateInTx(tx, num, model.PendingResolveRequest{
				Action:        "exclude",
				ExcludeReason: reason,
			}, nowISO); err != nil {
				return BatchTriageResult{}, fmt.Errorf("exclude pending issue #%d: %w", num, err)
			}
		} else {
			if err := s.excludeExistingIssueInTx(tx, num, reason, false, nowISO); err != nil {
				return BatchTriageResult{}, fmt.Errorf("exclude issue #%d: %w", num, err)
			}
		}
		res.ExcludedIssues = append(res.ExcludedIssues, num)
	}

	// 3. Link manual PRs BEFORE overrides/ack so newly linked PRs are snapshotted into baselines
	if len(planned.ManualPRLinks) > 0 {
		cnt, err := s.addManualPRLinksInTx(tx, planned.ManualPRLinks, acceptedSet)
		if err != nil {
			return BatchTriageResult{}, err
		}
		res.ManualPRsLinked = cnt
	}

	// 4. Apply overrides
	for _, num := range ovrNums {
		patch := planned.Overrides[strconv.Itoa(num)]
		if err := s.applyIssueOverrideInTx(tx, num, patch, nowISO); err != nil {
			return BatchTriageResult{}, fmt.Errorf("override issue #%d: %w", num, err)
		}
		res.OverriddenIssues = append(res.OverriddenIssues, num)
	}

	// 5. Acknowledge retriage
	if len(planned.AckRetriage) > 0 {
		acked, err := s.acknowledgeBatchRetriageInTx(tx, planned.AckRetriage, "cli", nowISO)
		if err != nil {
			return BatchTriageResult{}, err
		}
		res.AcknowledgedIssues = acked
	}

	if err := tx.Commit(); err != nil {
		return BatchTriageResult{}, err
	}

	_ = s.db.QueryRow(`SELECT COUNT(*) FROM pending_candidates WHERE repo = ?`, DefaultRepo).Scan(&res.RemainingPending)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM issue_triage_state WHERE repo = ? AND needs_retriage = 1`, DefaultRepo).Scan(&res.RemainingRetriage)
	res.MissingPRTargets, _ = s.listMissingOpenPRTargetsLocked()
	return res, nil
}

func (s *Store) ListMissingOpenPRTargets() ([]prstatus.PRTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listMissingOpenPRTargetsLocked()
}

// HasPRStatuses returns formatted labels of any targets that are not present in pull_requests.
func (s *Store) HasPRStatuses(targets []prstatus.PRTarget) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var missing []string
	for _, t := range targets {
		var cnt int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pull_requests WHERE repo = ? AND number = ?`, t.Repo, t.Number).Scan(&cnt); err != nil {
			return nil, err
		}
		if cnt == 0 {
			missing = append(missing, prstatus.FormatPRLabel(t.Repo, t.Number))
		}
	}
	return missing, nil
}

const sqlInChunkSize = 400

// ListIssueMissingPRTargets returns open/draft PRs linked to the given tracked issue numbers
// that are not yet present in `pull_requests` (so callers can pre-fetch them before snapshotting baselines).
func (s *Store) ListIssueMissingPRTargets(issueNums ...int) ([]prstatus.PRTarget, error) {
	if len(issueNums) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool)
	var out []prstatus.PRTarget
	for start := 0; start < len(issueNums); start += sqlInChunkSize {
		end := start + sqlInChunkSize
		if end > len(issueNums) {
			end = len(issueNums)
		}
		chunk := issueNums[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, DefaultRepo)
		for i, n := range chunk {
			ph[i] = "?"
			args = append(args, n)
		}
		rows, err := s.db.Query(`
SELECT DISTINCT l.pr_repo, l.pr_num
FROM issue_pr_links l
LEFT JOIN pull_requests p ON l.pr_repo = p.repo AND l.pr_num = p.number
WHERE l.issue_repo = ? AND l.issue_num IN (`+strings.Join(ph, ",")+`)
  AND l.state IN ('OPEN', 'DRAFT')
  AND p.number IS NULL
ORDER BY l.pr_repo ASC, l.pr_num DESC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var repo string
			var num int
			if err := rows.Scan(&repo, &num); err != nil {
				_ = rows.Close()
				return nil, err
			}
			k := model.PRKey(repo, num)
			if !seen[k] {
				seen[k] = true
				out = append(out, prstatus.PRTarget{
					Repo:         repo,
					Number:       num,
					TrackSources: []string{"issue_linked"},
				})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// ListPendingMissingPRTargets returns open/draft PRs referenced on the given pending candidate numbers
// that are not yet present in `pull_requests`.
func (s *Store) ListPendingMissingPRTargets(issueNums ...int) ([]prstatus.PRTarget, error) {
	if len(issueNums) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool)
	var out []prstatus.PRTarget
	for start := 0; start < len(issueNums); start += sqlInChunkSize {
		end := start + sqlInChunkSize
		if end > len(issueNums) {
			end = len(issueNums)
		}
		chunk := issueNums[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, DefaultRepo)
		for i, n := range chunk {
			ph[i] = "?"
			args = append(args, n)
		}
		rows, err := s.db.Query(`SELECT payload_json FROM pending_candidates WHERE repo = ? AND number IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		var refs []model.LinkedPRRef
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return nil, err
			}
			var pc model.PendingCandidate
			if json.Unmarshal([]byte(raw), &pc) == nil {
				refs = append(refs, pc.PRs...)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()

		for _, r := range refs {
			if r.Repo == "" || r.Num <= 0 || r.IsCherryPick || (r.State != "OPEN" && r.State != "DRAFT") {
				continue
			}
			k := model.PRKey(r.Repo, r.Num)
			if seen[k] {
				continue
			}
			var cnt int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM pull_requests WHERE repo = ? AND number = ?`, r.Repo, r.Num).Scan(&cnt); err == nil && cnt > 0 {
				continue
			}
			seen[k] = true
			out = append(out, prstatus.PRTarget{
				Repo:         r.Repo,
				Number:       r.Num,
				TrackSources: []string{"issue_linked"},
			})
		}
	}
	return out, nil
}

func (s *Store) CountRetriageNeeded() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cnt int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM issue_triage_state WHERE repo = ? AND needs_retriage = 1`, DefaultRepo).Scan(&cnt)
	return cnt, err
}

// ClassifyTargetedPRSources resolves TrackSources for explicit PR targets without leaking
// issue-only PRs (such as PRs linked exclusively to FILTER_NON_OPT_OR_FIXED issues) onto
// the standalone PR board with a "manual_cli" tag.
func (s *Store) ClassifyTargetedPRSources(targets []prstatus.PRTarget) ([]prstatus.PRTarget, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pendingLinked := make(map[string]bool)
	pRows, err := s.db.Query(`SELECT payload_json FROM pending_candidates WHERE repo = ?`, DefaultRepo)
	if err == nil {
		for pRows.Next() {
			var raw string
			if pRows.Scan(&raw) == nil {
				var pc model.PendingCandidate
				if json.Unmarshal([]byte(raw), &pc) == nil {
					for _, pr := range pc.PRs {
						pendingLinked[model.PRKey(pr.Repo, pr.Num)] = true
					}
				}
			}
		}
		_ = pRows.Close()
	}

	out := make([]prstatus.PRTarget, 0, len(targets))
	for _, t := range targets {
		srcs := make(map[string]bool)
		for _, s0 := range t.TrackSources {
			if s0 != "" {
				srcs[s0] = true
			}
		}
		var rawStatus string
		if err := s.db.QueryRow(`SELECT status_json FROM pull_requests WHERE repo = ? AND number = ?`, t.Repo, t.Number).Scan(&rawStatus); err == nil {
			var st model.PRStatus
			if json.Unmarshal([]byte(rawStatus), &st) == nil {
				for _, s0 := range st.TrackSources {
					if s0 != "" {
						srcs[s0] = true
					}
				}
			}
		}
		var linkCnt int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM issue_pr_links WHERE pr_repo = ? AND pr_num = ?`, t.Repo, t.Number).Scan(&linkCnt)
		if linkCnt > 0 || pendingLinked[model.PRKey(t.Repo, t.Number)] {
			srcs["issue_linked"] = true
		}
		if len(srcs) == 0 {
			srcs["manual_cli"] = true
		}
		list := make([]string, 0, len(srcs))
		for s0 := range srcs {
			list = append(list, s0)
		}
		sort.Strings(list)
		out = append(out, prstatus.PRTarget{
			Repo:         t.Repo,
			Number:       t.Number,
			TrackSources: list,
		})
	}
	return out, nil
}

func (s *Store) listMissingOpenPRTargetsLocked() ([]prstatus.PRTarget, error) {
	rows, err := s.db.Query(`
SELECT DISTINCT l.pr_repo, l.pr_num
FROM issue_pr_links l
JOIN issues i ON l.issue_repo = i.repo AND l.issue_num = i.number
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
LEFT JOIN pull_requests p ON l.pr_repo = p.repo AND l.pr_num = p.number
WHERE l.state IN ('OPEN', 'DRAFT')
  AND COALESCE(NULLIF(o.cat, ''), i.auto_cat) != 'FILTER_NON_OPT_OR_FIXED'
  AND p.number IS NULL
ORDER BY l.pr_repo ASC, l.pr_num DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []prstatus.PRTarget
	for rows.Next() {
		var repo string
		var num int
		if err := rows.Scan(&repo, &num); err != nil {
			return nil, err
		}
		out = append(out, prstatus.PRTarget{
			Repo:         repo,
			Number:       num,
			TrackSources: []string{"issue_linked"},
		})
	}
	return out, rows.Err()
}

func (s *Store) BuildDataset(includeWorkbenchFlags bool) (model.Dataset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildDatasetLocked(includeWorkbenchFlags)
}

func sortLinkedPRs(prs []model.LinkedPRRef) {
	issuesync.SortLinkedPRRefs(prs)
}

func (s *Store) buildDatasetLocked(includeWorkbenchFlags bool) (model.Dataset, error) {
	var syncMeta model.SyncMeta
	var rawMeta string
	if err := s.db.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'sync_meta'`).Scan(&rawMeta); err == nil {
		_ = json.Unmarshal([]byte(rawMeta), &syncMeta)
	}
	var rawPRMeta string
	if err := s.db.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'pr_sync_meta'`).Scan(&rawPRMeta); err == nil {
		var pm map[string]string
		if json.Unmarshal([]byte(rawPRMeta), &pm) == nil {
			syncMeta.PRSnapshotUTC = pm["pr_snapshot_utc"]
			syncMeta.PRSnapshotUTC8 = pm["pr_snapshot_utc8"]
		}
	}

	linksByIssue := make(map[int][]model.LinkedPRRef)
	lRows, err := s.db.Query(`
SELECT issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at, source
FROM issue_pr_links
WHERE issue_repo = ?`, DefaultRepo)
	if err != nil {
		return model.Dataset{}, err
	}
	defer lRows.Close()
	for lRows.Next() {
		var issNum, isCP int
		var p model.LinkedPRRef
		if err := lRows.Scan(&issNum, &p.Repo, &p.Num, &p.Label, &p.URL, &p.State, &p.Title, &isCP, &p.UpdatedAt, &p.Source); err != nil {
			return model.Dataset{}, err
		}
		p.IsCherryPick = isCP != 0
		linksByIssue[issNum] = append(linksByIssue[issNum], p)
	}
	if err := lRows.Err(); err != nil {
		return model.Dataset{}, err
	}

	rows, err := s.db.Query(`
SELECT
  i.number, i.title, i.author, i.assignees_json, i.created, i.created_at, i.updated_at,
  i.labels_json, i.is_canonical, i.is_customer, i.gh_severity,
  i.auto_cat, i.auto_severity, i.auto_subsystem, i.auto_is_borderline, i.auto_force_keep, i.auto_note_zh, i.auto_note_en,
  o.cat, o.severity, o.subsystem, o.is_borderline_kept, COALESCE(o.force_keep, 0), COALESCE(o.has_force_keep_override, 0),
  o.note_zh, o.note_en, COALESCE(o.has_note_override, 0),
  CASE WHEN o.number IS NOT NULL THEN 1 ELSE 0 END,
  COALESCE(o.updated_at, ''),
  COALESCE(ts.triaged_at, ''), COALESCE(ts.needs_retriage, 0),
  COALESCE(ts.retriage_reasons_json, '[]'), COALESCE(ts.retriage_details_json, '[]')
FROM issues i
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
LEFT JOIN issue_triage_state ts ON i.repo = ts.repo AND i.number = ts.number
WHERE i.repo = ?
ORDER BY i.number DESC`, DefaultRepo)
	if err != nil {
		return model.Dataset{}, err
	}
	defer rows.Close()

	records := make([]model.IssueRecord, 0, 600)
	maxUpdated := ""
	canCount := 0
	realBugs := 0
	boardEligibleOpenPRKeys := make(map[string]bool)
	var retriageNums []int

	for rows.Next() {
		var (
			r                        model.IssueRecord
			asgJSON, lbsJSON         string
			isCan, isCust            int
			autoCat, autoSev         string
			autoSub                  string
			autoBdr, autoFK          int
			autoNZh, autoNEn         string
			oCat, oSev, oSub         sql.NullString
			oBdr                     sql.NullInt64
			oForceKeep, oHasFK       int
			oNZh, oNEn               sql.NullString
			oHasNote, hasOvr         int
			ovrUpdatedAt, triagedAt  string
			needsRetriage            int
			reasonsJSON, detailsJSON string
		)
		if err := rows.Scan(
			&r.Number, &r.Title, &r.Author, &asgJSON, &r.Created, &r.CreatedAt, &r.UpdatedAt,
			&lbsJSON, &isCan, &isCust, &r.GHSeverity,
			&autoCat, &autoSev, &autoSub, &autoBdr, &autoFK, &autoNZh, &autoNEn,
			&oCat, &oSev, &oSub, &oBdr, &oForceKeep, &oHasFK,
			&oNZh, &oNEn, &oHasNote, &hasOvr,
			&ovrUpdatedAt, &triagedAt, &needsRetriage, &reasonsJSON, &detailsJSON,
		); err != nil {
			return model.Dataset{}, err
		}

		_ = json.Unmarshal([]byte(asgJSON), &r.Assignees)
		if r.Assignees == nil {
			r.Assignees = []string{}
		}
		_ = json.Unmarshal([]byte(lbsJSON), &r.Labels)
		if r.Labels == nil {
			r.Labels = []string{}
		}
		r.IsCanonical = isCan != 0
		r.IsCustomer = isCust != 0
		r.UpdatedDisplay = prstatus.ToUTC8Display(r.UpdatedAt)

		r.Cat = autoCat
		if oCat.Valid && oCat.String != "" {
			r.Cat = oCat.String
		}
		r.Severity = autoSev
		if oSev.Valid && oSev.String != "" {
			r.Severity = oSev.String
		}
		r.Subsystem = autoSub
		if oSub.Valid && oSub.String != "" {
			r.Subsystem = oSub.String
		}
		r.IsBorderlineKept = autoBdr != 0
		if oBdr.Valid {
			r.IsBorderlineKept = oBdr.Int64 != 0
		}
		if !model.IsRealBugCategory(r.Cat) {
			r.IsBorderlineKept = false
		}
		r.DiffRank = model.DiffRank[r.Cat]
		r.SevRank = model.SevRank[r.Severity]

		nZh, nEn := autoNZh, autoNEn
		if oHasNote != 0 {
			nZh = oNZh.String
			nEn = oNEn.String
		}
		if nZh != "" || nEn != "" {
			r.Note = &model.LocalizedNote{ZhCN: nZh, En: nEn}
		}

		prs := linksByIssue[r.Number]
		if prs == nil {
			prs = []model.LinkedPRRef{}
		} else {
			sortLinkedPRs(prs)
		}
		r.PRs = prs
		r.HasPR = len(prs) > 0
		for _, p := range prs {
			if p.State == "OPEN" || p.State == "DRAFT" {
				r.HasAnyOpenPR = true
				if r.Cat != "FILTER_NON_OPT_OR_FIXED" {
					boardEligibleOpenPRKeys[model.PRKey(p.Repo, p.Num)] = true
				}
				if !p.IsCherryPick {
					r.HasOpenPR = true
				}
			}
			if p.State == "MERGED" {
				r.HasMergedPR = true
			}
		}

		if needsRetriage != 0 {
			retriageNums = append(retriageNums, r.Number)
		}

		if includeWorkbenchFlags {
			r.ManualOverride = hasOvr != 0
			effForceKeep := autoFK != 0
			if oHasFK != 0 {
				effForceKeep = oForceKeep != 0
			}
			r.ForceKeep = effForceKeep
			r.TriagedAt = model.MaxRFC3339(ovrUpdatedAt, triagedAt)
			if needsRetriage != 0 {
				r.NeedsRetriage = true
				_ = json.Unmarshal([]byte(reasonsJSON), &r.RetriageReasons)
				_ = json.Unmarshal([]byte(detailsJSON), &r.RetriageDetails)
			}
		}

		maxUpdated = model.MaxRFC3339(maxUpdated, r.UpdatedAt)
		if r.IsCanonical {
			canCount++
		}
		if model.IsRealBugCategory(r.Cat) {
			realBugs++
		}

		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return model.Dataset{}, err
	}

	prMap := make(map[string]model.PRStatus)
	openPRCnt := 0
	mainlineOpenPRCnt := 0
	pRows, err := s.db.Query(`SELECT status_json FROM pull_requests WHERE state IN ('OPEN', 'DRAFT')`)
	if err != nil {
		return model.Dataset{}, err
	}
	defer pRows.Close()
	for pRows.Next() {
		var raw string
		if err := pRows.Scan(&raw); err != nil {
			return model.Dataset{}, err
		}
		var st model.PRStatus
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			continue
		}
		key := model.PRKey(st.Repo, st.Num)
		hasStandaloneSource := false
		for _, src := range st.TrackSources {
			if src != "" && src != "issue_linked" {
				hasStandaloneSource = true
				break
			}
		}
		if !boardEligibleOpenPRKeys[key] && !hasStandaloneSource {
			continue
		}
		prMap[key] = st
		openPRCnt++
		if !st.IsCherryPick {
			mainlineOpenPRCnt++
		}
	}

	var exNums, pendNums []int
	if includeWorkbenchFlags {
		exRows, err := s.db.Query(`SELECT number FROM excluded_issues WHERE repo = ? ORDER BY number DESC`, DefaultRepo)
		if err == nil {
			defer exRows.Close()
			for exRows.Next() {
				var n int
				if exRows.Scan(&n) == nil {
					exNums = append(exNums, n)
				}
			}
		}

		pendRows, err := s.db.Query(`SELECT number FROM pending_candidates WHERE repo = ? ORDER BY number DESC`, DefaultRepo)
		if err == nil {
			defer pendRows.Close()
			for pendRows.Next() {
				var n int
				if pendRows.Scan(&n) == nil {
					pendNums = append(pendNums, n)
				}
			}
		}
	} else {
		retriageNums = nil
	}

	syncMeta.SchemaVersion = model.SchemaVersion
	syncMeta.SupportedLocales = []string{"zh-CN", "en"}
	if model.CompareRFC3339(maxUpdated, syncMeta.MaxIssueUpdatedUTC) > 0 {
		syncMeta.MaxIssueUpdatedUTC = model.NormalizeRFC3339UTC(maxUpdated)
	}
	if syncMeta.MaxIssueUpdatedUTC != "" {
		syncMeta.MaxIssueUpdatedUTC8 = prstatus.ToUTC8Display(syncMeta.MaxIssueUpdatedUTC) + " (UTC+8)"
		syncMeta.IncrementalGHSearchQualifier = "updated:>=" + syncMeta.MaxIssueUpdatedUTC
	}
	syncMeta.TotalCandidates = len(records)
	syncMeta.CanonicalCandidates = canCount
	syncMeta.RealBugs = realBugs
	syncMeta.OpenPRCount = openPRCnt
	syncMeta.MainlineOpenPRCount = mainlineOpenPRCnt
	syncMeta.ExcludedIssueNums = exNums
	syncMeta.PendingCandidateNums = pendNums
	syncMeta.RetriageIssueNums = retriageNums

	return model.Dataset{
		SyncMeta: syncMeta,
		Enums: model.DatasetEnums{
			Categories:    model.ValidCategories,
			Severities:    model.ValidSeverities,
			Subsystems:    model.ValidSubsystems,
			BlockerStages: model.ValidBlockerStages,
			BallOwners:    model.ValidBallOwners,
		},
		Records:  records,
		PRStatus: prMap,
	}, nil
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
