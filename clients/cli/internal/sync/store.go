package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Local is one path as it exists on this machine right now.
type Local struct {
	Path  string
	Type  string
	Size  int64
	MTime int64 // unix milliseconds, to match the server
}

// Store is every read and write inside the sync folder, so the rules about
// atomicity and containment live in one place.
type Store struct {
	root string
}

func NewStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Store{root: resolved}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Abs(rel string) string {
	return filepath.Join(s.root, filepath.FromSlash(rel))
}

// Rel is the sync path for an absolute location, or false if it is outside the
// root. Normalised to NFC, because the server's index is composed.
func (s *Store) Rel(abs string) (string, bool) {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return norm.NFC.String(filepath.ToSlash(rel)), true
}

// Scan walks the whole tree, skipping anything the rules exclude.
func (s *Store) Scan(rules *Ignore) (map[string]Local, error) {
	found := make(map[string]Local)

	err := filepath.WalkDir(s.root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is normal, not fatal.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		rel, ok := s.Rel(abs)
		if !ok {
			return nil
		}

		if rules.Excludes(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Symlinks are out of scope for v1, matching the server.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		if d.IsDir() {
			found[rel] = Local{Path: rel, Type: "dir", MTime: info.ModTime().UnixMilli()}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		found[rel] = Local{
			Path:  rel,
			Type:  "file",
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		}
		return nil
	})

	return found, err
}

// Describe reports on a single path, or false if it is absent.
func (s *Store) Describe(rel string) (Local, bool) {
	info, err := os.Lstat(s.Abs(rel))
	if err != nil {
		return Local{}, false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Local{}, false
	}
	if info.IsDir() {
		return Local{Path: rel, Type: "dir", MTime: info.ModTime().UnixMilli()}, true
	}
	if !info.Mode().IsRegular() {
		return Local{}, false
	}
	return Local{
		Path: rel, Type: "file",
		Size: info.Size(), MTime: info.ModTime().UnixMilli(),
	}, true
}

func (s *Store) Exists(rel string) bool {
	_, err := os.Lstat(s.Abs(rel))
	return err == nil
}

// IsEmptyDir reports whether a directory holds nothing at all.
//
// Deliberately counts hidden entries too. It is asked before deleting a
// directory, and being wrong in that direction costs whatever else is in there.
func (s *Store) IsEmptyDir(rel string) (bool, error) {
	entries, err := os.ReadDir(s.Abs(rel))
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// Snapshot copies a file to a temporary location and returns where it landed.
//
// Everything that follows — hashing, uploading, recording what was sent — must
// run against this copy, never the original. An editor writing a file while it
// is being read produces bytes that belong to no version of it, and hashing one
// version while uploading another is how corruption gets in. The caller owns
// the result and must remove it.
func (s *Store) Snapshot(rel string) (string, error) {
	source, err := os.Open(s.Abs(rel))
	if err != nil {
		return "", err
	}
	defer source.Close()

	tmp, err := os.CreateTemp(s.tempDir(), ".upload-*.homesync-tmp")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, source); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// tempDir keeps working files beside the sync folder rather than inside it, so
// they are never candidates for upload and never trip the watcher.
func (s *Store) tempDir() string {
	dir := filepath.Join(os.TempDir(), "homesync-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return os.TempDir()
	}
	return dir
}

func (s *Store) TempDir() string { return s.tempDir() }

// HashFile streams a file through SHA-256, so memory is bounded by the buffer
// rather than by the file.
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

func (s *Store) Hash(rel string) (string, error) { return HashFile(s.Abs(rel)) }

// Install moves a downloaded temporary file into place atomically.
//
// A reader therefore sees the whole old file or the whole new one, never a
// half-written one, and an interrupted sync leaves nothing broken behind.
func (s *Store) Install(tmp, rel string, mtime int64) error {
	abs := s.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}

	// Rename cannot cross filesystems, and the download lands in the system
	// temp directory, which is often a different one.
	if err := os.Rename(tmp, abs); err != nil {
		if err := copyFile(tmp, abs); err != nil {
			return err
		}
		os.Remove(tmp)
	}

	// Carrying the server's mtime over means the next scan compares equal and
	// does not re-hash, let alone re-upload, what we just downloaded.
	if mtime > 0 {
		when := time.UnixMilli(mtime)
		if err := os.Chtimes(abs, when, when); err != nil {
			return fmt.Errorf("set mtime on %s: %w", rel, err)
		}
	}
	return nil
}

func copyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()

	tmp, err := os.CreateTemp(filepath.Dir(to), ".install-*.homesync-tmp")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	if _, err := io.Copy(tmp, source); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), to)
}

func (s *Store) Mkdir(rel string) error {
	return os.MkdirAll(s.Abs(rel), 0o755)
}

func (s *Store) Remove(rel string) error {
	err := os.RemoveAll(s.Abs(rel))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Move renames a path, used to park a local edit that lost a conflict.
func (s *Store) Move(from, to string) error {
	target := s.Abs(to)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.Rename(s.Abs(from), target)
}
