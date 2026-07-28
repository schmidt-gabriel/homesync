package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrCaseCollision means a different path already exists that differs only by
// letter case. Accepting it would produce two rows that map to one file on any
// case-insensitive volume.
var ErrCaseCollision = errors.New("case collision")

// EntryType distinguishes the two things we track. Symlinks are deliberately
// out of scope for v1 and are skipped by the scanner.
const (
	TypeFile = "file"
	TypeDir  = "dir"
)

// Entry is one row of the index: the state of one path at one revision.
type Entry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"` // unix milliseconds
	SHA256  string `json:"sha256,omitempty"`
	Rev     int64  `json:"rev"`
	Deleted bool   `json:"deleted"`
	Unsafe  bool   `json:"unsafe,omitempty"` // illegal name on Windows
}

// Index is the authoritative record of what exists, and at which revision.
//
// Every mutation bumps a single global counter. A client only ever remembers
// that one number and asks "what changed since N?", which is what keeps the
// multi-machine case simple: no client needs to know the others exist.
type Index struct {
	db *sql.DB

	mu       sync.Mutex
	onChange []func(rev int64)
}

const schema = `
CREATE TABLE IF NOT EXISTS files (
    path       TEXT PRIMARY KEY,
    fold       TEXT NOT NULL,
    type       TEXT NOT NULL,
    size       INTEGER NOT NULL DEFAULT 0,
    mtime      INTEGER NOT NULL DEFAULT 0,
    sha256     TEXT NOT NULL DEFAULT '',
    rev        INTEGER NOT NULL,
    deleted    INTEGER NOT NULL DEFAULT 0,
    unsafe     INTEGER NOT NULL DEFAULT 0,
    deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_files_rev ON files(rev);

-- Backstop for the explicit collision check in Upsert: even a bug upstream
-- cannot produce two live rows that collapse to one file on a case-insensitive
-- volume.
CREATE UNIQUE INDEX IF NOT EXISTS idx_files_fold_live
    ON files(fold) WHERE deleted = 0;

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS devices (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER,
    -- The subtree of the data directory this device syncs. Point two devices
    -- at the same scope and they share those files; that is how the
    -- multi-machine case is expressed rather than being the default.
    scope      TEXT NOT NULL DEFAULT ''
);

INSERT OR IGNORE INTO meta(key, value) VALUES ('current_rev', '0');
`

// Open connects to the SQLite database at path, creating it if needed.
func Open(path string) (*Index, error) {
	// _txlock=immediate makes write transactions take the write lock up front
	// instead of upgrading mid-transaction, which is where SQLITE_BUSY
	// deadlocks come from.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// One writer, serialised. The workload is tiny and this removes a whole
	// class of lock contention.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Index{db: db}, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

// DB exposes the handle for packages that own their own tables (devices).
func (ix *Index) DB() *sql.DB { return ix.db }

// OnChange registers a callback fired after every committed mutation. It is how
// the SSE endpoint learns that something moved without polling the database.
func (ix *Index) OnChange(fn func(rev int64)) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.onChange = append(ix.onChange, fn)
}

func (ix *Index) notify(rev int64) {
	ix.mu.Lock()
	fns := make([]func(int64), len(ix.onChange))
	copy(fns, ix.onChange)
	ix.mu.Unlock()

	for _, fn := range fns {
		fn(rev)
	}
}

// CurrentRev returns the newest revision in the index.
func (ix *Index) CurrentRev(ctx context.Context) (int64, error) {
	var rev int64
	err := ix.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'current_rev'`).Scan(&rev)
	return rev, err
}

// nextRev allocates the next revision inside an open transaction, so the bump
// and the row it describes commit together or not at all.
func nextRev(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = 'current_rev'`); err != nil {
		return 0, err
	}
	var rev int64
	err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'current_rev'`).Scan(&rev)
	return rev, err
}

const entryColumns = `path, type, size, mtime, sha256, rev, deleted, unsafe`

func scanEntry(s interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	err := s.Scan(&e.Path, &e.Type, &e.Size, &e.MTime, &e.SHA256, &e.Rev, &e.Deleted, &e.Unsafe)
	return e, err
}

// Lookup returns the entry for a path. Tombstones are returned too — a caller
// needs to tell "never existed" from "was deleted at revision N".
func (ix *Index) Lookup(ctx context.Context, path string) (Entry, bool, error) {
	row := ix.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM files WHERE path = ?`, path)

	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

// Changes returns every entry with rev > since, oldest first, along with the
// current revision. Tombstones are included: that is how a client that was
// offline learns what disappeared.
//
// An empty scope covers the whole tree; otherwise only paths inside it are
// returned, still carrying their full paths. Translating those to be relative
// to the scope is the API layer's job.
//
// The revision counter stays global rather than per scope. Filtering by scope
// and by `rev > since` together is still correct, and one counter means a
// device that later has its scope widened does not have to resync from zero.
//
// A limit of 0 means no limit.
func (ix *Index) Changes(ctx context.Context, scope string, since int64, limit int) ([]Entry, int64, error) {
	current, err := ix.CurrentRev(ctx)
	if err != nil {
		return nil, 0, err
	}

	query := `SELECT ` + entryColumns + ` FROM files WHERE rev > ?`
	args := []any{since}

	if scope != "" {
		// Strictly inside the scope. The scope directory itself is the device's
		// root, and a client has no entry for its own root.
		//
		// This has to be part of the query, not a filter applied to the result.
		// Dropping rows afterwards would let a page come back empty while
		// `more` was true, and the documented paging rule — ask again from the
		// last entry's revision — has no last entry to use.
		query += ` AND path LIKE ? || '/%'`
		args = append(args, scope)
	}

	query += ` ORDER BY rev ASC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := ix.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, current, rows.Err()
}

// All returns every live entry. Used by the scanner to reconcile the index
// against what is actually on disk.
func (ix *Index) All(ctx context.Context) ([]Entry, error) {
	rows, err := ix.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM files WHERE deleted = 0 ORDER BY path`)
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

// CheckCaseCollision reports whether a *different* live path differs from p
// only by letter case.
//
// Callers must run this before writing anything to disk. The data directory is
// very often on a case-insensitive volume (APFS and NTFS both are by default),
// where writing "NOTES.md" silently replaces "notes.md" — so rejecting only
// after the write would already have destroyed the original.
func (ix *Index) CheckCaseCollision(ctx context.Context, p string) error {
	var other string
	err := ix.db.QueryRowContext(ctx,
		`SELECT path FROM files WHERE fold = ? AND path <> ? AND deleted = 0`,
		FoldPath(p), p).Scan(&other)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	default:
		return fmt.Errorf("%w: %q already exists", ErrCaseCollision, other)
	}
}

// Upsert records a new or changed path and returns the revision assigned to it.
//
// It returns ErrCaseCollision if a *different* live path differs only by case.
// The same check runs inside the transaction here so the index cannot be
// corrupted even if a caller forgets the pre-flight one.
func (ix *Index) Upsert(ctx context.Context, e Entry) (int64, error) {
	rev, err := ix.write(ctx, func(ctx context.Context, tx *sql.Tx) (int64, error) {
		var other string
		err := tx.QueryRowContext(ctx,
			`SELECT path FROM files WHERE fold = ? AND path <> ? AND deleted = 0`,
			FoldPath(e.Path), e.Path).Scan(&other)
		switch {
		case err == nil:
			return 0, fmt.Errorf("%w: %q already exists", ErrCaseCollision, other)
		case !errors.Is(err, sql.ErrNoRows):
			return 0, err
		}

		rev, err := nextRev(ctx, tx)
		if err != nil {
			return 0, err
		}

		_, err = tx.ExecContext(ctx, `
            INSERT INTO files (path, fold, type, size, mtime, sha256, rev, deleted, unsafe, deleted_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, NULL)
            ON CONFLICT(path) DO UPDATE SET
                type = excluded.type, size = excluded.size, mtime = excluded.mtime,
                sha256 = excluded.sha256, rev = excluded.rev,
                deleted = 0, unsafe = excluded.unsafe, deleted_at = NULL`,
			e.Path, FoldPath(e.Path), e.Type, e.Size, e.MTime, e.SHA256, rev, IsWindowsUnsafe(e.Path))
		return rev, err
	})
	return rev, err
}

// MarkDeleted turns a path into a tombstone and returns the new revision.
// The row is kept so offline clients still learn about the deletion; a separate
// job prunes tombstones once they are older than any plausible client.
func (ix *Index) MarkDeleted(ctx context.Context, path string, now int64) (int64, error) {
	return ix.write(ctx, func(ctx context.Context, tx *sql.Tx) (int64, error) {
		rev, err := nextRev(ctx, tx)
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx, `
            UPDATE files SET deleted = 1, rev = ?, deleted_at = ?, sha256 = '', size = 0
            WHERE path = ?`, rev, now, path)
		return rev, err
	})
}

// PruneTombstones drops delete markers older than before, reclaiming space.
// A client offline for longer than the retention window must resync from zero.
func (ix *Index) PruneTombstones(ctx context.Context, before int64) (int64, error) {
	res, err := ix.db.ExecContext(ctx,
		`DELETE FROM files WHERE deleted = 1 AND deleted_at IS NOT NULL AND deleted_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// write runs fn in a transaction and fires change notifications only after a
// successful commit, so a listener never sees a revision that got rolled back.
func (ix *Index) write(ctx context.Context, fn func(context.Context, *sql.Tx) (int64, error)) (int64, error) {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rev, err := fn(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	ix.notify(rev)
	return rev, nil
}
