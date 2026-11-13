package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

// LoadSyncState snapshots the lightweight in-memory state needed by issuesync.Engine.PlanSync
// so that no SQLite locks are held while calling GitHub GraphQL.
func (s *Store) LoadSyncState() (issuesync.SyncState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var prevMeta model.SyncMeta
	var rawMeta string
	err := s.db.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'sync_meta'`).Scan(&rawMeta)
	if err != nil && err != sql.ErrNoRows {
		return issuesync.SyncState{}, err
	}
	if err == nil && rawMeta != "" {
		if err := json.Unmarshal([]byte(rawMeta), &prevMeta); err != nil {
			return issuesync.SyncState{}, fmt.Errorf("unmarshal sync_meta: %w", err)
		}
	}

	prsByIssue := make(map[int][]issuesync.TrackedPRLink)
	lRows, err := s.db.Query(`
SELECT issue_num, pr_repo, pr_num, state, is_cherry_pick, source
FROM issue_pr_links
WHERE issue_repo = ?`, DefaultRepo)
	if err != nil {
		return issuesync.SyncState{}, err
	}
	for lRows.Next() {
		var issNum, isCP int
		var pl issuesync.TrackedPRLink
		if err := lRows.Scan(&issNum, &pl.Repo, &pl.Num, &pl.State, &isCP, &pl.Source); err != nil {
			_ = lRows.Close()
			return issuesync.SyncState{}, err
		}
		pl.IsCherryPick = isCP != 0
		prsByIssue[issNum] = append(prsByIssue[issNum], pl)
	}
	if err := lRows.Err(); err != nil {
		_ = lRows.Close()
		return issuesync.SyncState{}, err
	}
	_ = lRows.Close()

	tracked := make(map[int]issuesync.TrackedIssueState)
	iRows, err := s.db.Query(`
SELECT
  i.number, i.title, i.updated_at, i.labels_json, i.is_canonical,
  COALESCE(NULLIF(o.cat, ''), i.auto_cat),
  COALESCE(NULLIF(o.severity, ''), i.auto_severity),
  CASE
    WHEN COALESCE(o.has_force_keep_override, 0) != 0 THEN COALESCE(o.force_keep, 0)
    ELSE i.auto_force_keep
  END
FROM issues i
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
WHERE i.repo = ?`, DefaultRepo)
	if err != nil {
		return issuesync.SyncState{}, err
	}
	for iRows.Next() {
		var (
			tr        issuesync.TrackedIssueState
			lbsJSON   string
			isCan, fk int
		)
		if err := iRows.Scan(&tr.Number, &tr.Title, &tr.UpdatedAt, &lbsJSON, &isCan, &tr.Cat, &tr.Severity, &fk); err != nil {
			_ = iRows.Close()
			return issuesync.SyncState{}, err
		}
		_ = json.Unmarshal([]byte(lbsJSON), &tr.Labels)
		tr.IsCanonical = isCan != 0
		tr.ForceKeep = fk != 0
		tr.PRs = prsByIssue[tr.Number]
		tracked[tr.Number] = tr
	}
	if err := iRows.Err(); err != nil {
		_ = iRows.Close()
		return issuesync.SyncState{}, err
	}
	_ = iRows.Close()

	archived := make(map[int]issuesync.ArchivedIssueState)
	aRows, err := s.db.Query(`
SELECT
  a.number,
  COALESCE(NULLIF(o.cat, ''), a.auto_cat),
  CASE
    WHEN COALESCE(o.has_force_keep_override, 0) != 0 THEN COALESCE(o.force_keep, 0)
    ELSE a.auto_force_keep
  END
FROM archived_issues a
LEFT JOIN issue_overrides o ON a.repo = o.repo AND a.number = o.number
WHERE a.repo = ?`, DefaultRepo)
	if err != nil {
		return issuesync.SyncState{}, err
	}
	for aRows.Next() {
		var n, fk int
		var cat string
		if err := aRows.Scan(&n, &cat, &fk); err != nil {
			_ = aRows.Close()
			return issuesync.SyncState{}, err
		}
		archived[n] = issuesync.ArchivedIssueState{Number: n, Cat: cat, ForceKeep: fk != 0}
	}
	if err := aRows.Err(); err != nil {
		_ = aRows.Close()
		return issuesync.SyncState{}, err
	}
	_ = aRows.Close()

	pending := make(map[int][]string)
	pendingCands := make(map[int]model.PendingCandidate)
	pRows, err := s.db.Query(`SELECT number, payload_json FROM pending_candidates WHERE repo = ?`, DefaultRepo)
	if err != nil {
		return issuesync.SyncState{}, err
	}
	for pRows.Next() {
		var n int
		var raw string
		if err := pRows.Scan(&n, &raw); err != nil {
			_ = pRows.Close()
			return issuesync.SyncState{}, err
		}
		var pc model.PendingCandidate
		if err := json.Unmarshal([]byte(raw), &pc); err == nil {
			pending[n] = pc.MatchedScopes
			pendingCands[n] = pc
		} else {
			pending[n] = []string{}
		}
	}
	if err := pRows.Err(); err != nil {
		_ = pRows.Close()
		return issuesync.SyncState{}, err
	}
	_ = pRows.Close()

	excluded := make(map[int][]string)
	excludedStates := make(map[int]issuesync.ExcludedIssueState)
	eRows, err := s.db.Query(`SELECT number, excluded_labels_json, source FROM excluded_issues WHERE repo = ?`, DefaultRepo)
	if err != nil {
		return issuesync.SyncState{}, err
	}
	for eRows.Next() {
		var n int
		var lbsJSON, src string
		if err := eRows.Scan(&n, &lbsJSON, &src); err != nil {
			_ = eRows.Close()
			return issuesync.SyncState{}, err
		}
		var lbs []string
		_ = json.Unmarshal([]byte(lbsJSON), &lbs)
		excluded[n] = lbs
		labelsKnown := src != "seed_removed" && src != "manual_untracked" &&
			(len(lbs) > 0 || src == "pending_triage" || src == "manual_exclude")
		excludedStates[n] = issuesync.ExcludedIssueState{
			Labels:      lbs,
			LabelsKnown: labelsKnown,
		}
	}
	if err := eRows.Err(); err != nil {
		_ = eRows.Close()
		return issuesync.SyncState{}, err
	}
	_ = eRows.Close()

	recheckSet, err := loadRecheckNums(s.db)
	if err != nil {
		return issuesync.SyncState{}, err
	}
	var recheckNums []int
	for n := range recheckSet {
		recheckNums = append(recheckNums, n)
	}
	sort.Ints(recheckNums)

	return issuesync.SyncState{
		PrevMeta:          prevMeta,
		TrackedIssues:     tracked,
		ArchivedIssues:    archived,
		PendingScopes:     pending,
		PendingCandidates: pendingCands,
		ExcludedLabels:    excluded,
		ExcludedIssues:    excludedStates,
		RecheckNums:       recheckNums,
	}, nil
}

func loadRecheckNums(q sqlExecQuerier) (map[int]bool, error) {
	var raw string
	err := q.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'restored_recheck_nums'`).Scan(&raw)
	if err == sql.ErrNoRows || raw == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nums []int
	if err := json.Unmarshal([]byte(raw), &nums); err != nil {
		return nil, err
	}
	if len(nums) == 0 {
		return nil, nil
	}
	out := make(map[int]bool, len(nums))
	for _, n := range nums {
		if n > 0 {
			out[n] = true
		}
	}
	return out, nil
}

func saveRecheckNumsInTx(tx *sql.Tx, set map[int]bool) error {
	if len(set) == 0 {
		_, err := tx.Exec(`DELETE FROM kv_meta WHERE key = 'restored_recheck_nums'`)
		return err
	}
	nums := make([]int, 0, len(set))
	for n := range set {
		if n > 0 {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	b, err := json.Marshal(nums)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
INSERT INTO kv_meta(key, val_json) VALUES('restored_recheck_nums', ?)
ON CONFLICT(key) DO UPDATE SET val_json = excluded.val_json`, string(b))
	return err
}

func addRecheckNumInTx(tx *sql.Tx, num int) error {
	if num <= 0 {
		return nil
	}
	set, err := loadRecheckNums(tx)
	if err != nil {
		return err
	}
	if set == nil {
		set = make(map[int]bool)
	}
	set[num] = true
	return saveRecheckNumsInTx(tx, set)
}

func removeRecheckNumInTx(tx *sql.Tx, nums ...int) error {
	if len(nums) == 0 {
		return nil
	}
	set, err := loadRecheckNums(tx)
	if err != nil || len(set) == 0 {
		return err
	}
	changed := false
	for _, n := range nums {
		if set[n] {
			delete(set, n)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return saveRecheckNumsInTx(tx, set)
}

func syncClosedPRStatusInTx(tx *sql.Tx, repo string, num int, state string) error {
	if state != "MERGED" && state != "CLOSED" {
		return nil
	}
	_, err := tx.Exec(`UPDATE pull_requests SET state = ? WHERE repo = ? AND number = ? AND state IN ('OPEN', 'DRAFT')`,
		state, repo, num)
	return err
}

func reconcileIssueTimelineLinksInTx(
	tx *sql.Tx,
	issueNum int,
	timelinePRs []model.LinkedPRRef,
	timelineTruncated bool,
	prRefPRs []model.LinkedPRRef,
	unlinkedPRRefs []string,
) error {
	timelineKeys := make(map[string]bool, len(timelinePRs))
	for _, p := range timelinePRs {
		timelineKeys[model.PRKey(p.Repo, p.Num)] = true
		if _, err := tx.Exec(`
INSERT INTO issue_pr_links(
  issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at, source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'timeline')
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET
  label=excluded.label,
  url=excluded.url,
  state=excluded.state,
  title=excluded.title,
  is_cherry_pick=excluded.is_cherry_pick,
  updated_at=CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE issue_pr_links.updated_at END,
  source=CASE WHEN issue_pr_links.source = 'manual' THEN 'manual' ELSE 'timeline' END`,
			DefaultRepo, issueNum, p.Repo, p.Num, p.Label, p.URL, p.State, p.Title, boolToInt(p.IsCherryPick), p.UpdatedAt,
		); err != nil {
			return err
		}
		if err := syncClosedPRStatusInTx(tx, p.Repo, p.Num, p.State); err != nil {
			return err
		}
	}

	for _, p := range prRefPRs {
		key := model.PRKey(p.Repo, p.Num)
		if timelineKeys[key] {
			continue
		}
		if _, err := tx.Exec(`
INSERT INTO issue_pr_links(
  issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at, source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pr_ref')
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET
  label=excluded.label,
  url=excluded.url,
  state=excluded.state,
  title=excluded.title,
  is_cherry_pick=excluded.is_cherry_pick,
  updated_at=CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE issue_pr_links.updated_at END,
  source=CASE WHEN issue_pr_links.source IN ('manual', 'timeline') THEN issue_pr_links.source ELSE 'pr_ref' END`,
			DefaultRepo, issueNum, p.Repo, p.Num, p.Label, p.URL, p.State, p.Title, boolToInt(p.IsCherryPick), p.UpdatedAt,
		); err != nil {
			return err
		}
		if err := syncClosedPRStatusInTx(tx, p.Repo, p.Num, p.State); err != nil {
			return err
		}
	}

	unlinkedSet := make(map[string]bool, len(unlinkedPRRefs))
	for _, k := range unlinkedPRRefs {
		unlinkedSet[k] = true
	}

	// Remove timeline-sourced links that were unlinked on GitHub (unless timeline was truncated at 100 items),
	// and pr_ref links whose PR body removed the reference.
	rows, err := tx.Query(`SELECT pr_repo, pr_num, source FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ?`, DefaultRepo, issueNum)
	if err != nil {
		return err
	}
	var stale [][2]any
	for rows.Next() {
		var prRepo, src string
		var prNum int
		if err := rows.Scan(&prRepo, &prNum, &src); err == nil {
			key := model.PRKey(prRepo, prNum)
			if src == model.LinkSourceTimeline && !timelineTruncated && !timelineKeys[key] {
				stale = append(stale, [2]any{prRepo, prNum})
			} else if src == model.LinkSourcePRRef && unlinkedSet[key] && !timelineKeys[key] {
				stale = append(stale, [2]any{prRepo, prNum})
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, st := range stale {
		if _, err := tx.Exec(`DELETE FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ? AND pr_repo = ? AND pr_num = ?`,
			DefaultRepo, issueNum, st[0], st[1]); err != nil {
			return err
		}
	}
	return nil
}

// ApplyIssueSync atomically applies an incremental or full SyncPlan without ever clobbering `issues.auto_*`.
func (s *Store) ApplyIssueSync(plan issuesync.SyncPlan) (model.Dataset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return model.Dataset{}, err
	}
	defer func() { _ = tx.Rollback() }()

	nowISO := plan.SnapshotUTC
	if nowISO == "" {
		nowISO = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

	archiveAndDelete := func(num int, reason string) (bool, error) {
		res, err := tx.Exec(`
INSERT INTO archived_issues(
  repo, number, title, auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en, archive_reason, archived_at
)
SELECT repo, number, title, auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en, ?, ?
FROM issues WHERE repo = ? AND number = ?
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title,
  auto_cat=excluded.auto_cat,
  auto_severity=excluded.auto_severity,
  auto_subsystem=excluded.auto_subsystem,
  auto_is_borderline=excluded.auto_is_borderline,
  auto_force_keep=excluded.auto_force_keep,
  auto_note_zh=excluded.auto_note_zh,
  auto_note_en=excluded.auto_note_en,
  archive_reason=excluded.archive_reason,
  archived_at=excluded.archived_at`,
			reason, nowISO, DefaultRepo, num)
		if err != nil {
			return false, err
		}
		affected, _ := res.RowsAffected()
		if _, err := tx.Exec(`DELETE FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, num); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ?`, DefaultRepo, num); err != nil {
			return false, err
		}
		// Keep issue_triage_state baseline intact so reopened issues evaluate changes against pre-close baseline
		if _, err := tx.Exec(`UPDATE issue_triage_state SET needs_retriage = 0, retriage_reasons_json = '[]', retriage_details_json = '[]' WHERE repo = ? AND number = ?`, DefaultRepo, num); err != nil {
			return false, err
		}
		return affected > 0, nil
	}

	var appliedClosed []model.DeltaClosedIssue
	for _, cr := range plan.ClosedRemovals {
		archived, err := archiveAndDelete(cr.Number, cr.Reason)
		if err != nil {
			return model.Dataset{}, err
		}
		if archived {
			appliedClosed = append(appliedClosed, cr)
		}
	}
	var appliedGraduated []model.DeltaGraduatedIssue
	for _, gr := range plan.GraduatedRemovals {
		var effCat string
		var effFK int
		err := tx.QueryRow(`
SELECT
  COALESCE(NULLIF(o.cat, ''), i.auto_cat),
  CASE WHEN COALESCE(o.has_force_keep_override, 0) != 0 THEN COALESCE(o.force_keep, 0) ELSE i.auto_force_keep END
FROM issues i
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
WHERE i.repo = ? AND i.number = ?`, DefaultRepo, gr.Number).Scan(&effCat, &effFK)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return model.Dataset{}, err
		}
		// Re-check retention against current in-tx cat/force_keep in case an override arrived during GraphQL fetch
		if effFK != 0 {
			continue
		}
		if len(gr.CurrentLabels) > 0 {
			if keep, _ := issuesync.ShouldRetainTrackedIssue(gr.CurrentLabels, false, effCat); keep {
				continue
			}
		}
		archived, err := archiveAndDelete(gr.Number, gr.Reason)
		if err != nil {
			return model.Dataset{}, err
		}
		if archived {
			appliedGraduated = append(appliedGraduated, gr)
		}
	}

	evalNumsSet := make(map[int]bool)
	reconciledIssues := make(map[int]bool)
	var restoredDelta []model.DeltaAddedIssue
	maxSyncedIssueUpdated := ""

	// Restore reopened archived issues with their preserved auto_* AND pre-close issue_triage_state baseline
	for _, upd := range plan.RestoredIssues {
		var isExcl int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, upd.Number).Scan(&isExcl); err != nil {
			return model.Dataset{}, err
		}
		if isExcl > 0 {
			continue
		}
		var cat, sev, sub, nZh, nEn string
		var bdr, fk int
		err := tx.QueryRow(`
SELECT auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en
FROM archived_issues WHERE repo = ? AND number = ?`, DefaultRepo, upd.Number).Scan(&cat, &sev, &sub, &bdr, &fk, &nZh, &nEn)
		if err != nil {
			continue
		}
		asgJSON, _ := json.Marshal(nonNilSlice(upd.Assignees))
		lbsJSON, _ := json.Marshal(nonNilSlice(upd.Labels))
		bodyVal := ""
		if upd.HasBody {
			bodyVal = upd.Body
		}
		if _, err := tx.Exec(`
INSERT INTO issues(
  repo, number, title, body, author, assignees_json, created, created_at, updated_at,
  labels_json, is_canonical, is_customer, gh_severity,
  auto_cat, auto_severity, auto_subsystem, auto_is_borderline, auto_force_keep, auto_note_zh, auto_note_en
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=excluded.title,
  body=CASE WHEN ? = 1 THEN excluded.body ELSE issues.body END,
  author=excluded.author,
  assignees_json=excluded.assignees_json,
  updated_at=excluded.updated_at,
  labels_json=excluded.labels_json,
  is_canonical=excluded.is_canonical,
  is_customer=excluded.is_customer,
  gh_severity=excluded.gh_severity`,
			DefaultRepo, upd.Number, upd.Title, bodyVal, upd.Author, string(asgJSON), upd.Created, upd.CreatedAt, upd.UpdatedAt,
			string(lbsJSON), boolToInt(upd.IsCanonical), boolToInt(upd.IsCustomer), upd.GHSeverity,
			cat, sev, sub, bdr, fk, nZh, nEn,
			boolToInt(upd.HasBody),
		); err != nil {
			return model.Dataset{}, err
		}
		if _, err := tx.Exec(`DELETE FROM archived_issues WHERE repo = ? AND number = ?`, DefaultRepo, upd.Number); err != nil {
			return model.Dataset{}, err
		}

		if err := reconcileIssueTimelineLinksInTx(tx, upd.Number, upd.TimelinePRs, upd.TimelineTruncated, plan.StreamBPRRefs[upd.Number], plan.UnlinkedStreamBPRs[upd.Number]); err != nil {
			return model.Dataset{}, err
		}
		reconciledIssues[upd.Number] = true
		if upd.HasComments {
			cmtsJSON, _ := json.Marshal(nonNilSlice(upd.RecentComments))
			if _, err := tx.Exec(`UPDATE issue_triage_state SET recent_comments_json = ? WHERE repo = ? AND number = ?`,
				string(cmtsJSON), DefaultRepo, upd.Number); err != nil {
				return model.Dataset{}, err
			}
		}
		evalNumsSet[upd.Number] = true
		maxSyncedIssueUpdated = model.MaxRFC3339(maxSyncedIssueUpdated, upd.UpdatedAt)

		effCat, effSev, effSub := cat, sev, sub
		var oCat, oSev, oSub sql.NullString
		if tx.QueryRow(`SELECT cat, severity, subsystem FROM issue_overrides WHERE repo = ? AND number = ?`, DefaultRepo, upd.Number).
			Scan(&oCat, &oSev, &oSub) == nil {
			if oCat.Valid && oCat.String != "" {
				effCat = oCat.String
			}
			if oSev.Valid && oSev.String != "" {
				effSev = oSev.String
			}
			if oSub.Valid && oSub.String != "" {
				effSub = oSub.String
			}
		}
		restoredDelta = append(restoredDelta, model.DeltaAddedIssue{
			Number:    upd.Number,
			Cat:       effCat,
			Severity:  effSev,
			Subsystem: effSub,
			Reason:    "Reopened on GitHub (restored previous triage)",
		})
	}

	// Update live GitHub metadata on tracked issues WITHOUT touching auto_*
	for _, upd := range plan.UpdatedIssues {
		asgJSON, _ := json.Marshal(nonNilSlice(upd.Assignees))
		lbsJSON, _ := json.Marshal(nonNilSlice(upd.Labels))
		res, err := tx.Exec(`
UPDATE issues
SET
  title = ?,
  body = CASE WHEN ? = 1 THEN ? ELSE body END,
  author = ?,
  assignees_json = ?,
  updated_at = ?,
  labels_json = ?,
  is_canonical = ?,
  is_customer = ?,
  gh_severity = ?
WHERE repo = ? AND number = ?`,
			upd.Title, boolToInt(upd.HasBody), upd.Body, upd.Author, string(asgJSON), upd.UpdatedAt, string(lbsJSON),
			boolToInt(upd.IsCanonical), boolToInt(upd.IsCustomer), upd.GHSeverity,
			DefaultRepo, upd.Number,
		)
		if err != nil {
			return model.Dataset{}, err
		}
		if rowsAff, _ := res.RowsAffected(); rowsAff == 0 {
			// Issue was concurrently excluded or deleted while sync was running; do not recreate PR links.
			continue
		}
		if err := reconcileIssueTimelineLinksInTx(tx, upd.Number, upd.TimelinePRs, upd.TimelineTruncated, plan.StreamBPRRefs[upd.Number], plan.UnlinkedStreamBPRs[upd.Number]); err != nil {
			return model.Dataset{}, err
		}
		reconciledIssues[upd.Number] = true
		if upd.HasComments {
			cmtsJSON, _ := json.Marshal(nonNilSlice(upd.RecentComments))
			if _, err := tx.Exec(`UPDATE issue_triage_state SET recent_comments_json = ? WHERE repo = ? AND number = ?`,
				string(cmtsJSON), DefaultRepo, upd.Number); err != nil {
				return model.Dataset{}, err
			}
		}
		evalNumsSet[upd.Number] = true
		maxSyncedIssueUpdated = model.MaxRFC3339(maxSyncedIssueUpdated, upd.UpdatedAt)
	}

	// Remove any Stream B pr_ref links whose PR body removed the issue reference even if the issue itself wasn't updated
	for issNum, unlinkedKeys := range plan.UnlinkedStreamBPRs {
		if reconciledIssues[issNum] || len(unlinkedKeys) == 0 {
			continue
		}
		for _, k := range unlinkedKeys {
			parts := strings.SplitN(k, "#", 2)
			if len(parts) != 2 {
				continue
			}
			prNum, err := strconv.Atoi(parts[1])
			if err != nil || prNum <= 0 {
				continue
			}
			res, err := tx.Exec(`
DELETE FROM issue_pr_links
WHERE issue_repo = ? AND issue_num = ? AND pr_repo = ? AND pr_num = ? AND source = 'pr_ref'`,
				DefaultRepo, issNum, parts[0], prNum)
			if err != nil {
				return model.Dataset{}, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				evalNumsSet[issNum] = true
			}
		}
	}

	// Apply direct Stream B updates to any manual/pr_ref links whose issue wasn't in UpdatedIssues
	for _, ref := range plan.DirectPRUpdates {
		if _, err := tx.Exec(`
UPDATE issue_pr_links
SET state = ?, title = ?, is_cherry_pick = ?, updated_at = CASE WHEN ? != '' THEN ? ELSE updated_at END
WHERE issue_repo = ? AND pr_repo = ? AND pr_num = ?`,
			ref.State, ref.Title, boolToInt(ref.IsCherryPick), ref.UpdatedAt, ref.UpdatedAt,
			DefaultRepo, ref.Repo, ref.Num,
		); err != nil {
			return model.Dataset{}, err
		}
		if err := syncClosedPRStatusInTx(tx, ref.Repo, ref.Num, ref.State); err != nil {
			return model.Dataset{}, err
		}
		lRows, err := tx.Query(`SELECT issue_num FROM issue_pr_links WHERE issue_repo = ? AND pr_repo = ? AND pr_num = ?`,
			DefaultRepo, ref.Repo, ref.Num)
		if err == nil {
			for lRows.Next() {
				var issNum int
				if lRows.Scan(&issNum) == nil {
					evalNumsSet[issNum] = true
				}
			}
			_ = lRows.Close()
		}
	}

	// Upsert / delete pending candidates & clear readmitted exclusions
	for _, n := range plan.DeletePendingNums {
		if _, err := tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, n); err != nil {
			return model.Dataset{}, err
		}
	}
	readmittedSet := make(map[int]bool, len(plan.ReadmittedNums))
	for _, n := range plan.ReadmittedNums {
		readmittedSet[n] = true
		if _, err := tx.Exec(`DELETE FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, n); err != nil {
			return model.Dataset{}, err
		}
	}
	for _, pc := range plan.UpsertPending {
		maxSyncedIssueUpdated = model.MaxRFC3339(maxSyncedIssueUpdated, pc.UpdatedAt)
		// Guard against concurrent triage resolution (accept -> issues, or exclude -> excluded_issues)
		// that happened while PlanSync was running outside the SQLite lock.
		var inTracked, inExcluded int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number).Scan(&inTracked); err != nil {
			return model.Dataset{}, err
		}
		if inTracked > 0 {
			_, _ = tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number)
			continue
		}
		if !readmittedSet[pc.Number] {
			if err := tx.QueryRow(`SELECT COUNT(*) FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number).Scan(&inExcluded); err != nil {
				return model.Dataset{}, err
			}
			if inExcluded > 0 {
				_, _ = tx.Exec(`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, pc.Number)
				continue
			}
		}
		b, err := json.Marshal(pc)
		if err != nil {
			return model.Dataset{}, err
		}
		if _, err := tx.Exec(`
INSERT INTO pending_candidates(repo, number, payload_json, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET payload_json=excluded.payload_json, updated_at=excluded.updated_at`,
			DefaultRepo, pc.Number, string(b), pc.UpdatedAt,
		); err != nil {
			return model.Dataset{}, err
		}
	}

	if len(plan.ClearedRecheckNums) > 0 {
		if err := removeRecheckNumInTx(tx, plan.ClearedRecheckNums...); err != nil {
			return model.Dataset{}, err
		}
	}

	// Ensure baselines exist and evaluate dynamic re-triage triggers
	if err := s.backfillTriageBaselines(tx); err != nil {
		return model.Dataset{}, err
	}
	if len(evalNumsSet) > 0 {
		evalNums := make([]int, 0, len(evalNumsSet))
		for n := range evalNumsSet {
			evalNums = append(evalNums, n)
		}
		if err := s.reevaluateIssueRetriageInTx(tx, evalNums); err != nil {
			return model.Dataset{}, err
		}
	}

	// Update sync_meta in kv_meta, preserving previous non-empty delta when this sync had 0 structural changes
	var prevMeta model.SyncMeta
	var rawMeta string
	if err := tx.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'sync_meta'`).Scan(&rawMeta); err == nil {
		_ = json.Unmarshal([]byte(rawMeta), &prevMeta)
	}
	newDelta := &model.SnapshotDelta{
		PrevSnapshotDate:          prevMeta.SnapshotUTC,
		ClosedIssuesRemoved:       nonNilSlice(appliedClosed),
		RelabeledGraduatedRemoved: nonNilSlice(appliedGraduated),
		NewOpenCandidatesAdded:    nonNilSlice(restoredDelta),
	}
	if !newDelta.HasChanges() && prevMeta.DeltaSincePrevSnapshot != nil && prevMeta.DeltaSincePrevSnapshot.HasChanges() {
		newDelta = prevMeta.DeltaSincePrevSnapshot
	}

	prevMeta.SchemaVersion = model.SchemaVersion
	prevMeta.SupportedLocales = []string{"zh-CN", "en"}
	prevMeta.SnapshotUTC = nowISO
	prevMeta.SnapshotUTC8 = prstatus.ToUTC8Display(nowISO) + " (UTC+8)"
	if model.CompareRFC3339(maxSyncedIssueUpdated, prevMeta.MaxIssueUpdatedUTC) > 0 {
		prevMeta.MaxIssueUpdatedUTC = model.NormalizeRFC3339UTC(maxSyncedIssueUpdated)
		prevMeta.MaxIssueUpdatedUTC8 = prstatus.ToUTC8Display(prevMeta.MaxIssueUpdatedUTC) + " (UTC+8)"
		prevMeta.IncrementalGHSearchQualifier = "updated:>=" + prevMeta.MaxIssueUpdatedUTC
	}
	prevMeta.DeltaSincePrevSnapshot = newDelta

	metaBytes, err := json.Marshal(prevMeta)
	if err != nil {
		return model.Dataset{}, err
	}
	if _, err := tx.Exec(`INSERT INTO kv_meta(key, val_json) VALUES('sync_meta', ?) ON CONFLICT(key) DO UPDATE SET val_json=excluded.val_json`, string(metaBytes)); err != nil {
		return model.Dataset{}, err
	}

	if err := tx.Commit(); err != nil {
		return model.Dataset{}, err
	}

	return s.buildDatasetLocked(true)
}

func (s *Store) reevaluateIssueRetriageInTx(tx *sql.Tx, nums []int) error {
	for _, num := range nums {
		var (
			author, labelsJSON, ghSev string
			isCust                    int
			effCat, effSev            string
			baseLabelsJSON, baseGHSev string
			baseIsCust                int
			baseOpenJSON              string
			baseMergedJSON            string
			watermark, cmtsJSON       string
		)
		err := tx.QueryRow(`
SELECT
  i.author, i.labels_json, i.gh_severity, i.is_customer,
  COALESCE(NULLIF(o.cat, ''), i.auto_cat),
  COALESCE(NULLIF(o.severity, ''), i.auto_severity),
  ts.baseline_labels_json, ts.baseline_gh_severity, ts.baseline_is_customer,
  ts.baseline_open_prs_json, ts.baseline_merged_prs_json,
  ts.comment_watermark_utc, ts.recent_comments_json
FROM issues i
JOIN issue_triage_state ts ON i.repo = ts.repo AND i.number = ts.number
LEFT JOIN issue_overrides o ON i.repo = o.repo AND i.number = o.number
WHERE i.repo = ? AND i.number = ?`, DefaultRepo, num).Scan(
			&author, &labelsJSON, &ghSev, &isCust,
			&effCat, &effSev,
			&baseLabelsJSON, &baseGHSev, &baseIsCust,
			&baseOpenJSON, &baseMergedJSON,
			&watermark, &cmtsJSON,
		)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}

		var curLabels, baseLabels, baseOpenKeys, baseMergedKeys []string
		var cmts []model.PendingComment
		_ = json.Unmarshal([]byte(labelsJSON), &curLabels)
		_ = json.Unmarshal([]byte(baseLabelsJSON), &baseLabels)
		_ = json.Unmarshal([]byte(baseOpenJSON), &baseOpenKeys)
		_ = json.Unmarshal([]byte(baseMergedJSON), &baseMergedKeys)
		_ = json.Unmarshal([]byte(cmtsJSON), &cmts)

		var prs []model.LinkedPRRef
		lRows, err := tx.Query(`
SELECT pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at
FROM issue_pr_links
WHERE issue_repo = ? AND issue_num = ?`, DefaultRepo, num)
		if err != nil {
			return err
		}
		for lRows.Next() {
			var p model.LinkedPRRef
			var isCP int
			if err := lRows.Scan(&p.Repo, &p.Num, &p.Label, &p.URL, &p.State, &p.Title, &isCP, &p.UpdatedAt); err == nil {
				p.IsCherryPick = isCP != 0
				prs = append(prs, p)
			}
		}
		_ = lRows.Close()
		sortLinkedPRs(prs)

		reasons, details := triage.EvaluateRetriage(triage.RetriageEvalInput{
			IssueAuthor:          author,
			CurrentCat:           effCat,
			CurrentSeverity:      effSev,
			CurrentLabels:        curLabels,
			CurrentGHSeverity:    ghSev,
			CurrentIsCustomer:    isCust != 0,
			CurrentPRs:           prs,
			RecentComments:       cmts,
			BaselineLabels:       baseLabels,
			BaselineGHSeverity:   baseGHSev,
			BaselineIsCustomer:   baseIsCust != 0,
			BaselineOpenPRKeys:   baseOpenKeys,
			BaselineMergedPRKeys: baseMergedKeys,
			CommentWatermarkUTC:  watermark,
		})

		needs := 0
		if len(reasons) > 0 {
			needs = 1
		}
		rBytes, _ := json.Marshal(nonNilSlice(reasons))
		dBytes, _ := json.Marshal(nonNilSlice(details))
		if _, err := tx.Exec(`
UPDATE issue_triage_state
SET needs_retriage = ?, retriage_reasons_json = ?, retriage_details_json = ?
WHERE repo = ? AND number = ?`,
			needs, string(rBytes), string(dBytes), DefaultRepo, num); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) snapshotBaselineInTx(tx *sql.Tx, num int, actor string, nowISO string) error {
	var (
		updatedAt, labelsJSON, ghSev string
		isCust                       int
	)
	err := tx.QueryRow(`
SELECT updated_at, labels_json, gh_severity, is_customer
FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&updatedAt, &labelsJSON, &ghSev, &isCust)
	if err == sql.ErrNoRows {
		return fmt.Errorf("issue #%d not found", num)
	}
	if err != nil {
		return err
	}

	var prs []model.LinkedPRRef
	lRows, err := tx.Query(`
SELECT pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at
FROM issue_pr_links
WHERE issue_repo = ? AND issue_num = ?`, DefaultRepo, num)
	if err != nil {
		return err
	}
	for lRows.Next() {
		var p model.LinkedPRRef
		var isCP int
		if err := lRows.Scan(&p.Repo, &p.Num, &p.Label, &p.URL, &p.State, &p.Title, &isCP, &p.UpdatedAt); err == nil {
			p.IsCherryPick = isCP != 0
			prs = append(prs, p)
		}
	}
	_ = lRows.Close()

	var prevWatermark, cmtsJSON, prevAutoBase string
	_ = tx.QueryRow(`SELECT comment_watermark_utc, recent_comments_json, auto_baseline_json FROM issue_triage_state WHERE repo = ? AND number = ?`,
		DefaultRepo, num).Scan(&prevWatermark, &cmtsJSON, &prevAutoBase)
	if cmtsJSON == "" {
		cmtsJSON = "[]"
	}
	watermark := model.MaxRFC3339(prevWatermark, updatedAt)
	var cmts []model.PendingComment
	if json.Unmarshal([]byte(cmtsJSON), &cmts) == nil {
		for _, c := range cmts {
			watermark = model.MaxRFC3339(watermark, c.CreatedAt)
		}
	}

	openKeys, mergedKeys := triage.ExtractMainlinePRKeySets(prs)
	openJSON, _ := json.Marshal(nonNilSlice(openKeys))
	mergedJSON, _ := json.Marshal(nonNilSlice(mergedKeys))

	var hasOvr int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM issue_overrides WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&hasOvr)

	autoBaseToWrite := prevAutoBase
	if actor != "override" && hasOvr == 0 {
		if b, err := json.Marshal(savedTriageBaseline{
			TriagedAt:             nowISO,
			TriagedBy:             actor,
			BaselineLabelsJSON:    labelsJSON,
			BaselineGHSeverity:    ghSev,
			BaselineIsCustomer:    isCust,
			BaselineOpenPRsJSON:   string(openJSON),
			BaselineMergedPRsJSON: string(mergedJSON),
			CommentWatermarkUTC:   watermark,
		}); err == nil {
			autoBaseToWrite = string(b)
		}
	}

	_, err = tx.Exec(`
INSERT INTO issue_triage_state(
  repo, number, triaged_at, triaged_by,
  baseline_labels_json, baseline_gh_severity, baseline_is_customer,
  baseline_open_prs_json, baseline_merged_prs_json,
  comment_watermark_utc, recent_comments_json, auto_baseline_json,
  needs_retriage, retriage_reasons_json, retriage_details_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '[]', '[]')
ON CONFLICT(repo, number) DO UPDATE SET
  triaged_at=excluded.triaged_at,
  triaged_by=excluded.triaged_by,
  baseline_labels_json=excluded.baseline_labels_json,
  baseline_gh_severity=excluded.baseline_gh_severity,
  baseline_is_customer=excluded.baseline_is_customer,
  baseline_open_prs_json=excluded.baseline_open_prs_json,
  baseline_merged_prs_json=excluded.baseline_merged_prs_json,
  comment_watermark_utc=excluded.comment_watermark_utc,
  auto_baseline_json=excluded.auto_baseline_json,
  needs_retriage=0,
  retriage_reasons_json='[]',
  retriage_details_json='[]'`,
		DefaultRepo, num, nowISO, actor,
		labelsJSON, ghSev, isCust,
		string(openJSON), string(mergedJSON),
		watermark, cmtsJSON, autoBaseToWrite,
	)
	return err
}

func (s *Store) AcknowledgeIssueRetriage(number int, actor string) (model.IssueRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if actor == "" {
		actor = "ack"
	}
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	tx, err := s.db.Begin()
	if err != nil {
		return model.IssueRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.snapshotBaselineInTx(tx, number, actor, nowISO); err != nil {
		return model.IssueRecord{}, err
	}
	if err := tx.Commit(); err != nil {
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

func (s *Store) AcknowledgeBatchRetriage(numbers []int, actor string) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(numbers) == 0 {
		return nil, nil
	}
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	acked, err := s.acknowledgeBatchRetriageInTx(tx, numbers, actor, nowISO)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return acked, nil
}

func (s *Store) acknowledgeBatchRetriageInTx(tx *sql.Tx, numbers []int, actor string, nowISO string) ([]int, error) {
	if len(numbers) == 0 {
		return nil, nil
	}
	if actor == "" {
		actor = "ack_batch"
	}
	seen := make(map[int]bool, len(numbers))
	var acked []int
	for _, n := range numbers {
		if n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		if err := s.snapshotBaselineInTx(tx, n, actor, nowISO); err != nil {
			return nil, err
		}
		acked = append(acked, n)
	}
	sort.Slice(acked, func(i, j int) bool { return acked[i] > acked[j] })
	return acked, nil
}

func (s *Store) ExcludeExistingIssue(number int, reason string, allowCanonical bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := s.excludeExistingIssueInTx(tx, number, reason, allowCanonical, nowISO); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) excludeExistingIssueInTx(tx *sql.Tx, number int, reason string, allowCanonical bool, nowISO string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Excluded non-optimizer issue"
	}

	var (
		title, lbsJSON string
		isCan          int
		source         = "manual_exclude"
	)
	err := tx.QueryRow(`SELECT title, labels_json, is_canonical FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, number).
		Scan(&title, &lbsJSON, &isCan)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if isCan != 0 && !allowCanonical {
			return fmt.Errorf("%w: #%d (classify as FILTER_* instead)", ErrCanonicalCannotExclude, number)
		}
	} else {
		var rawPend string
		if pErr := tx.QueryRow(`SELECT payload_json FROM pending_candidates WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&rawPend); pErr == nil {
			var pc model.PendingCandidate
			if json.Unmarshal([]byte(rawPend), &pc) == nil {
				if isCanonicalLabelList(pc.Labels) && !allowCanonical {
					return fmt.Errorf("%w: #%d must be accepted with a FILTER_* category", ErrCanonicalCannotExclude, number)
				}
				title = pc.Title
				b, _ := json.Marshal(nonNilSlice(pc.Labels))
				lbsJSON = string(b)
				source = "pending_triage"
			}
		} else {
			_ = tx.QueryRow(`SELECT title FROM archived_issues WHERE repo = ? AND number = ?`, DefaultRepo, number).Scan(&title)
			lbsJSON = "[]"
			source = "manual_untracked"
		}
	}
	if lbsJSON == "" {
		lbsJSON = "[]"
	}

	if _, err := tx.Exec(`
INSERT INTO excluded_issues(repo, number, title, reason, excluded_labels_json, source, excluded_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
  title=CASE WHEN excluded.title != '' THEN excluded.title ELSE excluded_issues.title END,
  reason=excluded.reason,
  excluded_labels_json=CASE WHEN excluded.excluded_labels_json != '[]' THEN excluded.excluded_labels_json ELSE excluded_issues.excluded_labels_json END,
  source=CASE
    WHEN excluded.source != 'manual_untracked' THEN excluded.source
    WHEN excluded_issues.excluded_labels_json != '[]' THEN excluded_issues.source
    ELSE excluded.source
  END,
  excluded_at=excluded.excluded_at`,
		DefaultRepo, number, title, reason, lbsJSON, source, nowISO); err != nil {
		return err
	}
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM issues WHERE repo = ? AND number = ?`, number},
		{`DELETE FROM archived_issues WHERE repo = ? AND number = ?`, number},
		{`DELETE FROM issue_pr_links WHERE issue_repo = ? AND issue_num = ?`, number},
		{`DELETE FROM issue_overrides WHERE repo = ? AND number = ?`, number},
		{`DELETE FROM issue_triage_state WHERE repo = ? AND number = ?`, number},
		{`DELETE FROM pending_candidates WHERE repo = ? AND number = ?`, number},
	} {
		if _, err := tx.Exec(q.sql, DefaultRepo, q.arg); err != nil {
			return err
		}
	}
	if err := removeRecheckNumInTx(tx, number); err != nil {
		return err
	}
	pruneLocalAcceptedDeltaInTx(tx, number)
	return nil
}

func pruneLocalAcceptedDeltaInTx(tx *sql.Tx, number int) {
	var raw string
	if err := tx.QueryRow(`SELECT val_json FROM kv_meta WHERE key = 'sync_meta'`).Scan(&raw); err != nil {
		return
	}
	var meta model.SyncMeta
	if json.Unmarshal([]byte(raw), &meta) != nil || meta.DeltaSincePrevSnapshot == nil {
		return
	}
	orig := meta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded
	if len(orig) == 0 {
		return
	}
	filtered := make([]model.DeltaAddedIssue, 0, len(orig))
	changed := false
	for _, x := range orig {
		if x.Number == number {
			changed = true
			continue
		}
		filtered = append(filtered, x)
	}
	if !changed {
		return
	}
	meta.DeltaSincePrevSnapshot.NewOpenCandidatesAdded = filtered
	if b, err := json.Marshal(meta); err == nil {
		_, _ = tx.Exec(`INSERT INTO kv_meta(key, val_json) VALUES('sync_meta', ?) ON CONFLICT(key) DO UPDATE SET val_json=excluded.val_json`, string(b))
	}
}

func (s *Store) ListExcludedIssues() ([]model.ExcludedIssueRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
SELECT number, title, reason, excluded_labels_json, source, excluded_at
FROM excluded_issues
WHERE repo = ?
ORDER BY excluded_at DESC, number DESC`, DefaultRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.ExcludedIssueRecord, 0)
	for rows.Next() {
		var r model.ExcludedIssueRecord
		var lbsJSON string
		if err := rows.Scan(&r.Number, &r.Title, &r.Reason, &lbsJSON, &r.Source, &r.ExcludedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(lbsJSON), &r.ExcludedLabels)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RestoreExcludedIssue removes an issue from excluded_issues and queues it into
// kv_meta['restored_recheck_nums'] so the next sync-issues deterministically re-fetches
// and queues it into pending_candidates even if GitHub updatedAt has not changed.
func (s *Store) RestoreExcludedIssue(number int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`DELETE FROM excluded_issues WHERE repo = ? AND number = ?`, DefaultRepo, number)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("excluded issue #%d not found", number)
	}
	if err := addRecheckNumInTx(tx, number); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdatePendingCandidates(candidates []model.PendingCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, pc := range candidates {
		b, err := json.Marshal(pc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
UPDATE pending_candidates SET payload_json = ?, updated_at = ?
WHERE repo = ? AND number = ?`,
			string(b), pc.UpdatedAt, DefaultRepo, pc.Number); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListRetriageCandidates(onlyFlagged bool, specificNums []int) ([]model.RetriageCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ds, err := s.buildDatasetLocked(true)
	if err != nil {
		return nil, err
	}

	wantSet := make(map[int]bool, len(specificNums))
	for _, n := range specificNums {
		wantSet[n] = true
	}

	var matched []model.IssueRecord
	matchedSet := make(map[int]bool)
	for _, r := range ds.Records {
		if len(wantSet) > 0 {
			if !wantSet[r.Number] {
				continue
			}
		} else if onlyFlagged && !r.NeedsRetriage {
			continue
		}
		matched = append(matched, r)
		matchedSet[r.Number] = true
	}
	if len(matched) == 0 {
		return []model.RetriageCandidate{}, nil
	}

	cmtsByNum := make(map[int][]model.PendingComment, len(matched))
	rows, err := s.db.Query(`SELECT number, recent_comments_json FROM issue_triage_state WHERE repo = ?`, DefaultRepo)
	if err == nil {
		for rows.Next() {
			var n int
			var raw string
			if rows.Scan(&n, &raw) == nil && matchedSet[n] {
				var cmts []model.PendingComment
				if json.Unmarshal([]byte(raw), &cmts) == nil {
					cmtsByNum[n] = cmts
				}
			}
		}
		_ = rows.Close()
	}

	bodyByNum := make(map[int]string, len(matched))
	bRows, err := s.db.Query(`SELECT number, body FROM issues WHERE repo = ?`, DefaultRepo)
	if err == nil {
		for bRows.Next() {
			var n int
			var body string
			if bRows.Scan(&n, &body) == nil && matchedSet[n] && body != "" {
				bodyByNum[n] = body
			}
		}
		_ = bRows.Close()
	}

	out := make([]model.RetriageCandidate, 0, len(matched))
	for _, r := range matched {
		out = append(out, model.RetriageCandidate{
			Number:           r.Number,
			URL:              fmt.Sprintf("https://github.com/pingcap/tidb/issues/%d", r.Number),
			Title:            r.Title,
			Body:             bodyByNum[r.Number],
			Author:           r.Author,
			IsCanonical:      r.IsCanonical,
			CurrentCat:       r.Cat,
			CurrentSeverity:  r.Severity,
			CurrentSubsys:    r.Subsystem,
			IsBorderlineKept: r.IsBorderlineKept,
			ForceKeep:        r.ForceKeep,
			CurrentNote:      r.Note,
			Labels:           r.Labels,
			PRs:              r.PRs,
			TriagedAt:        r.TriagedAt,
			Reasons:          nonNilSlice(r.RetriageReasons),
			Details:          nonNilSlice(r.RetriageDetails),
			RecentComments:   cmtsByNum[r.Number],
		})
	}
	return out, nil
}

// ListBackfillNoteCandidates returns active issues that currently lack a root-cause note,
// ordered by priority (customer -> severity -> difficulty -> number DESC).
func (s *Store) ListBackfillNoteCandidates(limit int, catFilter string) ([]model.IssueRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ds, err := s.buildDatasetLocked(true)
	if err != nil {
		return nil, err
	}

	catFilter = strings.ToUpper(strings.TrimSpace(catFilter))
	var missing []model.IssueRecord
	for _, r := range ds.Records {
		if r.Note != nil && strings.TrimSpace(r.Note.ZhCN) != "" && strings.TrimSpace(r.Note.En) != "" {
			continue
		}
		if catFilter != "" {
			if r.Cat != catFilter {
				continue
			}
		} else if !model.IsRealBugCategory(r.Cat) {
			// Default to real bugs first unless --cat is explicitly passed
			continue
		}
		missing = append(missing, r)
	}

	sort.Slice(missing, func(i, j int) bool {
		a, b := missing[i], missing[j]
		if a.IsCustomer != b.IsCustomer {
			return a.IsCustomer
		}
		if a.SevRank != b.SevRank {
			return a.SevRank > b.SevRank
		}
		if a.DiffRank != b.DiffRank {
			return a.DiffRank > b.DiffRank
		}
		return a.Number > b.Number
	})

	if limit > 0 && len(missing) > limit {
		missing = missing[:limit]
	}
	return missing, nil
}

func (s *Store) addManualPRLinksInTx(tx *sql.Tx, manualLinks map[string][]string, newlyAccepted map[int]bool) (int, error) {
	if len(manualLinks) == 0 {
		return 0, nil
	}

	keys, err := sortedNumericKeys(manualLinks)
	if err != nil {
		return 0, err
	}

	added := 0
	var evalNums []int
	for _, issNum := range keys {
		var specs []string
		for _, k := range []string{strconv.Itoa(issNum), "#" + strconv.Itoa(issNum)} {
			if v, ok := manualLinks[k]; ok {
				specs = v
				break
			}
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, DefaultRepo, issNum).Scan(&exists); err != nil || exists == 0 {
			return 0, fmt.Errorf("manual_pr_links issue #%d not found", issNum)
		}
		var linkedRefs []model.LinkedPRRef
		for _, sp := range specs {
			targets, err := prstatus.ParsePRSpecs(sp, DefaultRepo)
			if err != nil {
				return 0, err
			}
			for _, t := range targets {
				label := prstatus.FormatPRLabel(t.Repo, t.Number)
				url := fmt.Sprintf("https://github.com/%s/pull/%d", t.Repo, t.Number)
				state := "OPEN"
				title := label
				isCP := 0
				updAt := ""
				var rawStatus string
				if tx.QueryRow(`SELECT state, is_cherry_pick, updated_at, status_json FROM pull_requests WHERE repo = ? AND number = ?`,
					t.Repo, t.Number).Scan(&state, &isCP, &updAt, &rawStatus) == nil {
					var st model.PRStatus
					if json.Unmarshal([]byte(rawStatus), &st) == nil && st.Title != "" {
						title = st.Title
					}
				}
				if _, err := tx.Exec(`
INSERT INTO issue_pr_links(issue_repo, issue_num, pr_repo, pr_num, label, url, state, title, is_cherry_pick, updated_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'manual')
ON CONFLICT(issue_repo, issue_num, pr_repo, pr_num) DO UPDATE SET
  state=CASE WHEN excluded.updated_at != '' THEN excluded.state ELSE issue_pr_links.state END,
  title=CASE WHEN excluded.title != '' AND excluded.title != excluded.label THEN excluded.title ELSE issue_pr_links.title END,
  is_cherry_pick=CASE WHEN excluded.updated_at != '' THEN excluded.is_cherry_pick ELSE issue_pr_links.is_cherry_pick END,
  updated_at=CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE issue_pr_links.updated_at END,
  source='manual'`,
					DefaultRepo, issNum, t.Repo, t.Number, label, url, state, title, isCP, updAt,
				); err != nil {
					return 0, err
				}
				linkedRefs = append(linkedRefs, model.LinkedPRRef{
					Repo:         t.Repo,
					Num:          t.Number,
					State:        state,
					IsCherryPick: isCP != 0,
				})
				added++
			}
		}
		if newlyAccepted[issNum] {
			nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
			if err := s.snapshotBaselineInTx(tx, issNum, "triage", nowISO); err != nil {
				return 0, err
			}
		} else {
			if err := mergeManualPRsIntoBaselineInTx(tx, issNum, linkedRefs); err != nil {
				return 0, err
			}
			evalNums = append(evalNums, issNum)
		}
	}
	if len(evalNums) > 0 {
		if err := s.reevaluateIssueRetriageInTx(tx, evalNums); err != nil {
			return 0, err
		}
	}
	return added, nil
}

// mergeManualPRsIntoBaselineInTx records newly linked manual mainline PRs into the issue's
// baseline PR key sets without resetting unrelated baseline fields (comments, labels, severity),
// so linking a PR via manual_pr_links does not immediately self-flag new_fix_pr / fix_pr_merged_still_open.
func mergeManualPRsIntoBaselineInTx(tx *sql.Tx, num int, linked []model.LinkedPRRef) error {
	addOpen, addMerged := triage.ExtractMainlinePRKeySets(linked)
	if len(addOpen) == 0 && len(addMerged) == 0 {
		return nil
	}
	var openJSON, mergedJSON, autoBase string
	err := tx.QueryRow(`
SELECT baseline_open_prs_json, baseline_merged_prs_json, auto_baseline_json
FROM issue_triage_state
WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&openJSON, &mergedJSON, &autoBase)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	mergeKeys := func(rawJSON string, extra []string) string {
		var cur []string
		_ = json.Unmarshal([]byte(rawJSON), &cur)
		seen := make(map[string]bool, len(cur)+len(extra))
		for _, k := range cur {
			if k != "" {
				seen[k] = true
			}
		}
		for _, k := range extra {
			if k != "" {
				seen[k] = true
			}
		}
		out := make([]string, 0, len(seen))
		for k := range seen {
			out = append(out, k)
		}
		sort.Strings(out)
		b, _ := json.Marshal(out)
		return string(b)
	}

	newOpenJSON := mergeKeys(openJSON, addOpen)
	newMergedJSON := mergeKeys(mergedJSON, addMerged)

	var hasOvr int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM issue_overrides WHERE repo = ? AND number = ?`, DefaultRepo, num).Scan(&hasOvr)
	if hasOvr == 0 && autoBase != "" {
		var saved savedTriageBaseline
		if json.Unmarshal([]byte(autoBase), &saved) == nil {
			saved.BaselineOpenPRsJSON = mergeKeys(saved.BaselineOpenPRsJSON, addOpen)
			saved.BaselineMergedPRsJSON = mergeKeys(saved.BaselineMergedPRsJSON, addMerged)
			if b, err := json.Marshal(saved); err == nil {
				autoBase = string(b)
			}
		}
	}

	_, err = tx.Exec(`
UPDATE issue_triage_state
SET baseline_open_prs_json = ?, baseline_merged_prs_json = ?, auto_baseline_json = ?
WHERE repo = ? AND number = ?`,
		newOpenJSON, newMergedJSON, autoBase, DefaultRepo, num)
	return err
}
