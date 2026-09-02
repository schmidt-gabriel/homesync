package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}

	cases := map[string]func(*Config){
		"no dailies":           func(c *Config) { c.Daily = 0 },
		"negative weekly":      func(c *Config) { c.Weekly = -1 },
		"negative monthly":     func(c *Config) { c.Monthly = -1 },
		"unparseable schedule": func(c *Config) { c.Schedule = "every night" },
	}
	for name, breakIt := range cases {
		cfg := DefaultConfig()
		breakIt(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The defaults are the convention the compose file is written around: mount
// what to copy at /backup-source and the disk at /backup. /backup-source
// starts with /backup, so a containment check comparing prefixes without the
// separator would reject the out-of-the-box setup as "the source is inside the
// backup directory".
func TestDefaultPathsValidate(t *testing.T) {
	if err := DefaultPaths().Validate(); err != nil {
		t.Fatalf("the default paths do not validate: %v", err)
	}
}

func TestPathsValidate(t *testing.T) {
	cases := map[string]func(*Paths){
		"same path":                 func(p *Paths) { p.Dest = p.Source },
		"destination inside source": func(p *Paths) { p.Dest = "/backup-source/snapshots" },
		"source inside destination": func(p *Paths) { p.Source = "/backup/live" },
		"relative source":           func(p *Paths) { p.Source = "backup-source" },
		"root as destination":       func(p *Paths) { p.Dest = "/" },
		"empty marker":              func(p *Paths) { p.Marker = "  " },
		"marker with a path":        func(p *Paths) { p.Marker = "sub/.marker" },
	}
	for name, breakIt := range cases {
		paths := DefaultPaths()
		breakIt(&paths)
		if err := paths.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHealthProblem(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	paths := DefaultPaths()
	paths.Source, paths.Dest = source, dest

	if problem := checkHealth(paths).Problem(paths); !strings.Contains(problem, "does not exist") {
		t.Errorf("a missing source gave %q", problem)
	}

	mustMkdir(t, source)
	mustMkdir(t, dest)
	if problem := checkHealth(paths).Problem(paths); !strings.Contains(problem, "is empty") {
		t.Errorf("an empty source gave %q", problem)
	}

	mustWrite(t, filepath.Join(source, "a.txt"), "content")
	// The marker is the one check that stands between a run and filling the
	// root partition when the backup disk is not mounted.
	problem := checkHealth(paths).Problem(paths)
	if !strings.Contains(problem, paths.Marker) {
		t.Errorf("a missing marker gave %q", problem)
	}

	mustWrite(t, filepath.Join(dest, paths.Marker), "")
	if problem := checkHealth(paths).Problem(paths); problem != "" {
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

	paths := DefaultPaths()
	paths.Source, paths.Dest = source, dest
	cfg := DefaultConfig()

	day1 := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	first := execute(context.Background(), paths, cfg, TriggerManual, day1, nil)
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
	second := execute(context.Background(), paths, cfg, TriggerSchedule, day2, nil)
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

	paths := DefaultPaths()
	paths.Source, paths.Dest = source, dest

	run := execute(context.Background(), paths, DefaultConfig(), TriggerSchedule, time.Now(), nil)
	if run.Status != StatusFailed {
		t.Fatalf("the run reported %q with no marker present", run.Status)
	}
	if !strings.Contains(run.Error, paths.Marker) {
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

// rsync says what went wrong on the first line and repeats the exit code on
// the last. Reporting the last one gave an operator "errors selecting
// input/output files, dirs (code 3)" and no idea which file, or where.
func TestCauseTakesTheLineThatSaysWhy(t *testing.T) {
	stderr := `rsync: [Receiver] change_dir "/backup/2026-01-01" failed: No such file or directory (2)
rsync error: errors selecting input/output files, dirs (code 3) at main.c(768) [Receiver=3.5.0]
`
	got := cause(stderr)
	if !strings.Contains(got, "change_dir") {
		t.Errorf("cause = %q, which does not say what failed", got)
	}

	if got := cause("--link-dest arg does not exist: /backup/gone\n"); got != "--link-dest arg does not exist: /backup/gone" {
		t.Errorf("a single-line stderr came back as %q", got)
	}
	if got := cause("   \n\n"); got != "" {
		t.Errorf("empty stderr came back as %q", got)
	}
}

// The end of the reported bug: on a wide tree rsync recurses incrementally and
// reports ir-chk for most of the run, and a parser that only knew to-chk left
// the page saying "building the file list" while files were already being
// copied. Driving the real rsync through the real code path is the only way to
// see that phase — it depends on the shape of the tree, not on timing.
func TestExecuteReportsProgressWhileStillScanning(t *testing.T) {
	if !progressSupport() {
		t.Skip("this rsync cannot report progress (openrsync)")
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(dest, ".homesync_backup_disk"), "")

	// Wide and deep enough that rsync has not finished walking it before it
	// starts transferring. A flat directory is listed in one go and reports
	// to-chk from the first line, which is why this went unnoticed.
	for d := range 40 {
		sub := filepath.Join(source, fmt.Sprintf("dir%02d", d), "sub")
		mustMkdir(t, sub)
		for f := range 30 {
			mustWrite(t, filepath.Join(sub, fmt.Sprintf("f%02d.bin", f)), strings.Repeat("x", 2048))
		}
	}

	var mu sync.Mutex
	var scanning, copying, maxSeen int
	report := func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.FilesTotal > 0 {
			if p.Scanning {
				scanning++
			} else {
				copying++
			}
		}
		if int(p.Seen) > maxSeen {
			maxSeen = int(p.Seen)
		}
	}

	run := execute(context.Background(), Paths{Source: source, Dest: dest, Marker: ".homesync_backup_disk"},
		DefaultConfig(), TriggerManual, time.Now(), report)
	if run.Status != StatusSuccess {
		t.Fatalf("run failed: %s", run.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if scanning == 0 {
		t.Errorf("no progress was reported while rsync was still walking the tree; "+
			"the page shows nothing for that whole phase (copying updates: %d)", copying)
	}
	// And the heartbeat has to move too, for the runs that transfer nothing.
	if maxSeen == 0 {
		t.Error("no files were reported as seen")
	}
}

// Stopping a run must not leave what rsync had written behind. A half-copied
// directory sits in the snapshot list beside the complete ones, is counted by
// retention, and restores as though it were a backup.
func TestExecuteStoppedMidRunRemovesThePartialSnapshot(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(dest, ".homesync_backup_disk"), "")

	// Big enough that rsync is still working when the cancel lands.
	for d := range 30 {
		sub := filepath.Join(source, fmt.Sprintf("dir%02d", d))
		mustMkdir(t, sub)
		for f := range 200 {
			mustWrite(t, filepath.Join(sub, fmt.Sprintf("f%03d.bin", f)), strings.Repeat("x", 4096))
		}
	}

	paths := Paths{Source: source, Dest: dest, Marker: ".homesync_backup_disk"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	day := time.Date(2026, 5, 4, 3, 0, 0, 0, time.UTC)
	snapshot := filepath.Join(dest, day.Format(snapshotLayout))

	// Cancelled once the snapshot directory exists, which is the first moment
	// there is anything to clean up.
	//
	// Deliberately not driven from the progress callback. openrsync cannot
	// report progress, so on macOS the callback is never called at all — a
	// version of this test that cancelled from there let the run finish
	// untouched and then asserted nothing, passing while proving nothing.
	watching := make(chan struct{})
	defer close(watching)
	go func() {
		for {
			if _, err := os.Stat(snapshot); err == nil {
				cancel()
				return
			}
			select {
			case <-watching:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	run := execute(ctx, paths, DefaultConfig(), TriggerManual, day, nil)

	if run.Status != StatusCancelled {
		t.Fatalf("a stopped run reported %q (%s)", run.Status, run.Error)
	}
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Errorf("the partial snapshot was left on the disk: %v", err)
	}

	// The marker is not ours to remove, whatever happens.
	if _, err := os.Lstat(filepath.Join(dest, ".homesync_backup_disk")); err != nil {
		t.Errorf("the disk marker went with it: %v", err)
	}
}

// A snapshot that already finished must survive a cancelled re-run on the same
// day. Removing it because this run was stopped would throw away a good backup
// to clean up a directory this run never created.
func TestExecuteStoppedRunKeepsAnExistingSnapshot(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	mustMkdir(t, source)
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(source, "a.txt"), "content")
	mustWrite(t, filepath.Join(dest, ".homesync_backup_disk"), "")

	day := time.Date(2026, 5, 4, 3, 0, 0, 0, time.UTC)
	existing := filepath.Join(dest, "2026-05-04")
	mustMkdir(t, existing)
	mustWrite(t, filepath.Join(existing, "already-here.txt"), "yesterday's work")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already stopped before rsync can do anything

	run := execute(ctx, Paths{Source: source, Dest: dest, Marker: ".homesync_backup_disk"},
		DefaultConfig(), TriggerManual, day, nil)
	if run.Status != StatusCancelled {
		t.Fatalf("reported %q (%s)", run.Status, run.Error)
	}
	if _, err := os.Stat(filepath.Join(existing, "already-here.txt")); err != nil {
		t.Fatalf("a snapshot this run did not create was deleted: %v", err)
	}
}

// Cancelling is not failing. A streak that broke because someone stopped a
// run would be reporting something that did not happen.
func TestCancelledRunsDoNotBreakTheStreak(t *testing.T) {
	runs := []Run{
		{Status: StatusSuccess},
		{Status: StatusCancelled},
		{Status: StatusSuccess},
		{Status: StatusSkipped},
		{Status: StatusSuccess},
		{Status: StatusFailed},
		{Status: StatusSuccess},
	}
	m := buildMetrics(runs)
	if m.Streak != 3 {
		t.Errorf("streak = %d, want 3", m.Streak)
	}
	if m.Failures != 1 {
		t.Errorf("failures = %d, want 1", m.Failures)
	}
}

// checked has to converge on found: they count the same population, every file
// and directory rsync walks. This was reported from a running server showing
// "60,869 checked" beside "34,821 found" — the empty line that a carriage
// return leaves behind was being counted as a file, roughly doubling the
// tally, and the bar is drawn from that number.
func TestCheckedConvergesOnFilesFound(t *testing.T) {
	if !progressSupport() {
		t.Skip("this rsync cannot report progress (openrsync)")
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	dest := filepath.Join(dir, "backup")
	mustMkdir(t, dest)
	mustWrite(t, filepath.Join(dest, ".homesync_backup_disk"), "")

	for d := range 40 {
		sub := filepath.Join(source, fmt.Sprintf("dir%02d", d), "sub")
		mustMkdir(t, sub)
		for f := range 80 {
			mustWrite(t, filepath.Join(sub, fmt.Sprintf("f%02d.bin", f)), strings.Repeat("x", 1000))
		}
	}

	var mu sync.Mutex
	var last Progress
	run := execute(context.Background(),
		Paths{Source: source, Dest: dest, Marker: ".homesync_backup_disk"},
		DefaultConfig(), TriggerManual, time.Now(), func(p Progress) {
			mu.Lock()
			last = p
			mu.Unlock()
		})
	if run.Status != StatusSuccess {
		t.Fatalf("run failed: %s", run.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if last.FilesTotal == 0 {
		t.Fatal("no file total was ever reported")
	}
	ratio := float64(last.Seen) / float64(last.FilesTotal)
	if ratio > 1.1 || ratio < 0.9 {
		t.Errorf("checked=%d against found=%d is a ratio of %.2f; "+
			"they count the same files, so the bar drawn from the pair is wrong",
			last.Seen, last.FilesTotal, ratio)
	}
}
