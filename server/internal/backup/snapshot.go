package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

var (
	// ErrInvalidSnapshot is a name that could not be a snapshot at all, which
	// is also what keeps a delete request from naming a path outside the
	// backup directory.
	ErrInvalidSnapshot = errors.New("not a snapshot name")
	// ErrSnapshotNotFound is a well-formed name with no snapshot behind it.
	ErrSnapshotNotFound = errors.New("no such snapshot")
)

// SnapshotSize is how much of the backup disk one snapshot accounts for, as two
// numbers because hard links make one of them misleading on its own.
type SnapshotSize struct {
	// Total is everything the snapshot reaches, shared or not: what a restore
	// from it would write.
	Total int64 `json:"total"`
	// Unique is what only this snapshot holds — what deleting it gives back.
	// An unchanged file is one inode linked from every snapshot that has it, and
	// deleting one of those names frees nothing.
	Unique int64 `json:"unique"`
}

// measureSnapshot walks one snapshot and adds up its allocated blocks, not its
// file sizes: a sparse file or a directory is what the disk spends, not what
// ls says.
//
// A file counts as unique when it has one link. That is exact for what rsync
// writes, with one exception worth knowing: a file hard-linked to another
// inside the source arrives with two links in the same snapshot and is
// reported as shared.
func measureSnapshot(dir string) (SnapshotSize, error) {
	var size SnapshotSize
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no stat information", path)
		}
		bytes := int64(st.Blocks) * 512
		size.Total += bytes
		// Directories are never hard-linked, so each snapshot owns its own.
		if entry.IsDir() || uint64(st.Nlink) <= 1 {
			size.Unique += bytes
		}
		return nil
	})
	return size, err
}

// deleteSnapshot removes one snapshot by name. The name comes from a URL, so it
// is checked against the snapshot pattern before it is joined to anything, and
// the result must be a real directory: a symlink named like a date is not a
// snapshot, and RemoveAll on it would be one step from following it.
//
// Hard links make this safe for the others in the same way as retention: their
// content survives as long as any snapshot still links to it.
func deleteSnapshot(dir, name string) error {
	if !snapshotName.MatchString(name) {
		return ErrInvalidSnapshot
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return ErrSnapshotNotFound
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// removeLatestLink deletes the "latest" symlink that earlier versions kept
// beside the snapshots. Only a link: anything else by that name was put there
// by someone else.
func removeLatestLink(dir string) error {
	path := filepath.Join(dir, "latest")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Mode()&os.ModeSymlink == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(path)
}
