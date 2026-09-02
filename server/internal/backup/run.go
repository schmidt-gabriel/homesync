package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runTimeout stops a run that has stopped making progress from holding the
// schedule forever. A first backup of a large tree is measured in hours, so it
// is deliberately generous.
const runTimeout = 24 * time.Hour

// rsync exit codes worth telling apart.
const (
	// exitPartial is "partial transfer due to error", which in practice is
	// almost always a file the process could not read.
	exitPartial = 23
	// exitVanished is "some files vanished before they could be transferred",
	// which is normal when copying a directory that is in use. The snapshot is
	// complete apart from those files, so the run is a success that says so.
	exitVanished = 24
)

// DiskUsage describes the filesystem holding the backups.
type DiskUsage struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
	Used  int64 `json:"used"`
}

// Health is what a run checks before it starts, and what the admin page shows
// so the answer is visible before 03:00 rather than after.
type Health struct {
	RsyncAvailable bool `json:"rsync_available"`
	SourceExists   bool `json:"source_exists"`
	SourceReadable bool `json:"source_readable"`
	SourceEmpty    bool `json:"source_empty"`
	// ProgressSupported is false where rsync cannot report how far it has got
	// — openrsync, which macOS ships. The run works; only the bar is missing,
	// and the page says which it is rather than showing one that never moves.
	ProgressSupported bool `json:"progress_supported"`
	// SourceMountpoint is reported, never enforced. The source is normally a
	// bind mount, which is always a mountpoint inside the container whatever
	// the host is doing — so it proves nothing, and requiring it would rule
	// out backing up a subdirectory.
	SourceMountpoint bool       `json:"source_mountpoint"`
	BackupDirExists  bool       `json:"backup_dir_exists"`
	MarkerPresent    bool       `json:"marker_present"`
	Disk             *DiskUsage `json:"disk"`
	Latest           string     `json:"latest"`
}

// Problem returns the reason a run would refuse to start, or "" when it would
// go ahead.
func (h Health) Problem(paths Paths) string {
	switch {
	case !h.RsyncAvailable:
		return "rsync is not installed in this image"
	case !h.SourceExists:
		return fmt.Sprintf("the source %s does not exist — check the volume mount", paths.Source)
	case !h.SourceReadable:
		return fmt.Sprintf("the source %s cannot be read — the server runs unprivileged, "+
			"so a directory owned by another user needs the container to run as root", paths.Source)
	case h.SourceEmpty:
		// A snapshot of an empty source would become "latest", and tomorrow's
		// --link-dest would then have nothing to link against: a source that
		// disappeared would quietly cost a full copy and hide the problem.
		return fmt.Sprintf("the source %s is empty — refusing to snapshot nothing", paths.Source)
	case !h.BackupDirExists:
		return fmt.Sprintf("the backup directory %s does not exist — check the volume mount", paths.Dest)
	case !h.MarkerPresent:
		return fmt.Sprintf("the marker %q is missing from %s, so the backup disk is probably not "+
			"mounted; create it once with the disk mounted: touch %s",
			paths.Marker, paths.Dest, filepath.Join(paths.Dest, paths.Marker))
	}
	return ""
}

// checkHealth answers every question a run asks, without changing anything.
func checkHealth(paths Paths) Health {
	paths = paths.normalised()
	var h Health

	if _, err := exec.LookPath("rsync"); err == nil {
		h.RsyncAvailable = true
		h.ProgressSupported = progressSupport()
	}

	if info, err := os.Stat(paths.Source); err == nil && info.IsDir() {
		h.SourceExists = true
		h.SourceMountpoint = isMountpoint(paths.Source)
		// Opening the directory is the cheap half of the question. Whether
		// every file below it is readable only rsync can say, and it says so
		// with exit code 23.
		if entries, err := os.ReadDir(paths.Source); err == nil {
			h.SourceReadable = true
			h.SourceEmpty = len(entries) == 0
		}
	}

	if info, err := os.Stat(paths.Dest); err == nil && info.IsDir() {
		h.BackupDirExists = true
		if _, err := os.Lstat(filepath.Join(paths.Dest, paths.Marker)); err == nil {
			h.MarkerPresent = true
		}
		h.Disk = diskUsage(paths.Dest)
		if target, err := os.Readlink(filepath.Join(paths.Dest, "latest")); err == nil {
			h.Latest = target
		}
	}

	return h
}

// isMountpoint compares a directory's device with its parent's, which is what
// makes a mountpoint one.
func isMountpoint(dir string) bool {
	var self, parent syscall.Stat_t
	if err := syscall.Stat(dir, &self); err != nil {
		return false
	}
	if err := syscall.Stat(filepath.Dir(dir), &parent); err != nil {
		return false
	}
	// The second test catches "/", whose parent is itself.
	return uint64(self.Dev) != uint64(parent.Dev) || self.Ino == parent.Ino
}

func diskUsage(path string) *DiskUsage {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	size := uint64(st.Bsize)
	total := int64(st.Blocks * size)
	// Bavail, not Bfree: the reserved blocks are not ours to write into, so
	// counting them as free would promise room that does not exist.
	free := int64(uint64(st.Bavail) * size)
	return &DiskUsage{Total: total, Free: free, Used: total - int64(uint64(st.Bfree)*size)}
}

// execute takes one snapshot and applies retention. It assumes nothing about
// the caller beyond a validated config, and returns the Run to record whether
// it succeeded or not.
// expectedFiles is how many files the last successful run walked, or zero when
// there has not been one. It is only ever an estimate for the progress bar.
func execute(ctx context.Context, paths Paths, cfg Config, trigger string, now time.Time,
	expectedFiles int64, report func(Progress)) Run {
	paths, cfg = paths.normalised(), cfg.normalised()
	started := now
	run := Run{
		StartedAt: started.UnixMilli(),
		Trigger:   trigger,
		Pruned:    []string{},
	}

	finish := func(status, message string) Run {
		finished := time.Now()
		run.Status = status
		run.Error = message
		run.FinishedAt = finished.UnixMilli()
		run.DurationMS = finished.Sub(started).Milliseconds()
		return run
	}

	if problem := checkHealth(paths).Problem(paths); problem != "" {
		return finish(StatusFailed, problem)
	}

	name := started.Format(snapshotLayout)
	run.Snapshot = name
	dest := filepath.Join(paths.Dest, name)

	// Whether this run is the one creating the directory decides whether it may
	// clean it up. A run stopped halfway leaves a partial copy that would sit
	// in the list looking like a complete backup, so it is removed — but only
	// if this run made it. A second run on the same day writes into a snapshot
	// that already succeeded, and deleting that would throw away a good backup
	// because someone cancelled a redundant re-run.
	_, statErr := os.Lstat(dest)
	createdHere := errors.Is(statErr, os.ErrNotExist)

	snapshots, err := ListSnapshots(paths.Dest)
	if err != nil {
		return finish(StatusFailed, "cannot read the backup directory: "+err.Error())
	}

	args := []string{"-a", "--delete", "--partial", "--stats"}
	// progress2 gives a running total and a to-chk counter, which is what the
	// page draws, and outbuf=L flushes it per line — without it rsync buffers
	// when its output is a pipe rather than a terminal, and the whole run
	// arrives at once, after it is over. Added only where rsync understands
	// them: openrsync, which is what macOS ships, fails the run outright on an
	// option it does not know.
	if report != nil && progressSupport() {
		// name2 as well as progress2, because progress2 only speaks when bytes
		// move. A run whose --link-dest matches everything transfers nothing
		// and prints a single line for the whole pass, so a page watching only
		// progress2 cannot tell a scan of a million files from a hung process.
		// name2 mentions unchanged files too, one line each, which is the
		// heartbeat. The lines are counted and dropped, never buffered.
		args = append(args, "--info=progress2,name2", "--outbuf=L")
	}
	// The newest snapshot that is not the one being written. Deliberately not
	// the "latest" symlink: a second run on the same day would find it already
	// pointing at today's directory and hard-link the snapshot against itself.
	if previous := previousSnapshot(snapshots, name); previous != "" {
		args = append(args, "--link-dest="+filepath.Join(paths.Dest, previous))
	}
	// The trailing slash is load-bearing: it copies the contents of the source
	// rather than the source directory itself.
	args = append(args, paths.Source+string(filepath.Separator), dest)

	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "rsync", args...)
	stdout := &progressWriter{report: report, expected: expectedFiles}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	stats := parseStats(stdout.output())
	run.Stats = &stats

	if err != nil {
		var exit *exec.ExitError
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return finish(StatusFailed, fmt.Sprintf("rsync did not finish within %s", runTimeout))
		case errors.Is(runCtx.Err(), context.Canceled):
			// Stopped on purpose, by a person or by shutdown. Either way the
			// snapshot is half-written and must not be left in the list looking
			// finished.
			if createdHere {
				if err := os.RemoveAll(dest); err != nil {
					run.Warning = "the partial snapshot could not be removed: " + err.Error()
				}
			}
			return finish(StatusCancelled, "stopped before it finished")
		case errors.As(err, &exit) && exit.ExitCode() == exitVanished:
			// Not a failure: files disappeared underneath a live directory.
			run.Warning = "some files vanished while being copied; the snapshot is complete apart from those"
		case errors.As(err, &exit) && exit.ExitCode() == exitPartial:
			return finish(StatusFailed, "rsync could not read part of the source (exit 23). "+
				"The server runs unprivileged, so files owned by another user are unreadable "+
				"unless the container runs as root. "+cause(stderr.String()))
		default:
			return finish(StatusFailed, fmt.Sprintf("rsync failed: %v. %s", err, cause(stderr.String())))
		}
	}

	// Relative, so the link resolves both inside the container and on the host
	// where the disk is actually mounted.
	link := filepath.Join(paths.Dest, "latest")
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		run.Warning = appendWarning(run.Warning, "could not replace the 'latest' link: "+err.Error())
	} else if err := os.Symlink(name, link); err != nil {
		run.Warning = appendWarning(run.Warning, "could not create the 'latest' link: "+err.Error())
	}

	// Re-read: the snapshot just written has to be part of what retention
	// counts, or the run would prune as though it had one fewer daily.
	snapshots, err = ListSnapshots(paths.Dest)
	if err != nil {
		run.Warning = appendWarning(run.Warning, "could not apply retention: "+err.Error())
		return finish(StatusSuccess, "")
	}
	removed, err := prune(paths.Dest, Classify(snapshots, cfg))
	run.Pruned = removed
	if run.Pruned == nil {
		run.Pruned = []string{}
	}
	if err != nil {
		// The snapshot is on disk and correct; only the thinning failed.
		run.Warning = appendWarning(run.Warning, "retention did not finish: "+err.Error())
	}

	return finish(StatusSuccess, "")
}

// previousSnapshot returns the newest snapshot older than name, or "".
func previousSnapshot(snapshots []Snapshot, name string) string {
	for _, snapshot := range snapshots {
		if snapshot.Name < name {
			return snapshot.Name
		}
	}
	return ""
}

func appendWarning(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

// cause is the first line of rsync's stderr, which is where it says what
// actually went wrong. The last line is a summary that only repeats the exit
// code — "rsync error: errors selecting input/output files, dirs (code 3)" —
// and reporting that instead, as this did, told an operator nothing they could
// act on. The line above it is the one that names the file or the path.
func cause(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			// Long enough for a path, short enough to sit in a table cell.
			if len(line) > 300 {
				return line[:300] + "…"
			}
			return line
		}
	}
	return ""
}

// parseStats reads the summary rsync prints for --stats. Anything it cannot
// find stays zero: the numbers decorate the history, and a run is not a
// failure because a label changed between rsync versions.
func parseStats(output string) Stats {
	var s Stats
	fields := map[string]*int64{
		"Number of files:":                     &s.FilesTotal,
		"Number of created files:":             &s.FilesCreated,
		"Number of deleted files:":             &s.FilesDeleted,
		"Number of regular files transferred:": &s.FilesTransferred,
		"Total file size:":                     &s.TotalSize,
		"Total transferred file size:":         &s.TransferredSize,
		"Literal data:":                        &s.LiteralData,
		"Total bytes sent:":                    &s.BytesSent,
		"Total bytes received:":                &s.BytesReceived,
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		for label, target := range fields {
			if !strings.HasPrefix(line, label) {
				continue
			}
			// "Number of files: 1,234 (reg: 1,000, dir: 234)" — the first
			// number after the colon, without its thousands separators.
			rest := strings.TrimSpace(strings.TrimPrefix(line, label))
			if value, ok := firstNumber(rest); ok {
				*target = value
			}
			break
		}
		if _, after, found := strings.Cut(line, "speedup is "); found {
			if parts := strings.Fields(strings.ReplaceAll(after, ",", "")); len(parts) > 0 {
				if value, err := strconv.ParseFloat(parts[0], 64); err == nil {
					s.Speedup = value
				}
			}
		}
	}
	return s
}

func firstNumber(text string) (int64, bool) {
	var digits strings.Builder
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ',': // thousands separator
			continue
		default:
			// Stop at the first non-number, so "1,234 (reg: 1,000" gives 1234.
			if digits.Len() > 0 {
				value, err := strconv.ParseInt(digits.String(), 10, 64)
				return value, err == nil
			}
			return 0, false
		}
	}
	if digits.Len() == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(digits.String(), 10, 64)
	return value, err == nil
}
