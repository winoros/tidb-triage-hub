package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

const DefaultRepo = "pingcap/tidb"

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
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
  PRIMARY KEY (issue_repo, issue_num, pr_repo, pr_num)
);

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
  PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS excluded_issues (
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  reason TEXT NOT NULL,
  excluded_at TEXT NOT NULL,
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
	_, err := s.db.Exec(schema)
	return err
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
	s.mu.Lock()
	defer s.mu.Unlock()

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&cnt); err != nil {
		return false, err
	}
	if cnt > 0 {
		return false, nil
	}
	raw, err := os.ReadFile(dataJSONPath)
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

	if _, err := tx.Exec(`DELETE FROM issues WHERE repo = ?`, DefaultRepo); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM issue_pr_links WHERE issue_repo = ?`, DefaultRepo); err != nil {
		return err
	}

	insIssue, err := tx.Prepare(`
INSERT INTO issues(
  repo, number, title, author, assignees_json, created, created_at, updated_at,
  labels_json, is_canonical, is_customer, gh_severity,
  auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_note_zh, auto_note_en
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insIssue.Close()

	insLink, err := tx.Prepare(`
INSERT INTO issue_pr_links(
  issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET
  label=excluded.label, url=excluded.url, state=excluded.state,
  title=excluded.title, is_cherry_pick=excluded.is_cherry_pick, updated_at=excluded.updated_at`)
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
			r.Cat, r.Severity, r.Subsystem, boolToInt(r.IsBorderlineKept), nZh, nEn,
		); err != nil {
			return err
		}
		for _, p := range r.PRs {
			if _, err := insLink.Exec(
				DefaultRepo, r.Number, p.Repo, p.Num, p.Label, p.URL, p.State, p.Title, boolToInt(p.IsCherryPick), p.UpdatedAt,
			); err != nil {
				return err
			}
		}
	}

	nowISO := time.Now().UTC().Format(time.RFC3339)
	for _, exNum := range ds.SyncMeta.ExcludedIssueNums {
		if _, err := tx.Exec(`
INSERT INTO excluded_issues(repo, number, reason, excluded_at)
VALUES (?, ?, 'Excluded from optimizer candidate scope', ?)
ON CONFLICT(repo, number) DO NOTHING`, DefaultRepo, exNum, nowISO); err != nil {
			return err
		}
	}

	// If a previously excluded issue was re-admitted into records (e.g. gained sig/planner), remove from excluded_issues
	if len(ds.Records) > 0 {
		for _, r := range ds.Records {
			if _, err := tx.Exec(`DELETE FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, r.Number); err != nil {
				return err
			}
		}
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

	return tx.Commit()
}

func (s *Store) ListOpenPRTargets() ([]prstatus.PRTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
SELECT DISTINCT pr_repo, pr_num
FROM issue_pr_links
WHERE state IN ('OPEN', 'DRAFT')
ORDER BY pr_repo ASC, pr_num DESC`)
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

	stmtLink, err := tx.Prepare(`
UPDATE issue_pr_links
SET state = ?, title = ?, is_cherry_pick = ?, updated_at = ?
WHERE pr_repo = ? AND pr_num = ?`)
	if err != nil {
		return err
	}
	defer stmtLink.Close()

	for _, st := range statuses {
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if _, err := stmtPR.Exec(st.Repo, st.Num, st.State, boolToInt(st.IsCherryPick), st.UpdatedAt, string(b)); err != nil {
			return err
		}
		if _, err := stmtLink.Exec(st.State, st.Title, boolToInt(st.IsCherryPick), st.UpdatedAt, st.Repo, st.Num); err != nil {
			return err
		}
	}

	isoUTC := syncTime.UTC().Format("2006-01-02T15:04:05Z")
	prMeta, _ := json.Marshal(map[string]string{
		"pr_snapshot_utc":  isoUTC,
		"pr_snapshot_utc8": prstatus.ToUTC8Display(isoUTC) + " (UTC+8)",
	})
	if _, err := tx.Exec(`INSERT INTO kv_meta(key, val_json) VALUES('pr_sync_meta', ?) ON CONFLICT(key) DO UPDATE SET val_json=excluded.val_json`, string(prMeta)); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) SaveIssueOverride(number int, req model.TriagePatchRequest) (model.IssueRecord, error) {
	if err := model.ValidateTriageFields(req.Cat, req.Severity, req.Subsystem); err != nil {
		return model.IssueRecord{}, err
	}
	// Enforce N/A severity consistency on non-bug categories if user switched to filtered/enhancement
	isRealBug := req.Cat == "EASY" || req.Cat == "MEDIUM" || req.Cat == "HARD"
	if !isRealBug && req.Severity != "N/A" {
		req.Severity = "N/A"
	}
	if isRealBug && req.Severity == "N/A" {
		req.Severity = "MODERATE"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&exists); err != nil {
		return model.IssueRecord{}, err
	}
	if exists == 0 {
		return model.IssueRecord{}, fmt.Errorf("issue #%d not found", number)
	}

	nZh, nEn := "", ""
	if req.Note != nil {
		nZh = strings.TrimSpace(req.Note.ZhCN)
		nEn = strings.TrimSpace(req.Note.En)
	}
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	_, err := s.db.Exec(`
INSERT INTO issue_overrides(
  repo, number, cat, severity, subsystem, is_borderline_kept, force_keep,
  note_zh, note_en, has_note_override, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  cat=excluded.cat,
  severity=excluded.severity,
  subsystem=excluded.subsystem,
  is_borderline_kept=excluded.is_borderline_kept,
  force_keep=excluded.force_keep,
  note_zh=excluded.note_zh,
  note_en=excluded.note_en,
  has_note_override=1,
  updated_at=excluded.updated_at`,
		DefaultRepo, number, req.Cat, req.Severity, req.Subsystem,
		boolToInt(req.IsBorderlineKept), boolToInt(req.ForceKeep),
		nZh, nEn, nowISO,
	)
	if err != nil {
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
	return model.IssueRecord{}, fmt.Errorf("issue #%d missing after update", number)
}

func (s *Store) DeleteIssueOverride(number int) (model.IssueRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(`DELETE FROM issue_overrides WHERE repo = ? AND number = ? AND is_new_decision = 0`, DefaultRepo, number); err != nil {
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
	return model.IssueRecord{}, fmt.Errorf("issue #%d not found", number)
}

func (s *Store) ListPendingCandidates() ([]model.PendingCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT payload_json FROM pending_candidates WHERE repo = ? ORDER BY number DESC`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.PendingCandidate, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var pc model.PendingCandidate
		if err := json.Unmarshal([]byte(raw), &pc); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}

func (s *Store) ResolvePendingCandidate(number int, req model.PendingResolveRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var raw string
	err := s.db.QueryRow(`SELECT payload_json FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&raw)
	if err == sql.ErrNoRows {
		return fmt.Errorf("pending candidate #%d not found", number)
	}
	if err != nil {
		return err
	}
	var pc model.PendingCandidate
	if err := json.Unmarshal([]byte(raw), &pc); err != nil {
		return err
	}

	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	if req.Action == "exclude" {
		reason := strings.TrimSpace(req.ExcludeReason)
		if reason == "" {
			reason = "Excluded non-optimizer issue via workbench"
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`
INSERT INTO excluded_issues(repo, number, reason, excluded_at) VALUES (?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET reason=excluded.reason, excluded_at=excluded.excluded_at`,
			DefaultRepo, number, reason, nowISO); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number); err != nil {
			return err
		}
		return tx.Commit()
	}

	if req.Action != "accept" {
		return fmt.Errorf("invalid action %q", req.Action)
	}
	isRealBug := req.Cat == "EASY" || req.Cat == "MEDIUM" || req.Cat == "HARD"
	if !isRealBug && req.Severity != "N/A" {
		req.Severity = "N/A"
	}
	if isRealBug && (req.Severity == "" || req.Severity == "N/A") {
		req.Severity = "MAJOR"
	}
	if err := model.ValidateTriageFields(req.Cat, req.Severity, req.Subsystem); err != nil {
		return err
	}

	ghSev := "unlabeled"
	isCan := false
	isCust := false
	hasPlannerSig := false
	hasBugType := false
	for _, lb := range pc.Labels {
		if strings.HasPrefix(lb, "severity/") && ghSev == "unlabeled" {
			ghSev = strings.SplitN(lb, "/", 2)[1]
		}
		if lb == "sig/planner" {
			hasPlannerSig = true
		}
		if lb == "type/bug" {
			hasBugType = true
		}
		if lb == "report/customer" {
			isCust = true
		}
	}
	isCan = hasPlannerSig && hasBugType

	nZh, nEn := "", ""
	if req.Note != nil {
		nZh = strings.TrimSpace(req.Note.ZhCN)
		nEn = strings.TrimSpace(req.Note.En)
	}
	lbsJSON, _ := json.Marshal(nonNilSlice(pc.Labels))
	created := pc.CreatedAt
	if len(created) >= 10 {
		created = created[:10]
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
INSERT INTO issues(
  repo, number, title, author, assignees_json, created, created_at, updated_at,
  labels_json, is_canonical, is_customer, gh_severity,
  auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_note_zh, auto_note_en
) VALUES (?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title, updated_at=excluded.updated_at, labels_json=excluded.labels_json`,
		DefaultRepo, pc.Number, pc.Title, pc.Author, created, pc.CreatedAt, pc.UpdatedAt,
		string(lbsJSON), boolToInt(isCan), boolToInt(isCust), ghSev,
		req.Cat, req.Severity, req.Subsystem, boolToInt(req.IsBorderlineKept), nZh, nEn,
	); err != nil {
		return err
	}

	for _, p := range pc.PRs {
		if _, err := tx.Exec(`
INSERT INTO issue_pr_links(issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET state=excluded.state, title=excluded.title, updated_at=excluded.updated_at`,
			DefaultRepo, pc.Number, p.Repo, p.Num, p.Label, p.URL, p.State, p.Title, boolToInt(p.IsCherryPick), p.UpdatedAt,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`
INSERT INTO issue_overrides(
  repo, number, cat, severity, subsystem, is_borderline_kept, force_keep,
  note_zh, note_en, has_note_override, is_new_decision, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  cat=excluded.cat, severity=excluded.severity, subsystem=excluded.subsystem,
  is_borderline_kept=excluded.is_borderline_kept, force_keep=excluded.force_keep,
  note_zh=excluded.note_zh, note_en=excluded.note_en, has_note_override=1, is_new_decision=1, updated_at=excluded.updated_at`,
		DefaultRepo, pc.Number, req.Cat, req.Severity, req.Subsystem,
		boolToInt(req.IsBorderlineKept), boolToInt(req.ForceKeep), nZh, nEn, nowISO,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) ExportDecisionsForSync() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	newIssues := make(map[string]any)
	overrides := make(map[string]any)
	excludeIssues := make(map[string]string)

	rows, err := s.db.Query(`
SELECT number, cat, severity, subsystem, is_borderline_kept, force_keep, note_zh, note_en, has_note_override, is_new_decision
FROM issue_overrides
WHERE repo = ?`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			num           int
			cat, sev, sub sql.NullString
			bdr           sql.NullInt64
			forceKeep     int
			nZh, nEn      sql.NullString
			hasNote       int
			isNew         int
		)
		if err := rows.Scan(&num, &cat, &sev, &sub, &bdr, &forceKeep, &nZh, &nEn, &hasNote, &isNew); err != nil {
			return nil, err
		}
		entry := make(map[string]any)
		if cat.Valid {
			entry["cat"] = cat.String
		}
		if sev.Valid {
			entry["severity"] = sev.String
		}
		if sub.Valid {
			entry["subsystem"] = sub.String
		}
		if bdr.Valid {
			entry["is_borderline_kept"] = bdr.Int64 != 0
		}
		if forceKeep != 0 {
			entry["force_keep"] = true
		}
		if hasNote != 0 {
			zh := strings.TrimSpace(nZh.String)
			en := strings.TrimSpace(nEn.String)
			if zh == "" && en == "" {
				entry["note"] = nil
			} else {
				if zh == "" {
					zh = en
				}
				if en == "" {
					en = zh
				}
				entry["note"] = map[string]string{"zh-CN": zh, "en": en}
			}
		}
		key := fmt.Sprintf("%d", num)
		overrides[key] = entry
		if isNew != 0 {
			newIssues[key] = entry
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	exRows, err := s.db.Query(`SELECT number, reason FROM excluded_issues WHERE repo = ?`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer exRows.Close()
	for exRows.Next() {
		var num int
		var reason string
		if err := exRows.Scan(&num, &reason); err != nil {
			return nil, err
		}
		excludeIssues[fmt.Sprintf("%d", num)] = reason
	}

	return map[string]any{
		"new_issues":     newIssues,
		"exclude_issues": excludeIssues,
		"overrides":      overrides,
	}, exRows.Err()
}

func (s *Store) RecordSyncRun(kind string, started, finished time.Time, summary any) {
	b, _ := json.Marshal(summary)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO sync_runs(kind, started_at, finished_at, summary_json) VALUES (?, ?, ?, ?)`,
		kind, started.UTC().Format(time.RFC3339), finished.UTC().Format(time.RFC3339), string(b))
}

func (s *Store) BuildDataset(includeWorkbenchFlags bool) (model.Dataset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildDatasetLocked(includeWorkbenchFlags)
}

func sortLinkedPRs(prs []model.LinkedPRRef) {
	stRank := func(p model.LinkedPRRef) int {
		switch {
		case p.State == "OPEN" && !p.IsCherryPick:
			return 0
		case p.State == "DRAFT" && !p.IsCherryPick:
			return 1
		case p.State == "MERGED" && !p.IsCherryPick:
			return 2
		case p.State == "OPEN" && p.IsCherryPick:
			return 3
		case p.State == "DRAFT" && p.IsCherryPick:
			return 4
		case p.State == "MERGED" && p.IsCherryPick:
			return 5
		case p.State == "CLOSED" && !p.IsCherryPick:
			return 6
		case p.State == "CLOSED" && p.IsCherryPick:
			return 7
		default:
			return 8
		}
	}
	sort.Slice(prs, func(i, j int) bool {
		ri, rj := stRank(prs[i]), stRank(prs[j])
		if ri != rj {
			return ri < rj
		}
		return prs[i].Num > prs[j].Num
	})
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
SELECT issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at
FROM issue_pr_links
WHERE issue_repo = ?`, DefaultRepo)
	if err != nil {
		return model.Dataset{}, err
	}
	defer lRows.Close()
	for lRows.Next() {
		var issNum, isCP int
		var p model.LinkedPRRef
		if err := lRows.Scan(&issNum, &p.Repo, &p.Num, &p.Label, &p.URL, &p.State, &p.Title, &isCP, &p.UpdatedAt); err != nil {
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
  i.auto_cat, i.auto_severity, i.auto_subsystem, i.auto_is_borderline, i.auto_note_zh, i.auto_note_en,
  o.cat, o.severity, o.subsystem, o.is_borderline_kept, COALESCE(o.force_keep, 0),
  o.note_zh, o.note_en, COALESCE(o.has_note_override, 0), CASE WHEN o.number IS NOT NULL THEN 1 ELSE 0 END
FROM issues i
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
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
	activeOpenPRKeys := make(map[string]bool)

	for rows.Next() {
		var (
			r                model.IssueRecord
			asgJSON, lbsJSON string
			isCan, isCust    int
			autoCat, autoSev string
			autoSub          string
			autoBdr          int
			autoNZh, autoNEn string
			oCat, oSev, oSub sql.NullString
			oBdr             sql.NullInt64
			oForceKeep       int
			oNZh, oNEn       sql.NullString
			oHasNote, hasOvr int
		)
		if err := rows.Scan(
			&r.Number, &r.Title, &r.Author, &asgJSON, &r.Created, &r.CreatedAt, &r.UpdatedAt,
			&lbsJSON, &isCan, &isCust, &r.GHSeverity,
			&autoCat, &autoSev, &autoSub, &autoBdr, &autoNZh, &autoNEn,
			&oCat, &oSev, &oSub, &oBdr, &oForceKeep,
			&oNZh, &oNEn, &oHasNote, &hasOvr,
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
		r.DiffRank = model.DiffRank[r.Cat]
		r.SevRank = model.SevRank[r.Severity]

		nZh, nEn := autoNZh, autoNEn
		if oHasNote != 0 {
			nZh = oNZh.String
			nEn = oNEn.String
		}
		if nZh != "" || nEn != "" {
			if nZh == "" {
				nZh = nEn
			}
			if nEn == "" {
				nEn = nZh
			}
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
				activeOpenPRKeys[model.PRKey(p.Repo, p.Num)] = true
				if !p.IsCherryPick {
					r.HasOpenPR = true
				}
			}
			if p.State == "MERGED" {
				r.HasMergedPR = true
			}
		}

		if includeWorkbenchFlags {
			r.ManualOverride = hasOvr != 0
			r.ForceKeep = oForceKeep != 0
		}

		if r.UpdatedAt > maxUpdated {
			maxUpdated = r.UpdatedAt
		}
		if r.IsCanonical {
			canCount++
		}
		if r.Cat == "EASY" || r.Cat == "MEDIUM" || r.Cat == "HARD" {
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
		// Include if linked to an active open issue OR tracked via non-issue source
		hasStandaloneSource := false
		for _, src := range st.TrackSources {
			if src != "issue_linked" {
				hasStandaloneSource = true
				break
			}
		}
		if !activeOpenPRKeys[key] && !hasStandaloneSource {
			continue
		}
		prMap[key] = st
		openPRCnt++
		if !st.IsCherryPick {
			mainlineOpenPRCnt++
		}
	}

	var exNums []int
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

	var pendNums []int
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

	syncMeta.SchemaVersion = model.SchemaVersion
	syncMeta.SupportedLocales = []string{"zh-CN", "en"}
	if maxUpdated != "" {
		syncMeta.MaxIssueUpdatedUTC = maxUpdated
		syncMeta.MaxIssueUpdatedUTC8 = prstatus.ToUTC8Display(maxUpdated) + " (UTC+8)"
		syncMeta.IncrementalGHSearchQualifier = "updated:>=" + maxUpdated
	}
	syncMeta.TotalCandidates = len(records)
	syncMeta.CanonicalCandidates = canCount
	syncMeta.RealBugs = realBugs
	syncMeta.OpenPRCount = openPRCnt
	syncMeta.MainlineOpenPRCount = mainlineOpenPRCnt
	syncMeta.ExcludedIssueNums = exNums
	syncMeta.PendingCandidateNums = pendNums

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
