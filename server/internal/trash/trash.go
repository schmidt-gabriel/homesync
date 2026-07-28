// Package trash keeps the previous content of anything overwritten or deleted,
// so a mistake on one machine that propagates everywhere in seconds is still
// recoverable.
package trash

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DirName is the directory, relative to the data root, where discarded content
// lives. The scanner skips it, so nothing in here ever syncs.
const DirName = ".trash"

// timeLayout is filename-safe and sorts chronologically as plain text.
const timeLayout = "20060102T150405.000"

// Item is one entry in the trash.
type Item struct {
	// ID is the on-disk filename, used to restore.
	ID string `json:"id"`
	// Path is the original location, relative to the data root.
	Path string `json:"path"`
	// DeletedAt is when it was discarded.
	DeletedAt time.Time `json:"deleted_at"`
	Size      int64     `json:"size"`
}

type Trash struct {
	dir string
}

// New prepares the trash directory inside the data root.
func New(dataRoot string) (*Trash, error) {
	dir := filepath.Join(dataRoot, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create trash: %w", err)
	}
	return &Trash{dir: dir}, nil
}

func (t *Trash) Dir() string { return t.dir }

// encodeID builds a flat filename that round-trips back to the original path.
// Percent-encoding the separators keeps the trash a single flat directory —
// no nested structure to create, prune or get half-deleted.
func encodeID(when time.Time, path string) string {
	return when.UTC().Format(timeLayout) + "_" + url.PathEscape(path)
}

func decodeID(id string) (time.Time, string, bool) {
	stamp, escaped, found := strings.Cut(id, "_")
	if !found {
		return time.Time{}, "", false
	}
	when, err := time.ParseInLocation(timeLayout, stamp, time.UTC)
	if err != nil {
		return time.Time{}, "", false
	}
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return time.Time{}, "", false
	}
	return when, path, true
}

// Put moves absPath into the trash and returns the new item's ID.
//
// It is a rename, not a copy, so discarding a large file costs nothing and
// cannot half-succeed.
func (t *Trash) Put(absPath, relPath string, when time.Time) (string, error) {
	id := encodeID(when, relPath)
	dest := filepath.Join(t.dir, id)

	// Two deletions of the same path inside one millisecond would collide.
	for i := 1; ; i++ {
		if _, err := os.Lstat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(t.dir, fmt.Sprintf("%s.%d", id, i))
		id = fmt.Sprintf("%s.%d", id, i)
	}

	if err := os.Rename(absPath, dest); err != nil {
		return "", err
	}
	return id, nil
}

// List returns the trash contents, newest first.
func (t *Trash) List() ([]Item, error) {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, err
	}

	items := []Item{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		when, path, ok := decodeID(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, Item{
			ID: e.Name(), Path: path, DeletedAt: when, Size: info.Size(),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].DeletedAt.After(items[j].DeletedAt)
	})
	return items, nil
}

// Lookup finds one item by ID.
func (t *Trash) Lookup(id string) (Item, error) {
	// Reject anything that could point outside the trash directory.
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return Item{}, os.ErrNotExist
	}
	when, path, ok := decodeID(id)
	if !ok {
		return Item{}, os.ErrNotExist
	}
	info, err := os.Lstat(filepath.Join(t.dir, id))
	if err != nil {
		return Item{}, err
	}
	return Item{ID: id, Path: path, DeletedAt: when, Size: info.Size()}, nil
}

// AbsPath returns the on-disk location of a trash item.
func (t *Trash) AbsPath(id string) string { return filepath.Join(t.dir, id) }

// Empty discards everything, whatever its age, and reports how many items
// went. This is the only irreversible operation in the server: it is offered
// because a trash you cannot empty is just a disk leak with extra steps, and
// it is deliberately not reachable with a device token.
func (t *Trash) Empty() (int, error) {
	items, err := t.List()
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, item := range items {
		if err := os.Remove(filepath.Join(t.dir, item.ID)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Purge deletes everything discarded before the cutoff and reports how many
// items went.
func (t *Trash) Purge(before time.Time) (int, error) {
	items, err := t.List()
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, item := range items {
		if !item.DeletedAt.Before(before) {
			continue
		}
		if err := os.Remove(filepath.Join(t.dir, item.ID)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
