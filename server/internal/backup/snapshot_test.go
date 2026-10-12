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

// A snapshot owned by another user is the normal case when the source is a
// directory of other services' data. Giving up on the first folder that cannot
// be read left the snapshot unmeasured for good, and the page said
// "measuring…" forever.
func TestMeasureSnapshotCountsPastWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything, so nothing here is unreadable")
	}
	snapshot := filepath.Join(t.TempDir(), "2026-01-01")
	locked := filepath.Join(snapshot, "locked")
	mustMkdir(t, locked)
	mustWrite(t, filepath.Join(locked, "secret.bin"), strings.Repeat("x", 4096))
	mustWrite(t, filepath.Join(snapshot, "readable.bin"), strings.Repeat("r", 64<<10))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	size, err := measureSnapshot(snapshot)
	if err != nil {
		t.Fatalf("an unreadable folder failed the whole measurement: %v", err)
	}
	if size.Total < 64<<10 {
		t.Errorf("total = %d; the readable file was not counted", size.Total)
	}
	if size.Unreadable == 0 {
		t.Error("the folder that could not be read was not reported, so the size reads as exact")
	}
}

func TestMeasureSnapshotFailsWhenTheSnapshotIsGone(t *testing.T) {
	if _, err := measureSnapshot(filepath.Join(t.TempDir(), "2026-01-01")); err == nil {
		t.Error("measuring a snapshot that does not exist reported a size")
	}
}

// Only the snapshots whose link counts could have changed are walked again.
// Walking all of them after every run is the cost this exists to avoid.
func TestSizesToMeasure(t *testing.T) {
	current := []string{"2026-01-05", "2026-01-04", "2026-01-03", "2026-01-02", "2026-01-01"}
	all := map[string]SnapshotSize{}
	for _, name := range current {
		all[name] = SnapshotSize{}
	}
	without := func(missing ...string) map[string]SnapshotSize {
		stored := map[string]SnapshotSize{}
		for name, size := range all {
			stored[name] = size
		}
		for _, name := range missing {
			delete(stored, name)
		}
		return stored
	}

	cases := []struct {
		name    string
		stored  map[string]SnapshotSize
		touched []string
		want    []string
	}{
		// A restart with everything stored measures nothing.
		{"nothing changed", all, nil, nil},
		{"never measured", without("2026-01-03"), nil, []string{"2026-01-03"}},
		// A run links its unchanged files to the one before, which is what
		// stops that one's files being unique.
		{"new snapshot", without("2026-01-05"), []string{"2026-01-05"}, []string{"2026-01-05", "2026-01-04"}},
		// Deleting one hands what it shared to the neighbours on either side.
		{"deleted from the middle", all, []string{"2026-01-03"}, []string{"2026-01-04", "2026-01-02"}},
		{"deleted the oldest", all, []string{"2026-01-00"}, []string{"2026-01-01"}},
	}
	for _, c := range cases {
		// What was deleted is no longer on the disk when this is asked.
		var onDisk []string
		for _, name := range current {
			if c.name != "deleted from the middle" || name != "2026-01-03" {
				onDisk = append(onDisk, name)
			}
		}
		got := sizesToMeasure(onDisk, c.stored, c.touched)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: measured %v, want %v", c.name, got, c.want)
		}
	}
}
