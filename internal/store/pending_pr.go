package store

import (
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/triage"
)

// Pending JSON carries issue facts and PR relationships. PR metadata and file
// evidence are always assembled from the shared authoritative stores.
func normalizePendingPRCacheInTx(q sqlExecQuerier, pc *model.PendingCandidate) error {
	before := *pc
	pc.PRs = slices.Clone(pc.PRs)
	for i := range pc.PRs {
		if err := populatePRRef(q, &pc.PRs[i]); err != nil {
			return err
		}
	}
	issuesync.SortLinkedPRRefs(pc.PRs)
	pc.PRDetails = nil
	for _, p := range triage.FilterRelevantKernelPRs(pc.PRs) {
		d, err := readPRDetail(q, p)
		if err != nil {
			return err
		}
		if d != nil {
			pc.PRDetails = append(pc.PRDetails, *d)
		}
	}
	if !pendingCoreStateEqual(before, *pc) || pc.SuggestedTriage == nil {
		triage.AnalyzeCandidate(pc)
	}
	return nil
}

// This is the pending review-version boundary for PR observations. Only a
// material projection change advances the watermark and its version trigger;
// canonical PR cache writes never bump pending review versions directly.
func syncPendingPRProjectionInTx(tx *sql.Tx, repo string, num int) error {
	rows, err := tx.Query(`SELECT payload_json, updated_at FROM pending_candidates
 WHERE repo = ? AND EXISTS (SELECT 1 FROM json_each(payload_json, '$.prs') p
 WHERE json_extract(p.value, '$.repo') = ? AND json_extract(p.value, '$.num') = ?)`, DefaultRepo, repo, num)
	if err != nil {
		return err
	}
	type pendingRow struct{ raw, watermark string }
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.raw, &p.watermark); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, row := range pending {
		var pc model.PendingCandidate
		if err := json.Unmarshal([]byte(row.raw), &pc); err != nil {
			return err
		}
		before := pc
		if err := normalizePendingPRCacheInTx(tx, &pc); err != nil {
			return err
		}
		pc.Version, pc.SyncWatermark = nil, ""
		raw, err := json.Marshal(pc)
		if err != nil {
			return err
		}
		if string(raw) == row.raw {
			continue
		}
		watermark := row.watermark
		if !pendingCoreStateEqual(before, pc) {
			watermark = nextCandidateWatermark(watermark)
		}
		if _, err := tx.Exec(`UPDATE pending_candidates SET payload_json=?,updated_at=?,revision=revision+1 WHERE repo=? AND number=?`, string(raw), watermark, DefaultRepo, pc.Number); err != nil {
			return err
		}
	}
	return nil
}

// Persist fetched observations separately from queue payload replacement. An
// old sync plan with no details cannot delete a newer successful enrichment.
func observePendingPRsInTx(tx *sql.Tx, pc model.PendingCandidate, affected map[int]bool) error {
	used := make([]bool, len(pc.PRDetails))
	for _, p := range pc.PRs {
		var detail *model.PendingPRDetail
		for i := range pc.PRDetails {
			d := &pc.PRDetails[i]
			if !used[i] && d.Repo == p.Repo && d.Num == p.Num {
				detail, used[i] = d, true
				break
			}
		}
		if _, err := observePRInTx(tx, p.Facts(), false, detail, affected); err != nil {
			return err
		}
	}
	for i, d := range pc.PRDetails {
		if !used[i] {
			if err := observePRDetailInTx(tx, d, affected); err != nil {
				return err
			}
		}
	}
	return nil
}
