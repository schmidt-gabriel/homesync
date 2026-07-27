package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ScanStats summarises what a reconciliation pass changed.
type ScanStats struct {
	Seen    int
	Added   int
	Updated int
	Deleted int
	Skipped int
}

func (s ScanStats) String() string {
	return fmt.Sprintf("seen=%d added=%d updated=%d deleted=%d skipped=%d",
		s.Seen, s.Added, s.Updated, s.Deleted, s.Skipped)
}

// SkipFunc reports whether a path (relative, slash-separated) stays out of the
// index entirely.
type SkipFunc func(rel string) bool

// Scan walks root and reconciles the index against what is actually on disk.
//
// It runs at startup and on a timer. fsnotify gives us immediacy, but it drops
// events under load and knows nothing about what happened while the process was
// down, so a periodic full pass is the thing that actually guarantees the index
// is true.
func (ix *Index) Scan(ctx context.Context, root string, skip SkipFunc) (ScanStats, error) {
	var stats ScanStats

	onDisk := make(map[string]Entry)

	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is normal, not fatal.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Normalise here too: the walk reads names straight from the
		// filesystem, and on macOS those arrive decomposed.
		clean, cleanErr := CleanPath(rel)
		if cleanErr != nil {
			slog.Warn("skipping unrepresentable path", "path", rel)
			stats.Skipped++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if skip != nil && skip(clean) {
			stats.Skipped++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Symlinks are out of scope for v1: handling them properly means
		// deciding what to do with targets outside the root, and that is not
		// worth the complexity yet.
		if d.Type()&fs.ModeSymlink != 0 {
			stats.Skipped++
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			if os.IsNotExist(infoErr) {
				return nil
			}
			return infoErr
		}

		stats.Seen++
		if d.IsDir() {
			onDisk[clean] = Entry{Path: clean, Type: TypeDir, MTime: info.ModTime().UnixMilli()}
			return nil
		}
		if !info.Mode().IsRegular() {
			stats.Skipped++
			return nil
		}

		onDisk[clean] = Entry{
			Path:  clean,
			Type:  TypeFile,
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		}
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("walk %s: %w", root, err)
	}

	known, err := ix.All(ctx)
	if err != nil {
		return stats, err
	}
	knownByPath := make(map[string]Entry, len(known))
	for _, e := range known {
		knownByPath[e.Path] = e
	}

	for path, disk := range onDisk {
		prev, exists := knownByPath[path]

		// For a directory the only fact a client needs is that it exists. Its
		// mtime changes every time a child is added or removed, so tracking it
		// would bump a revision — and wake every connected machine — for every
		// single file created anywhere in the tree.
		if disk.Type == TypeDir {
			if exists && prev.Type == TypeDir {
				continue
			}
		} else if exists && prev.Type == disk.Type && prev.Size == disk.Size && prev.MTime == disk.MTime {
			// Cheap check first: only pay for a hash when size or mtime moved.
			// This is the same quick-check heuristic rsync uses by default.
			continue
		}

		if disk.Type == TypeFile {
			sum, err := HashFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				slog.Warn("cannot hash file", "path", path, "err", err)
				continue
			}
			disk.SHA256 = sum

			// Content identical despite a touched mtime: record the new mtime
			// but do not burn a revision, or every `touch` would wake every
			// client for nothing.
			if exists && prev.SHA256 == sum && prev.Size == disk.Size {
				continue
			}
		}

		if _, err := ix.Upsert(ctx, disk); err != nil {
			slog.Warn("cannot index path", "path", path, "err", err)
			continue
		}
		if exists {
			stats.Updated++
		} else {
			stats.Added++
		}
	}

	now := time.Now().UnixMilli()
	for path := range knownByPath {
		if _, stillThere := onDisk[path]; stillThere {
			continue
		}
		if _, err := ix.MarkDeleted(ctx, path, now); err != nil {
			slog.Warn("cannot tombstone path", "path", path, "err", err)
			continue
		}
		stats.Deleted++
	}

	// Recorded so the admin UI can show when the index was last known good.
	_ = ix.SetMeta(ctx, MetaLastScanAt, strconv.FormatInt(time.Now().UnixMilli(), 10))
	_ = ix.SetMeta(ctx, MetaLastScanStats, stats.String())

	return stats, nil
}

// Meta keys for the last reconciliation pass.
const (
	MetaLastScanAt    = "last_scan_at"
	MetaLastScanStats = "last_scan_stats"
)

// HashFile returns the hex-encoded SHA-256 of a file's contents, streaming so
// that a large file never lands in memory whole.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DefaultSkip keeps our own bookkeeping and the junk macOS scatters through
// every directory out of the index.
func DefaultSkip(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		switch part {
		case ".trash", ".DS_Store", ".Spotlight-V100", ".Trashes",
			".fseventsd", ".TemporaryItems", ".DocumentRevisions-V100":
			return true
		}
		if strings.HasPrefix(part, "._") {
			return true
		}
		if strings.HasSuffix(part, ".homesync-tmp") {
			return true
		}
	}
	return false
}
