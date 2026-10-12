package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What deleting a snapshot frees is only what no other snapshot links to.
// Reporting the full size instead would promise space that does not come
// back, because hard links keep the shared content alive.
func TestMeasureSnapshotSeparatesSharedFromUnique(t *testing.T) {
	dest := t.TempDir()
	older := filepath.Join(dest, "2026-01-01")
	newer := filepath.Join(dest, "2026-01-02")
	mustMkdir(t, older)
	mustMkdir(t, newer)

	const shared, unique = 256 << 10, 64 << 10
	mustWrite(t, filepath.Join(older, "shared.bin"), strings.Repeat("s", shared))
	if err := os.Link(filepath.Join(older, "shared.bin"), filepath.Join(newer, "shared.bin")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(newer, "only-here.bin"), strings.Repeat("u", unique))

	size, err := measureSnapshot(newer)
	if err != nil {
		t.Fatal(err)
	}
	if size.Total < shared+unique {
		t.Errorf("total = %d, want at least %d: shared content is still part of the snapshot", size.Total, shared+unique)
	}
	if size.Unique < unique || size.Unique >= shared {
		t.Errorf("unique = %d, want the %d bytes only it holds, not the %d it shares", size.Unique, unique, shared)
	}

	size, err = measureSnapshot(older)
	if err != nil {
		t.Fatal(err)
	}
	if size.Unique >= shared {
		t.Errorf("unique = %d for a snapshot whose only file is shared", size.Unique)
	}
}

func TestDeleteSnapshot(t *testing.T) {
	dest := t.TempDir()
	older := filepath.Join(dest, "2026-01-01")
	newer := filepath.Join(dest, "2026-01-02")
	mustMkdir(t, older)
	mustMkdir(t, newer)
	mustWrite(t, filepath.Join(older, "shared.txt"), "kept")
	if err := os.Link(filepath.Join(older, "shared.txt"), filepath.Join(newer, "shared.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dest, "notes.txt"), "not a snapshot")
	if err := os.Symlink("2026-01-02", filepath.Join(dest, "2026-01-03")); err != nil {
		t.Fatal(err)
	}

	// Each of these names something that is not a snapshot, and the request
	// comes from a URL, so every one of them is a path someone could send.
	for name, want := range map[string]error{
		"../escape":  ErrInvalidSnapshot,
		"notes.txt":  ErrInvalidSnapshot,
		"latest":     ErrInvalidSnapshot,
		"":           ErrInvalidSnapshot,
		"2026-02-01": ErrSnapshotNotFound,
		// A link named like a snapshot: removing it would be harmless, but
		// following it would delete the snapshot it points at.
		"2026-01-03": ErrSnapshotNotFound,
	} {
		if err := deleteSnapshot(dest, name); !errors.Is(err, want) {
			t.Errorf("deleteSnapshot(%q) = %v, want %v", name, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(newer, "shared.txt")); err != nil {
		t.Fatalf("a refused delete removed something: %v", err)
	}

	if err := deleteSnapshot(dest, "2026-01-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(older); !os.IsNotExist(err) {
		t.Error("the snapshot is still there")
	}
	if got := readFile(t, filepath.Join(newer, "shared.txt")); got != "kept" {
		t.Errorf("the newer snapshot's hard-linked copy reads %q", got)
	}
}
