package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

func TestEmptySnapshotUsesNormalBootstrap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.db")
	if e := os.WriteFile(p, nil, 0600); e != nil {
		t.Fatal(e)
	}
	seed, e := json.Marshal(model.Dataset{Records: []model.IssueRecord{{Number: 1, Title: "seeded", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite"}}})
	if e != nil {
		t.Fatal(e)
	}
	snap, clean, e := OpenSnapshotForDryRun(p, "", seed)
	if e != nil {
		t.Fatal(e)
	}
	defer clean()
	count, e := snap.IssueCount()
	if e != nil {
		t.Fatal(e)
	}
	real, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer real.Close()
	_, e = real.BootstrapIfEmptyWithFallback("", seed)
	if e != nil {
		t.Fatal(e)
	}
	realCount, _ := real.IssueCount()
	t.Logf("dry-run count=%d real count=%d", count, realCount)
	if count != realCount || realCount != 1 {
		t.Fatal("dry-run and execution bootstrap differ")
	}
}
func TestLockedWALSnapshotFailsWithoutCopyingOldMainFile(t *testing.T) {
	st := reviewTestStore(t)
	if _, e := st.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); e != nil {
		t.Fatal(e)
	}
	if e := st.ApplyIssueOverride(100, reviewPatch("HARD", nil)); e != nil {
		t.Fatal(e)
	}
	if _, e := st.db.Exec(`PRAGMA locking_mode=EXCLUSIVE`); e != nil {
		t.Fatal(e)
	}
	if _, e := st.db.Exec(`BEGIN EXCLUSIVE`); e != nil {
		t.Fatal(e)
	}
	defer st.db.Exec(`ROLLBACK`)
	begin := time.Now()
	snap, clean, e := OpenSnapshotForDryRun(st.DBPath(), "", nil)
	if e == nil {
		clean()
		t.Fatalf("inconsistent fallback returned a snapshot after %s: %+v", time.Since(begin), snap)
	}
	if snap != nil || clean != nil {
		t.Fatal("failed snapshot returned a usable handle")
	}
}

func TestWorkbenchRevisionOrdersCommitsAcrossConnectionsAndReopen(t *testing.T) {
	st := reviewTestStore(t)
	load := func(s *Store) WorkbenchSnapshot {
		t.Helper()
		snap, err := s.LoadWorkbenchSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	first := load(st)
	if first.Revision <= 0 || load(st).Revision != first.Revision {
		t.Fatal("reads changed revision")
	}
	writer, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	// A reader pins the revision and contents together even while a writer commits.
	tx, err := st.beginReviewRead()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRow(`SELECT revision FROM workbench_revision WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := writer.ExcludeExistingIssue(100, "test", false); err != nil {
		t.Fatal(err)
	}
	ds, err := buildDatasetFrom(tx, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range ds.Records {
		if r.Number == 100 {
			found = true
		}
	}
	if !found || revision != first.Revision {
		t.Fatal("reader mixed old revision with new contents")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	next := load(st)
	if next.Revision <= first.Revision || len(next.ExcludedIssues) != 1 {
		t.Fatal("exclusion did not advance snapshot")
	}
	if err := writer.RestoreExcludedIssue(100); err != nil {
		t.Fatal(err)
	}
	restored := load(st)
	if restored.Revision <= next.Revision || len(restored.ExcludedIssues) != 0 {
		t.Fatal("restore did not advance snapshot")
	}
	writeTx, err := writer.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeTx.Exec(`UPDATE issues SET title='rolled back' WHERE number=101`); err != nil {
		t.Fatal(err)
	}
	if err := writeTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if load(st).Revision != restored.Revision {
		t.Fatal("rollback advanced revision")
	}
	if err := writer.UpsertPRStatuses([]model.PRStatus{{Repo: DefaultRepo, Num: 12345, State: "OPEN", Title: "standalone", TrackSources: []string{"manual"}}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	last := load(st)
	if last.Revision <= restored.Revision {
		t.Fatal("standalone PR update did not advance revision")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if load(reopened).Revision != last.Revision {
		t.Fatal("restart reset revision")
	}
}

func TestSchemaThreeUpgradesWorkbenchRevision(t *testing.T) {
	st := reviewTestStore(t)
	// Reconstruct v3: existing review data and triggers, without the new clock.
	rows, err := st.db.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'workbench_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, name := range names {
		if _, err := st.db.Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`DROP TABLE workbench_revision; PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	before, err := upgraded.LoadWorkbenchSnapshot()
	if err != nil || before.Revision <= 0 {
		t.Fatalf("missing migrated revision: %+v %v", before, err)
	}
	if err := upgraded.ApplyIssueOverride(100, reviewPatch("HARD", nil)); err != nil {
		t.Fatal(err)
	}
	after, err := upgraded.LoadWorkbenchSnapshot()
	if err != nil || after.Revision <= before.Revision {
		t.Fatalf("missing migrated triggers: %+v %v", after, err)
	}
}
