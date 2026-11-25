package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "homesync.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

// touch moves a file's mtime without changing a byte of it.
func touch(t *testing.T, abs string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(abs, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// A file whose mtime moved but whose content did not must end up with the new
// mtime in the index. Otherwise the size/mtime quick check misses on every
// later pass and the file is hashed again each time, forever -- in production
// that was 62 files rehashed (and decrypted) on every 15-minute rescan.
func TestScanRecordsTouchedMTimeWithoutNewRevision(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	root := t.TempDir()
	abs := filepath.Join(root, "notes.md")
	if err := os.WriteFile(abs, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, abs, time.UnixMilli(1_700_000_000_000))

	if _, err := ix.Scan(ctx, root, nil, nil); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	before, _, _ := ix.Lookup(ctx, "notes.md")
	revBefore, _ := ix.CurrentRev(ctx)

	touched := time.UnixMilli(1_800_000_000_000)
	touch(t, abs, touched)

	stats, err := ix.Scan(ctx, root, nil, nil)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}

	after, _, _ := ix.Lookup(ctx, "notes.md")
	if after.MTime != touched.UnixMilli() {
		t.Errorf("index mtime = %d, want the touched mtime %d (the next scan would rehash it again)",
			after.MTime, touched.UnixMilli())
	}
	if after.Rev != before.Rev {
		t.Errorf("rev moved from %d to %d: a touch must not wake clients", before.Rev, after.Rev)
	}
	if revAfter, _ := ix.CurrentRev(ctx); revAfter != revBefore {
		t.Errorf("current rev moved from %d to %d", revBefore, revAfter)
	}
	if stats.Updated != 0 {
		t.Errorf("stats.Updated = %d, want 0 for a content-identical touch", stats.Updated)
	}
}

// Same rule on the fsnotify path, which has its own copy of the check.
func TestWatcherRecordsTouchedMTimeWithoutNewRevision(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	root := t.TempDir()
	abs := filepath.Join(root, "notes.md")
	if err := os.WriteFile(abs, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, abs, time.UnixMilli(1_700_000_000_000))

	w := &Watcher{ix: ix, root: root}
	if err := w.apply(ctx, "notes.md", abs); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	before, _, _ := ix.Lookup(ctx, "notes.md")

	touched := time.UnixMilli(1_800_000_000_000)
	touch(t, abs, touched)
	if err := w.apply(ctx, "notes.md", abs); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	after, _, _ := ix.Lookup(ctx, "notes.md")
	if after.MTime != touched.UnixMilli() {
		t.Errorf("index mtime = %d, want the touched mtime %d", after.MTime, touched.UnixMilli())
	}
	if after.Rev != before.Rev {
		t.Errorf("rev moved from %d to %d: a touch must not wake clients", before.Rev, after.Rev)
	}
}
