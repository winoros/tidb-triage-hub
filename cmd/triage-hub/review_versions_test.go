package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/server"
	"github.com/winoros/tidb-triage-hub/internal/store"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
	seedassets "github.com/winoros/tidb-triage-hub/seed"
)

func TestLocalAndRemoteTriageUseSameVersionContract(t *testing.T) {
	t.Setenv("TRIAGE_HUB_URL", "")
	t.Setenv("TRIAGE_HUB_TOKEN", "")
	st, err := store.Open(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.IngestSyncedDataset(model.Dataset{SyncMeta: model.SyncMeta{SnapshotUTC: "2020-01-01T00:00:00Z"}, Records: []model.IssueRecord{{Number: 1, Title: "planner issue", Cat: "MEDIUM", Severity: "MAJOR", Subsystem: "logical_rewrite"}}}, nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, syncer.NewWithRunner(st, &fakeGraphQLRunner{}), server.Options{AuthToken: "review-token"}).Handler())
	defer srv.Close()
	ds, _ := st.BuildDataset(true)
	old := ds.Records[0].Version
	if err := st.ApplyIssueOverride(1, model.TriagePatchRequest{Cat: "HARD", Severity: "MAJOR", Subsystem: "logical_rewrite"}); err != nil {
		t.Fatal(err)
	}
	dec := model.BatchTriageDecisions{ExpectedVersions: map[string]model.ReviewVersion{"1": *old}, BaseExportedAt: "2999-01-01T00:00:00Z", AckRetriage: model.FlexIntList{1}}
	raw, _ := json.Marshal(dec)
	for _, remote := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			args := []string{"--stdin", "--sync-new-prs=false", "--skip-conflicts=false", "--auto-backup=false", "--db", st.DBPath()}
			if remote {
				args = append(args, "--remote", srv.URL, "--token", "review-token")
			}
			if dry {
				args = append(args, "--dry-run")
			}
			var stdout, stderr bytes.Buffer
			err := executeTriage(args, bytes.NewReader(raw), &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "version changed") {
				t.Fatalf("remote=%v dry=%v did not reject stale decision: %v %s", remote, dry, err, stdout.String())
			}
			args = append(args, "--skip-conflicts=true")
			err = executeTriage(args, bytes.NewReader(raw), &stdout, &stderr)
			if err != nil || !strings.Contains(stdout.String(), "skipped_conflicts") {
				t.Fatalf("remote=%v dry=%v skip failed: %v %s", remote, dry, err, stdout.String())
			}
		}
	}
	ds, _ = st.BuildDataset(true)
	if ds.Records[0].Cat != "HARD" {
		t.Fatal("stale CLI call overwrote current classification")
	}
}

func TestInvalidTriageDoesNotCreateDatabase(t *testing.T) {
	t.Setenv("TRIAGE_HUB_URL", "")
	t.Setenv("TRIAGE_HUB_TOKEN", "")
	path := filepath.Join(t.TempDir(), "should-not-exist.db")
	var out, diagnostic bytes.Buffer
	err := executeTriage([]string{"--stdin", "--db", path, "--auto-backup=false"}, strings.NewReader(`{"overrides":{"12345":{"cat":"INVALID","severity":"MAJOR","subsystem":"logical_rewrite"}}}`), &out, &diagnostic)
	if err == nil {
		t.Fatal("invalid request accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("validation created database: %v", err)
	}
}

func TestExistingEmptyDatabaseDryRunUsesDefaultSeed(t *testing.T) {
	t.Setenv("TRIAGE_HUB_URL", "")
	t.Setenv("TRIAGE_HUB_TOKEN", "")
	for _, seedSource := range []string{"default-file", "embedded"} {
		for _, emptyKind := range []string{"zero-byte", "empty-schema"} {
			t.Run(seedSource+"/"+emptyKind, func(t *testing.T) {
				t.Chdir(t.TempDir())
				number := 12345
				if seedSource == "default-file" {
					if err := os.Mkdir("seed", 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile("seed/initial_dataset.json", []byte(`{"records":[{"number":12345,"title":"seeded issue","cat":"MEDIUM","severity":"MAJOR","subsystem":"logical_rewrite"}]}`), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					var seed model.Dataset
					if err := json.Unmarshal(seedassets.InitialDatasetJSON, &seed); err != nil {
						t.Fatal(err)
					}
					if len(seed.Records) == 0 {
						t.Fatal("embedded seed is empty")
					}
					number = seed.Records[0].Number
				}
				dbPath := filepath.Join(t.TempDir(), "empty.db")
				if emptyKind == "zero-byte" {
					if err := os.WriteFile(dbPath, nil, 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					st, err := store.Open(dbPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := st.Close(); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				note := model.NotePatchRequest{ZhCN: "基数估算遗漏过滤条件，导致选择低效索引", En: "Cardinality estimation misses the filter and selects an inefficient index"}
				raw, err := json.Marshal(model.BatchTriageDecisions{Notes: map[string]model.NotePatchRequest{strconv.Itoa(number): note}})
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"--stdin", "--db", dbPath, "--auto-backup=false"}
				var stdout, stderr bytes.Buffer
				if err := executeTriage(append(args, "--dry-run"), bytes.NewReader(raw), &stdout, &stderr); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(stdout.String(), `"db_validated": true`) {
					t.Fatalf("expected database validation: %s", stdout.String())
				}
				after, err := os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("dry-run modified source database")
				}
				stdout.Reset()
				if err := executeTriage(args, bytes.NewReader(raw), &stdout, &stderr); err != nil {
					t.Fatalf("real execution differs: %v", err)
				}
				st, err := store.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				ds, err := st.BuildDataset(true)
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range ds.Records {
					if rec.Number == number && rec.Note != nil && rec.Note.En == note.En {
						return
					}
				}
				t.Fatal("real execution did not seed and apply note")
			})
		}
	}
}
