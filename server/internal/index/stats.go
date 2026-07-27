package index

import (
	"context"
	"database/sql"
	"errors"
)

// Stats summarises the index for the admin overview.
type Stats struct {
	Files      int64 `json:"files"`
	Dirs       int64 `json:"dirs"`
	Tombstones int64 `json:"tombstones"`
	TotalSize  int64 `json:"total_size"`
	CurrentRev int64 `json:"current_rev"`
}

// Stats computes the index summary in a single pass over the table.
func (ix *Index) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := ix.db.QueryRowContext(ctx, `
        SELECT
            COALESCE(SUM(CASE WHEN deleted = 0 AND type = 'file' THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN deleted = 0 AND type = 'dir'  THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN deleted = 1 THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN deleted = 0 THEN size ELSE 0 END), 0)
        FROM files`).Scan(&s.Files, &s.Dirs, &s.Tombstones, &s.TotalSize)
	if err != nil {
		return Stats{}, err
	}

	s.CurrentRev, err = ix.CurrentRev(ctx)
	return s, err
}

// SetMeta stores a small piece of server state.
func (ix *Index) SetMeta(ctx context.Context, key, value string) error {
	_, err := ix.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// GetMeta reads a value, returning fallback when the key is unset.
func (ix *Index) GetMeta(ctx context.Context, key, fallback string) (string, error) {
	var value string
	err := ix.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// Browse lists live entries directly under prefix, directories first. It backs
// the file browser in the admin UI; the sync protocol never uses it.
func (ix *Index) Browse(ctx context.Context, prefix string, limit int) ([]Entry, error) {
	// Match children of prefix but not grandchildren: everything starting with
	// "<prefix>/" that has no further slash.
	pattern := prefix
	if pattern != "" {
		pattern += "/"
	}

	rows, err := ix.db.QueryContext(ctx, `
        SELECT `+entryColumns+` FROM files
        WHERE deleted = 0
          AND path LIKE ? || '%'
          AND instr(substr(path, length(?) + 1), '/') = 0
        ORDER BY type DESC, path ASC
        LIMIT ?`, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
