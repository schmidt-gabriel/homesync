package backup

import (
	"bytes"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Progress is how far the run in flight has got. Only meaningful while one is
// running; the page shows it and nothing is stored.
type Progress struct {
	// FilesDone and FilesTotal come from rsync's to-chk counter, which is the
	// only honest fraction it offers. Total grows while rsync is still
	// building the file list, so early on the pair moves in both directions —
	// the page shows both numbers rather than a percentage alone so that reads
	// as "it found more work", not as a bar going backwards for no reason.
	FilesDone  int64 `json:"files_done"`
	FilesTotal int64 `json:"files_total"`

	// Bytes is the running total rsync reports. Deliberately not turned into a
	// percentage: rsync's own byte percentage is against an estimate, and with
	// --link-dest most files are skipped instantly, so it reaches 99% in the
	// first seconds of a run that has hours left.
	Bytes int64 `json:"bytes"`

	// Percent is the share of files done. A convenience for the page, which
	// still shows both counts: this number alone would hide that the total is
	// what moved.
	Percent int `json:"percent"`

	UpdatedAt int64 `json:"updated_at"` // unix milliseconds
}

// percentDone is the share of files handled, clamped to 0-100. A run whose
// file list is still empty is 0 rather than a division by zero, and rsync has
// been seen to report a to-chk total that briefly trails the count done.
func percentDone(done, total int64) int {
	if total <= 0 || done <= 0 {
		return 0
	}
	if done >= total {
		return 100
	}
	return int(100 * done / total)
}

// progressLine matches what --info=progress2 prints:
//
//	7,920,000  99%  537.28MB/s    0:00:00 (xfr#198, to-chk=2/201)
var progressLine = regexp.MustCompile(`^\s*([\d,]+)\s+\d+%.*to-chk=(\d+)/(\d+)`)

func parseProgress(line string) (Progress, bool) {
	match := progressLine.FindStringSubmatch(line)
	if match == nil {
		return Progress{}, false
	}
	bytesDone, err := strconv.ParseInt(strings.ReplaceAll(match[1], ",", ""), 10, 64)
	if err != nil {
		return Progress{}, false
	}
	remaining, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		return Progress{}, false
	}
	total, err := strconv.ParseInt(match[3], 10, 64)
	if err != nil {
		return Progress{}, false
	}
	return Progress{
		FilesDone:  total - remaining,
		FilesTotal: total,
		Bytes:      bytesDone,
		Percent:    percentDone(total-remaining, total),
		UpdatedAt:  time.Now().UnixMilli(),
	}, true
}

// progressWriter reads rsync's output as it arrives: progress lines go to the
// callback, everything else is kept for parseStats.
//
// Progress lines are separated by carriage returns, because they are meant to
// overwrite each other on a terminal, so this cannot be a bufio.Scanner over
// newlines. They are also thrown away rather than buffered: rsync prints one
// per file, and holding a million of them to find the dozen lines of --stats
// at the end would cost more memory than the backup does.
type progressWriter struct {
	kept    bytes.Buffer
	partial []byte
	report  func(Progress)
}

// maxPartial stops a stream with no line terminator at all from growing
// without limit. Nothing rsync prints comes close.
const maxPartial = 64 << 10

func (w *progressWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)

	for {
		i := bytes.IndexAny(w.partial, "\r\n")
		if i < 0 {
			break
		}
		w.consume(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}

	if len(w.partial) > maxPartial {
		w.consume(string(w.partial))
		w.partial = w.partial[:0]
	}
	return len(p), nil
}

func (w *progressWriter) consume(line string) {
	if progress, ok := parseProgress(line); ok {
		if w.report != nil {
			w.report(progress)
		}
		return
	}
	w.kept.WriteString(line)
	w.kept.WriteByte('\n')
}

// output is everything that was not a progress line, including whatever was
// left unterminated when the command exited.
func (w *progressWriter) output() string {
	if len(w.partial) > 0 {
		w.consume(string(w.partial))
		w.partial = w.partial[:0]
	}
	return w.kept.String()
}

// progressSupport is probed once. The check runs the exact flag the run will
// use rather than parsing a version string: Apple ships openrsync, which
// answers "rsync version 2.6.9 compatible" and then rejects --info, so
// anything derived from the version would be wrong on the machine it matters
// most on. A run there still works; it just cannot report how far it has got.
var progressSupport = sync.OnceValue(func() bool {
	// --version exits after parsing the options, so this costs a process and
	// no filesystem access.
	return exec.Command("rsync", "--info=progress2", "--version").Run() == nil
})
