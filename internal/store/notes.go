package store

import "database/sql"

// Notes have one owner and outlive archival/restoration. The auto_note columns
// are legacy import data only; normal reads never use them as a fallback.
func migrateCanonicalNotes(tx *sql.Tx) error {
	for _, table := range []string{"issues", "archived_issues"} {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO issue_notes(repo, number, note_zh, note_en, updated_at)
SELECT repo, number, auto_note_zh, auto_note_en, '' FROM ` + table); err != nil {
			return err
		}
	}
	return nil
}

func writeIssueNoteInTx(tx *sql.Tx, number int, zh, en, now string) error {
	_, err := tx.Exec(`INSERT INTO issue_notes(repo, number, note_zh, note_en, updated_at)
VALUES (?, ?, ?, ?, ?) ON CONFLICT(repo, number) DO UPDATE SET
note_zh=excluded.note_zh, note_en=excluded.note_en, updated_at=excluded.updated_at
WHERE issue_notes.note_zh IS NOT excluded.note_zh OR issue_notes.note_en IS NOT excluded.note_en`,
		DefaultRepo, number, zh, en, now)
	return err
}
