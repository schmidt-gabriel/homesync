package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIDRoundTrip(t *testing.T) {
	when := time.Date(2026, 7, 27, 20, 13, 47, 51_000_000, time.UTC)

	// The trash is a single flat directory, so the original path has to
	// survive being flattened into one filename and come back intact.
	paths := []string{
		"notes.md",
		"projects/alpha/deep/notes.md",
		"my notes/draft two.md",
		"ação.txt",
		"weird#name?.txt",
		"a%20already-encoded.txt",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			id := encodeID(when, path)

			if filepath.Base(id) != id {
				t.Fatalf("encodeID(%q) = %q, which is not a flat filename", path, id)
			}

			gotWhen, gotPath, ok := decodeID(id)
			if !ok {
				t.Fatalf("decodeID(%q) failed", id)
			}
			if !gotWhen.Equal(when) {
				t.Errorf("timestamp: got %v, want %v", gotWhen, when)
			}
			if gotPath != path {
				t.Errorf("path: got %q, want %q", gotPath, path)
			}
		})
	}
}

func TestIDsSortChronologically(t *testing.T) {
	// List() relies on decoding, but a plain lexical sort of the filenames
	// should agree with time order too — it makes the directory readable and
	// keeps any future pruning by name honest.
	earlier := encodeID(time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC), "a.txt")
	later := encodeID(time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC), "a.txt")

	if !(earlier < later) {
		t.Errorf("expected %q to sort before %q", earlier, later)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, id := range []string{"", "no-underscore", "notatimestamp_file.txt"} {
		if _, _, ok := decodeID(id); ok {
			t.Errorf("decodeID(%q) succeeded, want failure", id)
		}
	}
}

func TestLookupRefusesEscapes(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// An id comes from a client request, so it must never be usable to reach
	// outside the trash directory.
	for _, id := range []string{
		"../../etc/passwd",
		"..",
		"sub/dir",
		`back\slash`,
		"20260727T201347.051_ok.txt/../../escape",
	} {
		t.Run(id, func(t *testing.T) {
			if _, err := tr.Lookup(id); err == nil {
				t.Errorf("Lookup(%q) succeeded, want rejection", id)
			}
		})
	}
}

func TestPutAndPurge(t *testing.T) {
	root := t.TempDir()
	tr, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	discard := func(rel string, when time.Time) string {
		t.Helper()

		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte("content of "+rel), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		id, err := tr.Put(abs, rel, when)
		if err != nil {
			t.Fatalf("Put(%q): %v", rel, err)
		}
		return id
	}

	now := time.Now()
	oldID := discard("old.txt", now.AddDate(0, 0, -40))
	freshID := discard("fresh.txt", now)

	t.Run("moves rather than copies", func(t *testing.T) {
		if _, err := os.Lstat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
			t.Error("the original file is still in place; Put should move it")
		}
	})

	t.Run("lists newest first", func(t *testing.T) {
		items, err := tr.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != freshID {
			t.Errorf("expected the newest item first, got %q", items[0].ID)
		}
	})

	t.Run("collisions within the same instant get distinct ids", func(t *testing.T) {
		when := time.Now()
		first := discard("same.txt", when)
		second := discard("same.txt", when)

		if first == second {
			t.Errorf("two deletions of %q at the same instant share id %q", "same.txt", first)
		}
		if _, err := os.Lstat(tr.AbsPath(first)); err != nil {
			t.Errorf("the first item was clobbered: %v", err)
		}
	})

	t.Run("purge removes only what is old enough", func(t *testing.T) {
		removed, err := tr.Purge(now.AddDate(0, 0, -30))
		if err != nil {
			t.Fatalf("Purge: %v", err)
		}
		if removed != 1 {
			t.Errorf("expected 1 item purged, got %d", removed)
		}

		if _, err := os.Lstat(tr.AbsPath(oldID)); !os.IsNotExist(err) {
			t.Error("the expired item survived the purge")
		}
		if _, err := os.Lstat(tr.AbsPath(freshID)); err != nil {
			t.Errorf("the recent item was purged: %v", err)
		}
	})
}
