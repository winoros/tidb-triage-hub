package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistentAuthSessionStorage(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	user := GitHubSession{UserID: 42, Login: "user", Expires: expires}
	if err := st.CreateGitHubSession(ctx, "random-token", "", "app", user); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := st.db.QueryRow("SELECT token_hash FROM github_sessions").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == "random-token" || hash != authTokenHash("random-token") {
		t.Fatal("raw bearer token stored")
	}
	got, ok, err := st.LoadGitHubSession(ctx, "random-token", "app")
	if err != nil || !ok || got.UserID != 42 || !got.Expires.Equal(expires) {
		t.Fatalf("load: %+v %v %v", got, ok, err)
	}
	if _, ok, _ := st.LoadGitHubSession(ctx, "random-token", "other-app"); ok {
		t.Fatal("audience mismatch accepted")
	}
	if _, ok, _ := st.LoadGitHubSession(ctx, "forged-token", "app"); ok {
		t.Fatal("forged cookie accepted")
	}
	if err := st.CreateGitHubSession(ctx, "new-token", "random-token", "app", user); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadGitHubSession(ctx, "random-token", "app"); ok {
		t.Fatal("rotation retained old session")
	}
	user.Expires = time.Now().Add(-time.Second)
	if err := st.CreateGitHubSession(ctx, "expired-token", "", "app", user); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadGitHubSession(ctx, "expired-token", "app"); ok {
		t.Fatal("expired cookie accepted")
	}
	user.Expires = expires
	if err := st.CreateGitHubSession(ctx, "fresh-token", "", "app", user); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.db.QueryRow("SELECT count(*) FROM github_sessions WHERE token_hash = ?", authTokenHash("expired-token")).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired session not pruned")
	}
}

func TestAuthSessionMigrationFromV4PreservesReviewTriggers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v4.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.Exec(`DROP TABLE github_sessions;
CREATE TRIGGER version_auth_sentinel AFTER INSERT ON kv_meta BEGIN SELECT 1; END;
PRAGMA user_version = 4;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var count int
	if err := st.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'version_auth_sentinel'").Scan(&count); err != nil || count != 1 {
		t.Fatal("session migration rebuilt review triggers")
	}
	if err := st.CreateGitHubSession(context.Background(), "token", "", "app", GitHubSession{42, "user", time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func TestExportedBackupsDoNotRestoreRevokedSessions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateGitHubSession(ctx, "cookie", "", "app", GitHubSession{42, "user", time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup.db")
	if err := st.BackupTo(backup); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.LoadGitHubSession(ctx, "cookie", "app"); err != nil || !ok {
		t.Fatal("backup changed live sessions")
	}
	if err := st.DeleteGitHubSession(ctx, "cookie", "app"); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, ok, err := restored.LoadGitHubSession(ctx, "cookie", "app"); err != nil || ok {
		t.Fatal("backup resurrected revoked session")
	}
	ro, err := Open(sqliteFileURI(st.DBPath(), "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.BackupTo(filepath.Join(dir, "readonly-backup.db")); err != nil {
		t.Fatal(err)
	}
}
