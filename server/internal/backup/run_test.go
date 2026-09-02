package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Taken from GNU rsync 3.x, which is what the container has. The thousands
// separators and the trailing "(reg: ...)" are the parts that used to be read
// as part of the number.
const gnuStats = `
Number of files: 12,345 (reg: 11,000, dir: 1,345)
Number of created files: 210 (reg: 200, dir: 10)
Number of deleted files: 7 (reg: 7)
Number of regular files transferred: 42
Total file size: 9,876,543,210 bytes
Total transferred file size: 1,234,567 bytes
Literal data: 1,200,000 bytes
Matched data: 34,567 bytes
File list size: 262,144
Total bytes sent: 1,300,000
Total bytes received: 9,001

sent 1,300,000 bytes  received 9,001 bytes  120,000.00 bytes/sec
total size is 9,876,543,210  speedup is 7,597.34
`

func TestParseStats(t *testing.T) {
	got := parseStats(gnuStats)
	want := Stats{
		FilesTotal:       12345,
		FilesCreated:     210,
		FilesDeleted:     7,
		FilesTransferred: 42,
		TotalSize:        9876543210,
		TransferredSize:  1234567,
		LiteralData:      1200000,
		BytesSent:        1300000,
		BytesReceived:    9001,
		Speedup:          7597.34,
	}
	if got != want {
		t.Errorf("parseStats:\n got %+v\nwant %+v", got, want)
	}
}

// A label rsync no longer prints must leave a zero behind, not fail the run:
// the numbers decorate the history, and openrsync on macOS already uses
// different ones.
func TestParseStatsToleratesMissingLabels(t *testing.T) {
	got := parseStats("Number of files: 2\nTotal file size: 3 B\n")
	if got.FilesTotal != 2 || got.TotalSize != 3 {
		t.Fatalf("parseStats read the labels it does know wrong: %+v", got)
	}
	if got.FilesCreated != 0 || got.Speedup != 0 {
		t.Fatalf("parseStats invented values for labels that were absent: %+v", got)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		c := DefaultConfig()
		c.SourceDir, c.BackupDir = "/data", "/backup"
		return c
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}

	cases := map[string]func(*Config){
		"same path":                 func(c *Config) { c.BackupDir = c.SourceDir },
		"destination inside source": func(c *Config) { c.BackupDir = "/data/backups" },
		"source inside destination": func(c *Config) { c.SourceDir = "/backup/live" },
		"relative source":           func(c *Config) { c.SourceDir = "data" },
		"root as destination":       func(c *Config) { c.BackupDir = "/" },
		"empty marker":              func(c *Config) { c.Marker = "  " },
		"marker with a path":        func(c *Config) { c.Marker = "sub/.marker" },
		"no dailies":                func(c *Config) { c.Daily = 0 },
		"negative weekly":           func(c *Config) { c.Weekly = -1 },
		"unparseable schedule":      func(c *Config) { c.Schedule = "every night" },
	}
	for name, break_ := range cases {
		cfg := valid()
		break_(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHealthProblem(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	cfg := DefaultConfig()
	cfg.SourceDir, cfg.BackupDir = source, dest

	if problem := checkHealth(cfg).Problem(cfg); !strings.Contains(problem, "does not exist") {
		t.Errorf("a missing source gave %q", problem)
	}

	mustMkdir(t, source)
	mustMkdir(t, dest)
	if problem := checkHealth(cfg).Problem(cfg); !strings.Contains(problem, "is empty") {
		t.Errorf("an empty source gave %q", problem)
	}

	mustWrite(t, filepath.Join(source, "a.txt"), "content")
	// The marker is the one check that stands between a run and filling the
	// root partition when the backup disk is not mounted.
	problem := checkHealth(cfg).Problem(cfg)
	if !strings.Contains(problem, cfg.Marker) {
		t.Errorf("a missing marker gave %q", problem)
	}

	mustWrite(t, filepath.Join(dest, cfg.Marker), "")
	if problem := checkHealth(cfg).Problem(cfg); problem != "" {
		t.Errorf("a healthy setup reported %q", problem)
	}
}

// The end of the whole feature: consecutive runs produce browsable snapshots
// that share unchanged content through hard links, and a file removed from the
// source disappears from the new snapshot while staying in the old one.
func TestExecuteTakesIncrementalSnapshots(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	mustMkdir(t, source)
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(dest, ".homesync_backup_disk"), "")

	mustWrite(t, filepath.Join(source, "stable.txt"), "never changes")
	mustWrite(t, filepath.Join(source, "volatile.txt"), "before")
	mustWrite(t, filepath.Join(source, "doomed.txt"), "delete me")

	cfg := DefaultConfig()
	cfg.SourceDir, cfg.BackupDir = source, dest
	cfg.Daily, cfg.Weekly, cfg.Monthly = 7, 4, 6

	day1 := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	first := execute(context.Background(), cfg, TriggerManual, day1)
	if first.Status != StatusSuccess {
		t.Fatalf("first run failed: %s", first.Error)
	}
	if first.Snapshot != "2026-01-01" {
		t.Fatalf("first snapshot is %q", first.Snapshot)
	}
	if got := readFile(t, filepath.Join(dest, "2026-01-01", "stable.txt")); got != "never changes" {
		t.Fatalf("snapshot content = %q", got)
	}

	mustWrite(t, filepath.Join(source, "volatile.txt"), "after")
	if err := os.Remove(filepath.Join(source, "doomed.txt")); err != nil {
		t.Fatal(err)
	}

	day2 := day1.AddDate(0, 0, 1)
	second := execute(context.Background(), cfg, TriggerSchedule, day2)
	if second.Status != StatusSuccess {
		t.Fatalf("second run failed: %s", second.Error)
	}

	// Unchanged: one inode, two names. This is what makes a month of daily
	// "full" backups fit on the disk.
	if !sameFile(t, filepath.Join(dest, "2026-01-01", "stable.txt"),
		filepath.Join(dest, "2026-01-02", "stable.txt")) {
		t.Error("an unchanged file was copied instead of hard-linked")
	}
	// Changed: a new inode, so the older snapshot keeps the older content.
	if sameFile(t, filepath.Join(dest, "2026-01-01", "volatile.txt"),
		filepath.Join(dest, "2026-01-02", "volatile.txt")) {
		t.Error("a changed file was hard-linked, which would rewrite history")
	}
	if got := readFile(t, filepath.Join(dest, "2026-01-01", "volatile.txt")); got != "before" {
		t.Errorf("the first snapshot now reads %q; it should still say \"before\"", got)
	}

	// Deletions propagate forward without rewriting the past.
	if _, err := os.Stat(filepath.Join(dest, "2026-01-02", "doomed.txt")); !os.IsNotExist(err) {
		t.Error("a file deleted from the source survived into the new snapshot")
	}
	if _, err := os.Stat(filepath.Join(dest, "2026-01-01", "doomed.txt")); err != nil {
		t.Error("a file deleted from the source vanished from the old snapshot too")
	}

	target, err := os.Readlink(filepath.Join(dest, "latest"))
	if err != nil {
		t.Fatalf("no 'latest' link: %v", err)
	}
	// Relative, so it resolves on the host as well as inside the container.
	if target != "2026-01-02" {
		t.Errorf("'latest' points at %q", target)
	}
}

// A run that starts while the backup disk is unmounted must write nothing at
// all. This is the failure the marker exists for: a bind mount resolves to the
// empty directory underneath the mountpoint, and the backup fills the root
// partition instead of the disk.
func TestExecuteRefusesWithoutTheMarker(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	mustMkdir(t, source)
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(source, "a.txt"), "content")

	cfg := DefaultConfig()
	cfg.SourceDir, cfg.BackupDir = source, dest

	run := execute(context.Background(), cfg, TriggerSchedule, time.Now())
	if run.Status != StatusFailed {
		t.Fatalf("the run reported %q with no marker present", run.Status)
	}
	if !strings.Contains(run.Error, cfg.Marker) {
		t.Errorf("the error does not name the marker: %s", run.Error)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused run wrote %d entries into the backup directory", len(entries))
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// sameFile reports whether two paths are the same inode — a hard link.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	infoA, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(infoA, infoB)
}
