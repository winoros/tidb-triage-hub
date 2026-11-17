package store

import (
	"fmt"
	"net/url"
	"strings"
)

const schemaVersion = 4

func uriMode(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("mode")
}

// Opening an up-to-date database is read-only. Repairs and trigger replacement
// run once, atomically with the schema version, rather than on every CLI read.
func (s *Store) ensureSchema(readOnly bool) error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", version, schemaVersion)
	}
	if readOnly {
		return fmt.Errorf("database schema upgrade required; open it writable once before using mode=ro")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Another process may have completed the migration while we waited.
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return tx.Commit()
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", version, schemaVersion)
	}
	rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'version_%'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.Exec(`DROP TRIGGER "` + strings.ReplaceAll(name, `"`, `""`) + `"`); err != nil {
			return err
		}
	}
	if err := s.migrate(tx); err != nil {
		return err
	}
	if err := migratePRFactsInTx(tx); err != nil {
		return err
	}
	if err := invalidatePRAssessmentsInTx(tx); err != nil {
		return err
	}
	// Existing versioned exports must be re-reviewed after a protocol upgrade.
	if _, err := tx.Exec(`UPDATE issue_versions SET review_version = review_version + 1, note_version = note_version + 1`); err != nil {
		return err
	}
	if err := migrateWorkbenchRevision(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}
