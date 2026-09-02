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

	// Percent is Seen against FilesTotal: files checked out of files found.
	// Deliberately not FilesDone against FilesTotal — rsync only prints a
	// progress line when it transfers something, so that pair holds still
	// between transfers and freezes the bar for as long as a stretch of
	// unchanged files takes. Seen moves on every file.
	//
	// A convenience for the page, which shows the counts too: this number
	// alone would hide that the denominator is what moved.
	Percent int `json:"percent"`

	// Seen counts every file rsync has said anything about, transferred or
	// not. It exists because progress2 only reports transfers: a run where
	// --link-dest matches everything moves no bytes and prints one line for
	// the whole pass, so the fields above stay empty from start to finish and
	// a page with only those cannot tell a scan of a million files from a
	// hung process. This one always moves.
	Seen int64 `json:"seen"`

	// Scanning is true while rsync is still walking the tree, so FilesTotal is
	// what it has found rather than what there is.
	Scanning bool `json:"scanning"`

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
//	        0   0%    0.00kB/s    0:00:00 (xfr#0, ir-chk=1036/1098)
//
// Both spellings, and the second one is the one that matters. rsync recurses
// incrementally: until it has walked the whole tree it says ir-chk and counts
// against what it has found so far, switching to to-chk once the list is
// complete. On a wide tree that is most of the run — a 60x60 directory sample
// gave 2572 ir-chk lines to 1149 to-chk — so matching only to-chk left the
// page reporting "building the file list" through the entire early phase,
// while rsync was already copying.
var progressLine = regexp.MustCompile(`^\s*([\d,]+)\s+\d+%.*\b(ir|to)-chk=(\d+)/(\d+)`)

func parseProgress(line string) (Progress, bool) {
	match := progressLine.FindStringSubmatch(line)
	if match == nil {
		return Progress{}, false
	}
	bytesDone, err := strconv.ParseInt(strings.ReplaceAll(match[1], ",", ""), 10, 64)
	if err != nil {
		return Progress{}, false
	}
	remaining, err := strconv.ParseInt(match[3], 10, 64)
	if err != nil {
		return Progress{}, false
	}
	total, err := strconv.ParseInt(match[4], 10, 64)
	if err != nil {
		return Progress{}, false
	}
	// Percent is left to the caller: it is Seen against FilesTotal, and Seen
	// is counted by the writer rather than reported on this line.
	return Progress{
		FilesDone:  total - remaining,
		FilesTotal: total,
		Bytes:      bytesDone,
		// ir-chk means the total is only what rsync has found so far and will
		// keep climbing. The fraction is real but provisional, and the page
		// says so rather than presenting it as a countdown to the end.
		Scanning:  match[2] == "ir",
		UpdatedAt: time.Now().UnixMilli(),
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
	// kept is a ring of the most recent lines, not everything: --info=name2
	// prints one line per file, and a tree with a million of them would
	// otherwise be held in memory to find the dozen lines of --stats at the
	// end. rsync prints that block last, so the tail is all it can be in.
	kept    []string
	next    int
	full    bool
	partial []byte
	seen    int64
	// statsStarted stops the count once rsync begins its --stats block. Those
	// lines are not files, and counting them pushed the checked total past the
	// found total in the last moment of a run — the bar reaching 100% and the
	// two numbers disagreeing, both at the point someone is looking hardest.
	statsStarted bool
	last         Progress
	report       func(Progress)
}

// keptLines is the tail held for parseStats. The block is about fifteen lines;
// this leaves room for a version that prints more without holding a file list.
const keptLines = 64

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
		progress.Seen = w.seen
		progress.Percent = percentDone(w.seen, progress.FilesTotal)
		w.last = progress
		if w.report != nil {
			w.report(progress)
		}
		return
	}

	// A carriage return followed by a newline ends one progress line and opens
	// an empty one. Counting that as a file inflates the tally and, now that
	// the bar is drawn from it, moves the bar for something that is not there.
	if line == "" {
		return
	}

	// The --stats block is always last and always opens with this line, so
	// everything from here on is a summary rather than a file.
	if strings.HasPrefix(line, "Number of files:") {
		w.statsStarted = true
	}
	if w.statsStarted {
		w.keep(line)
		return
	}

	// Anything else is a file rsync mentioned. The --stats block at the end is
	// a handful of lines counted the same way, which would matter if the count
	// were shown afterwards — it is only shown while the run is going, and by
	// the time that block prints there is nothing left to show.
	w.seen++
	if w.report != nil {
		// The transfer figures are carried forward and only the count moves,
		// which is the point: between two transferred files rsync says nothing
		// about progress, and this is what keeps the bar advancing through a
		// long stretch of unchanged ones.
		next := w.last
		next.Seen = w.seen
		next.Percent = percentDone(w.seen, next.FilesTotal)
		next.UpdatedAt = time.Now().UnixMilli()
		w.report(next)
	}

	w.keep(line)
}

func (w *progressWriter) keep(line string) {
	if w.kept == nil {
		w.kept = make([]string, keptLines)
	}
	w.kept[w.next] = line
	w.next = (w.next + 1) % keptLines
	if w.next == 0 {
		w.full = true
	}
}

// output is the tail of everything that was not a progress line, including
// whatever was left unterminated when the command exited.
func (w *progressWriter) output() string {
	if len(w.partial) > 0 {
		w.consume(string(w.partial))
		w.partial = w.partial[:0]
	}
	if w.kept == nil {
		return ""
	}

	var out strings.Builder
	if w.full {
		for i := w.next; i < keptLines; i++ {
			out.WriteString(w.kept[i])
			out.WriteByte('\n')
		}
	}
	for i := 0; i < w.next; i++ {
		out.WriteString(w.kept[i])
		out.WriteByte('\n')
	}
	return out.String()
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
