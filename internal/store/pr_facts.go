package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

// pull_requests owns PR facts. Link rows and pending payloads are projections,
// never competing sources when a command accepts a reviewed snapshot.
func readPRFact(q sqlExecQuerier, repo string, number int) (model.PRStatus, bool, error) {
	var st model.PRStatus
	var raw, state, updated string
	var cp, complete int
	err := q.QueryRow(`SELECT state, is_cherry_pick, updated_at, status_json, status_complete FROM pull_requests WHERE repo = ? AND number = ?`, repo, number).Scan(&state, &cp, &updated, &raw, &complete)
	if err == sql.ErrNoRows {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return st, false, err
	}
	st.Repo, st.Num, st.State, st.UpdatedAt, st.IsCherryPick = repo, number, state, updated, cp != 0
	return st, complete != 0, nil
}

// Observe only fields that this source actually fetched. In particular an old
// detail cache without labels does not turn unknown cherry-pick evidence false.
func observePRFactInTx(tx *sql.Tx, incoming model.PRStatus, fullStatus bool, affected map[int]bool) (model.PRStatus, error) {
	return observePRInTx(tx, incoming, fullStatus, nil, affected)
}

func observePRDetailInTx(tx *sql.Tx, d model.PendingPRDetail, affected map[int]bool) error {
	_, err := observePRInTx(tx, d.Facts(), false, &d, affected)
	return err
}

// Merge facts and optional fetched details before projecting or invalidating a
// review. An atomic refresh must not expose an intermediate hidden-body state.
func observePRInTx(tx *sql.Tx, incoming model.PRStatus, fullStatus bool, detail *model.PendingPRDetail, affected map[int]bool) (model.PRStatus, error) {
	if incoming.Repo == "" || incoming.Num <= 0 {
		return incoming, fmt.Errorf("%w: invalid PR identity", ErrInvalidInput)
	}
	cur, complete, err := readPRFact(tx, incoming.Repo, incoming.Num)
	if err != nil {
		return cur, err
	}
	known := cur.Repo != ""
	next, applied := model.MergePRObservation(cur, incoming, complete, fullStatus)
	complete = (applied && fullStatus) || (complete && cur.AssessmentMatches(next))
	if detail != nil {
		// A pending snapshot can carry a relationship observation and fetched
		// details at different times. Apply the same merge policy to both before
		// comparing the final evidence or updating a queue projection.
		withDetail, _ := model.MergePRObservation(next, detail.Facts(), complete, false)
		complete = complete && next.AssessmentMatches(withDetail)
		next = withDetail
	}
	if !complete {
		next = next.WithoutAssessment()
	}
	next.UpdatedDisplay = prstatus.ToUTC8Display(next.UpdatedAt)
	if next.State == "" {
		next.State = model.PRStateUnknown
	}
	if next.Label == "" {
		next.Label = prstatus.FormatPRLabel(next.Repo, next.Num)
	}
	if next.URL == "" {
		next.URL = fmt.Sprintf("https://github.com/%s/pull/%d", next.Repo, next.Num)
	}
	next.TrackSources, _ = mergeTrackSources(cur.TrackSources, incoming.TrackSources)
	if len(next.TrackSources) == 0 {
		next.TrackSources = []string{"issue_linked"}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return next, err
	}
	if _, err := tx.Exec(`INSERT INTO pull_requests(repo, number, state, is_cherry_pick, updated_at, status_json, status_complete)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(repo, number) DO UPDATE SET state=excluded.state,
is_cherry_pick=excluded.is_cherry_pick, updated_at=excluded.updated_at, status_json=excluded.status_json, status_complete=excluded.status_complete`,
		next.Repo, next.Num, next.State, boolToInt(next.IsCherryPick), next.UpdatedAt, string(raw), boolToInt(complete)); err != nil {
		return next, err
	}
	if next.State != "" && next.State != model.PRStateUnknown {
		if _, err := tx.Exec(`DELETE FROM pr_lookup_failures WHERE repo=? AND number=?`, next.Repo, next.Num); err != nil {
			return next, err
		}
	}
	previous, err := readCachedPRDetail(tx, next.Repo, next.Num)
	if err != nil {
		return next, err
	}
	current := previous
	ref := (model.LinkedPRRef{}).WithFacts(next)
	if detail != nil && model.PRDetailMatches(*detail, ref) && !model.PRObservationIsStale(next, detail.Facts()) &&
		(previous.Repo == "" || model.CompareRFC3339(detail.UpdatedAt, previous.UpdatedAt) >= 0) {
		// A failed/missing or stale fetch cannot erase existing evidence.
		raw, err := json.Marshal(detail)
		if err != nil {
			return next, err
		}
		if _, err := tx.Exec(`INSERT INTO pr_details(repo, number, detail_json) VALUES (?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET detail_json=excluded.detail_json`, detail.Repo, detail.Num, string(raw)); err != nil {
			return next, err
		}
		current = *detail
	}
	if err := projectPRFactToLinksInTx(tx, next, affected); err != nil {
		return next, err
	}
	// Link triggers cover state/title/relationship changes. Commit identities
	// remain review facts; timestamps alone are conservative guards only without
	// a known head. Otherwise compare evidence actually visible to reviewers.
	before, _ := previous.ForFacts((model.LinkedPRRef{}).WithFacts(cur))
	after, _ := current.ForFacts(ref)
	if (known && (cur.HeadOID != next.HeadOID || cur.BaseOID != next.BaseOID ||
		(next.HeadOID == "" && cur.UpdatedAt != next.UpdatedAt))) ||
		!reflect.DeepEqual(detailEvidence(before), detailEvidence(after)) {
		if err := bumpLinkedPRReviewVersions(tx, next.Repo, next.Num); err != nil {
			return next, err
		}
	}
	return next, syncPendingPRProjectionInTx(tx, next.Repo, next.Num)
}

func projectPRFactToLinksInTx(tx *sql.Tx, st model.PRStatus, affected map[int]bool) error {
	if affected != nil {
		rows, err := tx.Query(`SELECT issue_num FROM issue_pr_links WHERE issue_repo = ? AND pr_repo = ? AND pr_num = ?`, DefaultRepo, st.Repo, st.Num)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			affected[n] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE issue_pr_links SET state = ?, title = ?, is_cherry_pick = ?, updated_at = ?, merged_at = ?,
ack_at = CASE WHEN ? = 'CLOSED' THEN '' ELSE ack_at END WHERE pr_repo = ? AND pr_num = ?`,
		st.State, st.Title, boolToInt(st.IsCherryPick), st.UpdatedAt, st.MergedAt, st.State, st.Repo, st.Num)
	return err
}

// Pending versions are advanced solely by their materialized projection's
// watermark. Explicit invalidation here covers issues stored through PR links.
func bumpLinkedPRReviewVersions(tx *sql.Tx, repo string, num int) error {
	_, err := tx.Exec(`UPDATE issue_versions SET review_version = review_version + 1
WHERE EXISTS (SELECT 1 FROM issue_pr_links l WHERE l.issue_repo = issue_versions.repo AND l.issue_num = issue_versions.number AND l.pr_repo = ? AND l.pr_num = ?)`, repo, num)
	return err
}

func readCachedPRDetail(q sqlExecQuerier, repo string, num int) (model.PendingPRDetail, error) {
	var d model.PendingPRDetail
	var raw string
	err := q.QueryRow(`SELECT detail_json FROM pr_details WHERE repo = ? AND number = ?`, repo, num).Scan(&raw)
	if err == sql.ErrNoRows {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	err = json.Unmarshal([]byte(raw), &d)
	return d, err
}

func readPRDetail(q sqlExecQuerier, p model.LinkedPRRef) (*model.PendingPRDetail, error) {
	d, err := readCachedPRDetail(q, p.Repo, p.Num)
	if err != nil || d.Repo == "" {
		return nil, err
	}
	d, ok := d.ForFacts(p)
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func detailEvidence(d model.PendingPRDetail) model.PendingPRDetail {
	// Metadata and freshness live in the PR fact; only evidence affects this
	// cache's review version. Normalize empty collections for stable equality.
	d.State, d.Title, d.UpdatedAt, d.MergedAt = "", "", "", ""
	d.IsCherryPick, d.CherryPickKnown = false, false
	if len(d.NonTestFiles) == 0 {
		d.NonTestFiles = nil
	}
	if len(d.ChangedFiles) == 0 {
		d.ChangedFiles = nil
	}
	return d
}

func populatePRRef(q sqlExecQuerier, p *model.LinkedPRRef) error {
	st, _, err := readPRFact(q, p.Repo, p.Num)
	if err != nil {
		return err
	}
	if st.Repo == "" {
		return nil
	}
	*p = p.WithFacts(st)
	return nil
}

// Import legacy projections once during migration. Subsequent reads never
// repair data or invent versions in a dry-run copy.
func migratePRFactsInTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT pr_repo, pr_num, state, title, is_cherry_pick, updated_at, merged_at FROM issue_pr_links`)
	if err != nil {
		return err
	}
	var observations []model.PRStatus
	for rows.Next() {
		var st model.PRStatus
		var cp int
		if err := rows.Scan(&st.Repo, &st.Num, &st.State, &st.Title, &cp, &st.UpdatedAt, &st.MergedAt); err != nil {
			rows.Close()
			return err
		}
		st.IsCherryPick = cp != 0
		observations = append(observations, st)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.Query(`SELECT payload_json FROM pending_candidates`)
	if err != nil {
		return err
	}
	var pending []model.PendingCandidate
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var pc model.PendingCandidate
		if err := json.Unmarshal([]byte(raw), &pc); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, pc)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, st := range observations {
		st.CherryPickKnown = true
		if _, err := observePRFactInTx(tx, st, false, nil); err != nil {
			return err
		}
	}
	for _, pc := range pending {
		if err := observePendingPRsInTx(tx, pc, nil); err != nil {
			return err
		}
	}
	return nil
}

func readPRDetails(q sqlExecQuerier, prs []model.LinkedPRRef) ([]model.PendingPRDetail, error) {
	var out []model.PendingPRDetail
	for _, p := range triage.FilterRelevantKernelPRs(prs) {
		d, err := readPRDetail(q, p)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	return out, nil
}

// Older releases could combine fresh metadata with stale assessment fields.
// An upgrade invalidates those assessments once; normal reads never repair them.
func invalidatePRAssessmentsInTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT status_json FROM pull_requests`)
	if err != nil {
		return err
	}
	var statuses []model.PRStatus
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var st model.PRStatus
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			rows.Close()
			return err
		}
		statuses = append(statuses, st.WithoutAssessment())
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, st := range statuses {
		st.UpdatedDisplay = prstatus.ToUTC8Display(st.UpdatedAt)
		raw, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE pull_requests SET status_complete=0, status_json=? WHERE repo=? AND number=?`, string(raw), st.Repo, st.Num); err != nil {
			return err
		}
	}
	return nil
}

func failedPRLookupKeys(q sqlExecQuerier) (map[string]bool, error) {
	rows, err := q.Query(`SELECT repo, number FROM pr_lookup_failures`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	failed := make(map[string]bool)
	for rows.Next() {
		var repo string
		var number int
		if err := rows.Scan(&repo, &number); err != nil {
			return nil, err
		}
		failed[model.PRKey(repo, number)] = true
	}
	return failed, rows.Err()
}

// FilterAutomaticPRLookups applies the same failed-lookup policy as sync-prs.
// Explicit retries bypass this filter; a positive observation clears the marker.
func (s *Store) FilterAutomaticPRLookups(refs []model.LinkedPRRef) ([]model.LinkedPRRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	failed, err := failedPRLookupKeys(s.db)
	if err != nil {
		return nil, err
	}
	out := make([]model.LinkedPRRef, 0, len(refs))
	for _, ref := range refs {
		if !failed[model.PRKey(ref.Repo, ref.Num)] {
			out = append(out, ref)
		}
	}
	return out, nil
}
