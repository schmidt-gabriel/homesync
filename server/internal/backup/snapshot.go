package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
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
	// Unreadable counts the files and folders the walk could not look at,
	// which the server cannot when it runs unprivileged over other users'
	// data. Non-zero means both numbers are a floor, not the size.
	Unreadable int64 `json:"unreadable,omitempty"`
	MeasuredAt int64 `json:"measured_at"` // unix milliseconds
}

// measureSnapshot walks one snapshot and adds up its allocated blocks, not its
// file sizes: a sparse file or a directory is what the disk spends, not what
// ls says.
//
// A file counts as unique when it has one link. That is exact for what rsync
// writes, with one exception worth knowing: a file hard-linked to another
// inside the source arrives with two links in the same snapshot and is
// reported as shared.
//
// Anything below the snapshot that cannot be read is counted and stepped over
// rather than failing the walk: one folder owned by another user used to leave
// the whole snapshot unmeasured for good.
func measureSnapshot(dir string) (SnapshotSize, error) {
	var size SnapshotSize
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			// For a folder that cannot be listed, returning nil skips it.
			size.Unreadable++
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			size.Unreadable++
			return nil
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
	size.MeasuredAt = time.Now().UnixMilli()
	return size, err
}

// sizesToMeasure decides which snapshots to walk, given the ones on the disk
// (newest first), the sizes already stored, and the snapshots a run or a
// delete just created or removed.
//
// Walking all of them after every change is what this avoids: on a large tree
// it is many minutes per snapshot. Only the neighbours of a change can have
// moved, because a snapshot hard-links to the one before it, so the snapshots
// sharing an inode are always a contiguous run of the list. Creating one takes
// files that were unique in the one before it and shares them. Removing one
// can leave a file it shared with a single survivor, and that survivor is the
// nearest one on either side.
func sizesToMeasure(current []string, stored map[string]SnapshotSize, touched []string) []string {
	want := map[string]bool{}
	for _, name := range current {
		if _, ok := stored[name]; !ok {
			want[name] = true
		}
	}
	for _, changed := range touched {
		var older, newer string
		for _, name := range current {
			switch {
			case name == changed:
				want[name] = true
			case name > changed:
				newer = name // the list is newest first, so the last one wins
			case older == "":
				older = name
			}
		}
		for _, name := range []string{older, newer} {
			if name != "" {
				want[name] = true
			}
		}
	}

	var names []string
	for _, name := range current {
		if want[name] {
			names = append(names, name)
		}
	}
	return names
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

// sizesKey is where the measurements live in the index's meta table: a small
// JSON document keyed by snapshot name, rewritten whole after each walk.
const sizesKey = "backup_snapshot_sizes"

func loadSizes(ctx context.Context, ix *index.Index) (map[string]SnapshotSize, error) {
	sizes := map[string]SnapshotSize{}
	raw, err := ix.GetMeta(ctx, sizesKey, "")
	if err != nil || raw == "" {
		return sizes, err
	}
	if err := json.Unmarshal([]byte(raw), &sizes); err != nil {
		return map[string]SnapshotSize{}, fmt.Errorf("stored snapshot sizes are not valid JSON: %w", err)
	}
	return sizes, nil
}

func saveSizes(ctx context.Context, ix *index.Index, sizes map[string]SnapshotSize) error {
	raw, err := json.Marshal(sizes)
	if err != nil {
		return err
	}
	return ix.SetMeta(ctx, sizesKey, string(raw))
}
