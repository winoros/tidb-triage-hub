package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

const authSessionSchema = `
CREATE TABLE IF NOT EXISTS github_sessions (
	token_hash TEXT PRIMARY KEY,
	audience TEXT NOT NULL,
	user_id INTEGER NOT NULL,
	login TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS github_sessions_expiry ON github_sessions(expires_at);
`

// GitHubSession contains identity, not permissions: authorization uses the current allowlist.
type GitHubSession struct {
	UserID  int64
	Login   string
	Expires time.Time
}

func authTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// CreateGitHubSession rotates a browser session atomically. Only token hashes are stored,
// so database backups do not contain reusable bearer credentials.
func (s *Store) CreateGitHubSession(ctx context.Context, token, oldToken, audience string, session GitHubSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM github_sessions WHERE expires_at <= ?", time.Now().Unix()); err != nil {
		return err
	}
	if oldToken != "" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM github_sessions WHERE token_hash = ? AND audience = ?", authTokenHash(oldToken), audience); err != nil {
			return err
		}
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM github_sessions").Scan(&count); err != nil {
		return err
	}
	if count >= 10000 {
		return fmt.Errorf("too many active login sessions")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO github_sessions(token_hash,audience,user_id,login,expires_at) VALUES(?,?,?,?,?)",
		authTokenHash(token), audience, session.UserID, session.Login, session.Expires.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadGitHubSession(ctx context.Context, token, audience string) (GitHubSession, bool, error) {
	var session GitHubSession
	var expires int64
	err := s.db.QueryRowContext(ctx, "SELECT user_id,login,expires_at FROM github_sessions WHERE token_hash = ? AND audience = ? AND expires_at > ?",
		authTokenHash(token), audience, time.Now().Unix()).Scan(&session.UserID, &session.Login, &expires)
	if err == sql.ErrNoRows {
		return session, false, nil
	}
	if err != nil {
		return session, false, err
	}
	session.Expires = time.Unix(expires, 0)
	return session, true, nil
}

func (s *Store) DeleteGitHubSession(ctx context.Context, token, audience string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM github_sessions WHERE token_hash = ? AND audience = ?", authTokenHash(token), audience)
	return err
}

// The destination is a private temporary snapshot, never the live database.
// DELETE journaling keeps the sanitized export self-contained before its rename.
func clearSnapshotSessions(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=rw&_pragma=journal_mode(DELETE)&_pragma=secure_delete(ON)"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "DELETE FROM github_sessions"); err != nil {
		return err
	}
	return db.Close()
}
