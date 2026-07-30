package ignore

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

// The rule document lives in the index's meta table so that every machine
// filters the same way: a rule added on one Mac takes effect everywhere without
// touching the others.
const (
	rulesKey   = "ignore_rules"
	versionKey = "ignore_version"
)

// Default is seeded on first start: the files macOS scatters through every
// directory, the build and dependency trees that are large, churn constantly
// and are reproducible from the source next to them, and the version control
// internals that are actively harmful to sync.
const Default = `# One pattern per line. Blank lines and # comments are ignored.
# Syntax: gitignore-style globs matched against the path relative to the root.
# A trailing slash restricts a rule to directories.

.DS_Store
._*
Icon?
.Spotlight-V100
.Trashes
.fseventsd
.TemporaryItems
.DocumentRevisions-V100
*.swp
~$*

# Dependency and build directories. These are large, they churn constantly,
# and every one of them is reproducible from the source next to it, so syncing
# them costs a great deal and is worth nothing.
node_modules/
bower_components/
.pnpm-store/
.yarn/cache/
target/
build/
dist/
out/
.next/
.nuxt/
.svelte-kit/
.parcel-cache/
.turbo/
.gradle/
.venv/
venv/
__pycache__/
*.pyc
.tox/
.mypy_cache/
.pytest_cache/
.ruff_cache/
vendor/
Pods/
Carthage/
DerivedData/
.build/
.swiftpm/
*.xcuserstate
xcuserdata/
.terraform/
.stack-work/
_build/
deps/
obj/
bin/Debug/
bin/Release/

# Version control internals. A repository that syncs half-written objects
# between machines is worse than one that does not sync at all: use the remote.
.git/
.hg/
.svn/
`

// Shared is the rule document the whole server filters by, kept parsed in
// memory because the scanner asks about every path on the volume.
//
// It carries the device scopes as well. A rule is written from the point of
// view of the folder someone is looking at, and a scoped device sees its own
// subtree as the root — so reading a rule the way a client reads it means
// knowing where each client's root is.
type Shared struct {
	db *sql.DB

	mu     sync.RWMutex
	rules  *Rules
	scopes []string
}

// NewShared prepares the cache. Nothing is read until Refresh.
func NewShared(db *sql.DB) *Shared {
	return &Shared{db: db, rules: Parse(Default)}
}

// Load returns the stored document and its version, or the defaults and version
// 0 when nothing has been saved.
func (s *Shared) Load(ctx context.Context) (string, int64, error) {
	var rules string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, rulesKey).Scan(&rules)
	if errors.Is(err, sql.ErrNoRows) {
		return Default, 0, nil
	}
	if err != nil {
		return "", 0, err
	}

	var raw string
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, versionKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return rules, 0, nil
	}
	if err != nil {
		return "", 0, err
	}

	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return rules, 0, nil
	}
	return rules, version, nil
}

// Save stores a new document and returns its version. The cache is updated
// before it returns, so the next scan and the purge that follows a save both
// see the rules that were just written.
func (s *Shared) Save(ctx context.Context, rules string) (int64, error) {
	version := time.Now().UnixMilli()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?), (?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		rulesKey, rules, versionKey, strconv.FormatInt(version, 10))
	if err != nil {
		return 0, err
	}

	return version, s.Refresh(ctx)
}

// Refresh re-reads the document and the device scopes.
//
// Called on every save and before every full scan. A device registered by the
// CLI while the server is running is therefore picked up at the next scan
// rather than immediately, which only ever makes the rules *less* eager: an
// unknown scope means a path is judged by its full name alone.
func (s *Shared) Refresh(ctx context.Context) error {
	rules, _, err := s.Load(ctx)
	if err != nil {
		return err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT scope FROM devices WHERE scope <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.rules, s.scopes = Parse(rules), scopes
	s.mu.Unlock()
	return nil
}

// Skip is the index.SkipFunc the scanner and the watcher run: our own
// bookkeeping and the platform noise, plus whatever the shared rules exclude.
func (s *Shared) Skip(rel string, isDir bool) bool {
	return index.DefaultSkip(rel, isDir) || s.Excludes(rel, isDir)
}

// Excludes reports whether the shared rules keep a path off this server.
//
// A path is only dropped when every reading of it excludes it: its full name,
// and its name as each device that syncs it would see it. Requiring all of them
// is what stops the server tombstoning something one device ignores while
// another is still syncing it — which would delete and re-upload the same file
// for as long as both machines were running.
func (s *Shared) Excludes(rel string, isDir bool) bool {
	s.mu.RLock()
	rules, scopes := s.rules, s.scopes
	s.mu.RUnlock()

	if !rules.Excludes(rel, isDir) {
		return false
	}
	for _, scope := range scopes {
		inner, inScope := strings.CutPrefix(rel, scope+"/")
		if !inScope {
			continue
		}
		if !rules.Excludes(inner, isDir) {
			return false
		}
	}
	return true
}
