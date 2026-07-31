package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustTime(t *testing.T, text string) time.Time {
	t.Helper()
	when, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return when
}

// The value is written out rather than compared against a second call, which
// is the whole point.
//
// The Mac client keyed this on a hash the runtime seeded per process. Two calls
// in one process agreed happily while every launch got a different filename,
// found no state, and read the server's whole tombstone history as instructions
// to delete local files. Only a constant catches that.
func TestStatePathIsStableAcrossProcesses(t *testing.T) {
	// A path that exists, so EvalSymlinks resolves rather than falling back.
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	first := StatePath(dir)
	if first != StatePath(resolved) {
		t.Errorf("a root and its resolved form disagree:\n  %s\n  %s", first, StatePath(resolved))
	}

	// And a symlink to the same directory shares its record, rather than
	// getting a second, empty one over a folder that is already full.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if StatePath(link) != first {
		t.Errorf("a symlinked root got its own state file:\n  %s\n  %s", StatePath(link), first)
	}

	if StatePath("/somewhere/else") == first {
		t.Error("two different roots share one state file")
	}
}

func TestStateRoundTrip(t *testing.T) {
	state, err := OpenState(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer state.Close()

	if rev := state.LastRev(); rev != 0 {
		t.Errorf("a fresh state should start at revision 0, got %d", rev)
	}

	entry := Synced{Path: "a/b.txt", Type: "file", Size: 11, MTime: 1785194027083,
		SHA256: "b94d27b9", Rev: 42}
	if err := state.Record(entry); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, known, err := state.Get("a/b.txt")
	if err != nil || !known {
		t.Fatalf("get: %v known=%v", err, known)
	}
	if got != entry {
		t.Errorf("round trip differs:\n got %+v\nwant %+v", got, entry)
	}

	// Recording the same path again updates rather than duplicating.
	entry.Rev = 43
	if err := state.Record(entry); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if n, err := state.Count(); err != nil || n != 1 {
		t.Errorf("count = %d (%v), want 1", n, err)
	}

	if err := state.SetLastRev(87); err != nil {
		t.Fatalf("set last rev: %v", err)
	}
	if rev := state.LastRev(); rev != 87 {
		t.Errorf("last rev = %d, want 87", rev)
	}

	if err := state.Forget("a/b.txt"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, known, _ := state.Get("a/b.txt"); known {
		t.Error("the path is still recorded after being forgotten")
	}
}

// Reopening has to find what the last run wrote, or every start reads the
// server's history as deletions.
func TestStateSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")

	state, err := OpenState(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := state.SetLastRev(99); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := state.Record(Synced{Path: "kept.txt", Type: "file", Rev: 5}); err != nil {
		t.Fatalf("record: %v", err)
	}
	state.Close()

	reopened, err := OpenState(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if rev := reopened.LastRev(); rev != 99 {
		t.Errorf("last rev after reopening = %d, want 99", rev)
	}
	if _, known, _ := reopened.Get("kept.txt"); !known {
		t.Error("the recorded file was lost across a restart")
	}
}
