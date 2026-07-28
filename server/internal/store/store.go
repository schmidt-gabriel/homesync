// Package store handles every read and write inside the data root. Nothing
// else in the server touches the filesystem, so the containment rules live in
// exactly one place.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schmidt-gabriel/homesync/server/internal/crypt"
)

// ErrOutsideRoot means a path resolved somewhere it has no business being.
var ErrOutsideRoot = errors.New("path escapes data root")

// ErrNotRegular means the target exists but is not a plain file — a symlink,
// socket or device. We refuse to serve or overwrite those.
var ErrNotRegular = errors.New("not a regular file")

type Store struct {
	root string
	// nil when the volume holds plaintext. Reading copes with either form
	// whatever this says, so turning encryption on does not strand the files
	// that were already there.
	key *crypt.Key
}

// New prepares the data root, creating it if it does not exist. A non-nil key
// means new content is encrypted on the way to disk.
func New(root string, key *crypt.Key) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create data root: %w", err)
	}
	// Resolve symlinks once, up front: if the root itself is a link, every
	// containment check below has to compare against the real location.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Store{root: resolved, key: key}, nil
}

func (s *Store) Root() string { return s.root }

// Encrypted reports whether new content is being encrypted at rest.
func (s *Store) Encrypted() bool { return s.key != nil }

// Key is what the index needs to read plaintext during a scan.
func (s *Store) Key() *crypt.Key { return s.key }

// Abs turns a validated relative path into an absolute one, refusing anything
// that would land outside the root. Callers have already run the path through
// index.CleanPath; this is the second lock on the same door.
func (s *Store) Abs(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", ErrOutsideRoot
	}
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if abs != s.root && !strings.HasPrefix(abs, s.root+string(os.PathSeparator)) {
		return "", ErrOutsideRoot
	}
	return abs, nil
}

// Stat reports on a path without following symlinks.
func (s *Store) Stat(rel string) (os.FileInfo, error) {
	abs, err := s.Abs(rel)
	if err != nil {
		return nil, err
	}
	return os.Lstat(abs)
}

// Open returns a reader over a regular file's *plaintext*. Symlinks are
// rejected rather than followed, so a link planted in the data root cannot be
// used to read arbitrary files off the host.
//
// The FileInfo describes the bytes on disk, so its Size is the encrypted
// length when the file is encrypted. Callers that need the plaintext length
// seek the reader, which is what http.ServeContent does anyway.
func (s *Store) Open(rel string) (io.ReadSeekCloser, os.FileInfo, error) {
	abs, err := s.Abs(rel)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	r, _, err := crypt.Open(abs, s.key)
	if err != nil {
		return nil, nil, err
	}
	return r, info, nil
}

// WriteResult describes a file as it landed on disk.
type WriteResult struct {
	Size   int64
	SHA256 string
	MTime  int64 // unix milliseconds
}

// Write streams r into rel atomically: content goes to a temporary file in the
// same directory, gets fsynced, and only then is renamed into place. A reader
// therefore sees either the whole old file or the whole new one, never a
// half-written one — and an interrupted upload leaves no damage behind.
//
// Writing via a fresh temp name also means we never follow a symlink sitting at
// the destination: rename replaces the link itself.
func (s *Store) Write(rel string, r io.Reader) (WriteResult, error) {
	abs, err := s.Abs(rel)
	if err != nil {
		return WriteResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return WriteResult{}, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), ".upload-*.homesync-tmp")
	if err != nil {
		return WriteResult{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()

	// The hash and the size always describe the plaintext, whatever lands on
	// disk. They are what the client compares against and what the index
	// records, and neither has any use for the length of the ciphertext.
	h := sha256.New()
	var size int64
	if s.key != nil {
		size, err = crypt.Encrypt(tmp, io.TeeReader(r, h), *s.key)
	} else {
		size, err = io.Copy(io.MultiWriter(tmp, h), r)
	}
	if err != nil {
		return WriteResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		return WriteResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return WriteResult{}, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return WriteResult{}, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return WriteResult{}, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{
		Size:   size,
		SHA256: hex.EncodeToString(h.Sum(nil)),
		MTime:  info.ModTime().UnixMilli(),
	}, nil
}

// Mkdir creates a directory and any missing parents.
func (s *Store) Mkdir(rel string) error {
	abs, err := s.Abs(rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

// Remove deletes a file, or an empty directory.
func (s *Store) Remove(rel string) error {
	abs, err := s.Abs(rel)
	if err != nil {
		return err
	}
	return os.Remove(abs)
}

// Rename moves a path within the root, creating the destination's parent.
func (s *Store) Rename(fromRel, toRel string) error {
	from, err := s.Abs(fromRel)
	if err != nil {
		return err
	}
	to, err := s.Abs(toRel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// Exists reports whether a path is present, without following symlinks.
func (s *Store) Exists(rel string) bool {
	_, err := s.Stat(rel)
	return err == nil
}
