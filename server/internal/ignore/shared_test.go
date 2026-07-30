package ignore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

func openShared(t *testing.T) (*Shared, *sql.DB) {
	t.Helper()

	ix, err := index.Open(filepath.Join(t.TempDir(), "homesync.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { ix.Close() })

	return NewShared(ix.DB()), ix.DB()
}

func addDevice(t *testing.T, db *sql.DB, name, scope string) {
	t.Helper()

	_, err := db.Exec(
		`INSERT INTO devices(id, name, token_hash, created_at, scope) VALUES (?, ?, '', 0, ?)`,
		name, name, scope)
	if err != nil {
		t.Fatalf("insert device: %v", err)
	}
}

func TestSharedLoadDefaultsUntilSaved(t *testing.T) {
	shared, _ := openShared(t)
	ctx := context.Background()

	rules, version, err := shared.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rules != Default || version != 0 {
		t.Errorf("expected the defaults at version 0, got version %d and %d bytes of rules",
			version, len(rules))
	}

	saved, err := shared.Save(ctx, "*.tmp\n")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved == 0 {
		t.Error("expected a non-zero version after a save")
	}

	rules, version, err = shared.Load(ctx)
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	if rules != "*.tmp\n" || version != saved {
		t.Errorf("expected the saved document at version %d, got %q at version %d",
			saved, rules, version)
	}
}

func TestSharedSkipAppliesSavedRules(t *testing.T) {
	shared, db := openShared(t)
	ctx := context.Background()
	addDevice(t, db, "mac", "mac")

	if _, err := shared.Save(ctx, ".git/\n"); err != nil {
		t.Fatalf("save: %v", err)
	}

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"mac/.git", true, true},
		{"mac/.git/HEAD", false, true},
		{"mac/notes.md", false, false},
		// The platform noise list applies whatever the document says, because
		// the document just replaced it.
		{"mac/.DS_Store", false, true},
		{".trash/20260101T000000.000_x", false, true},
	}

	for _, c := range cases {
		if got := shared.Skip(c.path, c.isDir); got != c.want {
			t.Errorf("Skip(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

// A rule naming a scope explicitly reads differently on the two sides: the
// server sees "mac/build", the device that syncs "mac" sees "build". Dropping
// it on the server's reading alone would tombstone a directory that device is
// still uploading, and the two would undo each other for as long as both ran.
func TestSharedKeepsWhatAScopedDeviceStillSyncs(t *testing.T) {
	shared, db := openShared(t)
	ctx := context.Background()

	if _, err := shared.Save(ctx, "mac/build/\n"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !shared.Skip("mac/build", true) {
		t.Error("with no scoped device, the rule is read from the root and matches")
	}

	addDevice(t, db, "mac", "mac")
	if err := shared.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if shared.Skip("mac/build", true) {
		t.Error("a path the device syncing that scope does not ignore must stay")
	}
}
