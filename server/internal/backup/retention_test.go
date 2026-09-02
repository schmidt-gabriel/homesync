package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// The set is hand-computed against the rule as written in the README: 7
// dailies, the newest snapshot of each of 4 ISO weeks, the newest of each of
// 6 months. The interesting part is that the week and month counters advance
// only on the snapshot that first claims one, so "4 weekly" means four
// distinct weeks rather than the four newest snapshots — which in a run of
// dailies would all be in the same week and keep nothing older.
func TestClassify(t *testing.T) {
	cfg := Config{Daily: 7, Weekly: 4, Monthly: 6}

	want := []struct {
		name  string
		tiers []string
	}{
		{"2026-09-02", []string{"daily", "weekly", "monthly"}}, // newest: claims W36 and September
		{"2026-09-01", []string{"daily"}},
		{"2026-08-31", []string{"daily", "monthly"}}, // Monday, still W36; first of August
		{"2026-08-30", []string{"daily", "weekly"}},  // claims W35
		{"2026-08-29", []string{"daily"}},
		{"2026-08-28", []string{"daily"}},
		{"2026-08-27", []string{"daily"}}, // the seventh and last daily
		{"2026-08-26", nil},               // past the dailies, its week and month are taken
		{"2026-08-25", nil},
		{"2026-08-20", []string{"weekly"}}, // W34
		{"2026-08-13", []string{"weekly"}}, // W33, the fourth and last week
		{"2026-08-06", nil},                // W32 is over the weekly limit
		{"2026-07-31", []string{"monthly"}},
		{"2026-06-30", []string{"monthly"}},
		{"2026-05-31", []string{"monthly"}},
		{"2026-04-30", []string{"monthly"}}, // the sixth and last month
		{"2026-03-31", nil},
		{"2026-02-28", nil},
	}

	snapshots := make([]Snapshot, 0, len(want))
	for _, w := range want {
		snapshots = append(snapshots, Snapshot{Name: w.name})
	}

	got := Classify(snapshots, cfg)
	for i, w := range want {
		if got[i].Name != w.name {
			t.Fatalf("order changed: position %d is %s, want %s", i, got[i].Name, w.name)
		}
		if !reflect.DeepEqual(got[i].Tiers, w.tiers) {
			t.Errorf("%s: tiers = %v, want %v", w.name, got[i].Tiers, w.tiers)
		}
		if got[i].Kept != (len(w.tiers) > 0) {
			t.Errorf("%s: kept = %v, want %v", w.name, got[i].Kept, len(w.tiers) > 0)
		}
	}
}

// A single snapshot must survive its own retention pass. Without at least one
// daily, the first run of the day would delete the snapshot it had just taken
// on any date that is neither the first of its week nor of its month.
func TestClassifyKeepsTheSnapshotJustTaken(t *testing.T) {
	cfg := Config{Daily: 1, Weekly: 0, Monthly: 0}
	got := Classify([]Snapshot{{Name: "2026-09-02"}}, cfg)
	if !got[0].Kept {
		t.Fatal("the only snapshot on disk was marked for deletion")
	}
}

// Pruning must not disturb the snapshots it keeps. They share inodes through
// hard links, which is the whole reason a month of "full" backups fits on the
// disk — and it is also the thing that looks most likely to break.
func TestPruneLeavesHardLinkedContentIntact(t *testing.T) {
	dir := t.TempDir()

	const content = "the file that all three snapshots share"
	original := filepath.Join(dir, "2026-01-01", "notes.md")
	if err := os.MkdirAll(filepath.Dir(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-01-02", "2026-06-01"} {
		if err := os.MkdirAll(filepath.Join(dir, day), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(original, filepath.Join(dir, day, "notes.md")); err != nil {
			t.Fatal(err)
		}
	}
	// Not a snapshot: neither listing nor retention may touch it.
	if err := os.WriteFile(filepath.Join(dir, ".homesync_backup_disk"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	snapshots, err := ListSnapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 3 {
		t.Fatalf("ListSnapshots found %d snapshots, want 3", len(snapshots))
	}

	// One daily and one monthly: keeps 2026-06-01 (newest, June) and
	// 2026-01-01 (January's newest is 01-02 — so 01-02 is kept and 01-01 goes).
	cfg := Config{Daily: 1, Weekly: 0, Monthly: 2}
	removed, err := prune(dir, Classify(snapshots, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"2026-01-01"}) {
		t.Fatalf("pruned %v, want [2026-01-01]", removed)
	}

	// The surviving link still resolves: deleting the directory that happened
	// to be written first did not take the content with it.
	survivor, err := os.ReadFile(filepath.Join(dir, "2026-01-02", "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(survivor) != content {
		t.Fatalf("content after pruning = %q, want %q", survivor, content)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".homesync_backup_disk")); err != nil {
		t.Fatalf("retention removed the disk marker: %v", err)
	}
}
