package store

import (
	"github.com/winoros/tidb-triage-hub/internal/model"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBugHistoryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`DROP TABLE bug_history;
PRAGMA user_version=5;
INSERT INTO sync_runs(kind,started_at,finished_at,summary_json) VALUES
('issues_dual_stream','','2026-11-01T15:00:00Z','{"script_output":"  Candidates    : 5 -> 5 (canonical: 5, real_bugs: 4, retriage: 0)"}'),
('issues_dual_stream','','2026-11-01T15:10:00Z','{"script_output":"  Candidates    : 5 -> 4 (canonical: 4, real_bugs: 3, retriage: 0)"}'),
('issues_dual_stream','','2026-11-01T16:00:00Z','{"script_output":"  Candidates    : 4 -> 2 (canonical: 2, real_bugs: 0, retriage: 0)"}');`)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ds, err := s.BuildDataset(false)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.BugCountPoint{{Date: "2026-11-01", Count: 3}, {Date: "2026-11-02", Count: 0}}
	if !reflect.DeepEqual(ds.BugHistory, want) {
		t.Fatalf("got history %+v, want %+v", ds.BugHistory, want)
	}
}
