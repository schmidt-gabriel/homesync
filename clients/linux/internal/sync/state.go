package sync

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

// Synced is what we knew about a path the last time it synced cleanly.
//
// This is the third reference point that makes two-way sync possible. Comparing
// the disk against the server tells you they differ; comparing both against
// this tells you *which one* changed, and therefore whether to push, pull, or
// declare a conflict.
type Synced struct {
	Path   string
	Type   string
	Size   int64
	MTime  int64
	SHA256 string
	Rev    int64
}

// State is the client's record of the last confirmed sync.
type State struct {
	db *sql.DB
}

// StatePath is where a given root's record lives.
//
// Keyed by a SHA-256 of the resolved root path, and deliberately not by
// anything the runtime seeds per process. The Mac client used Go's equivalent
// of that and got a different filename on every launch: it found no state, ran
// as if it had never synced, and read the server's whole tombstone history as
// instructions to delete local files. Resolving symlinks first means a root
// reached through one shares the record of the directory it points at.
func StatePath(root string) string {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = filepath.Clean(root)
	}

	sum := sha256.Sum256([]byte(resolved))
	name := "state-" + hex.EncodeToString(sum[:8]) + ".sqlite"

	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "homesync", name)
}

func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// WAL so a crash mid-cycle leaves a consistent record rather than a
	// half-applied one.
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS files (
		path   TEXT PRIMARY KEY,
		type   TEXT NOT NULL,
		size   INTEGER NOT NULL,
		mtime  INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		rev    INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &State{db: db}, nil
}

func (s *State) Close() error { return s.db.Close() }

func (s *State) meta(key string) string {
	var value string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	return value
}

func (s *State) setMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// LastRev is the revision this client is current as of.
func (s *State) LastRev() int64 {
	rev, _ := strconv.ParseInt(s.meta("last_rev"), 10, 64)
	return rev
}

func (s *State) SetLastRev(rev int64) error {
	return s.setMeta("last_rev", strconv.FormatInt(rev, 10))
}

func (s *State) IgnoreRules() string { return s.meta("ignore_rules") }

func (s *State) IgnoreVersion() int64 {
	v, _ := strconv.ParseInt(s.meta("ignore_version"), 10, 64)
	return v
}

func (s *State) SetIgnore(rules string, version int64) error {
	if err := s.setMeta("ignore_rules", rules); err != nil {
		return err
	}
	return s.setMeta("ignore_version", strconv.FormatInt(version, 10))
}

func (s *State) Record(e Synced) error {
	_, err := s.db.Exec(
		`INSERT INTO files (path, type, size, mtime, sha256, rev)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   type = excluded.type, size = excluded.size, mtime = excluded.mtime,
		   sha256 = excluded.sha256, rev = excluded.rev`,
		e.Path, e.Type, e.Size, e.MTime, e.SHA256, e.Rev)
	return err
}

func (s *State) Forget(path string) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE path = ?`, path)
	return err
}

// Get returns what we recorded for a path, and whether we have any record.
func (s *State) Get(path string) (Synced, bool, error) {
	var e Synced
	err := s.db.QueryRow(
		`SELECT path, type, size, mtime, sha256, rev FROM files WHERE path = ?`, path,
	).Scan(&e.Path, &e.Type, &e.Size, &e.MTime, &e.SHA256, &e.Rev)

	if err == sql.ErrNoRows {
		return Synced{}, false, nil
	}
	if err != nil {
		return Synced{}, false, err
	}
	return e, true, nil
}

func (s *State) All() (map[string]Synced, error) {
	rows, err := s.db.Query(`SELECT path, type, size, mtime, sha256, rev FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]Synced)
	for rows.Next() {
		var e Synced
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &e.MTime, &e.SHA256, &e.Rev); err != nil {
			return nil, err
		}
		out[e.Path] = e
	}
	return out, rows.Err()
}

func (s *State) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	return n, err
}
