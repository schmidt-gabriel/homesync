package backup

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseProgress(t *testing.T) {
	line := "      7,920,000  99%  537.28MB/s    0:00:00 (xfr#198, to-chk=2/201)"
	got, ok := parseProgress(line)
	if !ok {
		t.Fatalf("parseProgress did not recognise %q", line)
	}
	if got.Bytes != 7920000 {
		t.Errorf("Bytes = %d, want 7920000", got.Bytes)
	}
	// 201 files in the list, 2 still to check: 199 are done. Reading to-chk as
	// the count done rather than remaining would show 2 of 201 at the end of a
	// finished run.
	if got.FilesDone != 199 || got.FilesTotal != 201 {
		t.Errorf("files = %d/%d, want 199/201", got.FilesDone, got.FilesTotal)
	}
	// xfr#198: the files actually transferred, which is neither the checked
	// count nor the total and was not being read at all.
	if got.FilesCopied != 198 {
		t.Errorf("FilesCopied = %d, want 198", got.FilesCopied)
	}

	// Percent is not this function's to set: it is files checked against files
	// found, and the checked count is kept by the writer across lines.
	if got.Percent != 0 {
		t.Errorf("parseProgress set Percent to %d; the writer owns it", got.Percent)
	}
}

// The bar answers "how much is left", so it has to reach the end and it has to
// move on the way there. Files checked against files found is the only pair
// that does both.
//
// The two rejected candidates are asserted here as well, because each looked
// right in isolation and neither survives an incremental run: the copied count
// held at twelve for an entire measured pass over 9,000 files, and rsync's own
// check counter only advances on a line it prints when it transfers something.
func TestProgressPercentMeasuresWhatIsLeft(t *testing.T) {
	var last Progress
	w := &progressWriter{report: func(p Progress) { last = p }}

	// 200 files found, 20 copied, 100 checked so far.
	fmt.Fprint(w, " 20,000  10%  1MB/s 0:00:01 (xfr#20, to-chk=100/200)\r")
	for i := range 100 {
		fmt.Fprintf(w, "unchanged/file%03d.bin is uptodate\n", i)
	}

	if last.Percent != 50 {
		t.Errorf("Percent = %d, want 50 (100 checked of 200 found)", last.Percent)
	}
	// Had the bar been drawn from either of these it would read 10% and stay
	// there for the rest of the run.
	if got := percentDone(last.FilesCopied, last.FilesTotal); got != 10 {
		t.Fatalf("copied share = %d%%, expected the rejected 10%%", got)
	}
	if got := percentDone(last.FilesDone, last.FilesTotal); got != 50 {
		t.Fatalf("rsync check share = %d%%", got)
	}

	// Checking more files moves the bar; copying none does not stall it.
	for i := 100; i < 200; i++ {
		fmt.Fprintf(w, "unchanged/file%03d.bin is uptodate\n", i)
	}
	if last.Percent != 100 {
		t.Errorf("Percent = %d at the end of the file list, want 100", last.Percent)
	}
	if last.FilesCopied != 20 {
		t.Errorf("FilesCopied = %d, want 20 — nothing else was copied", last.FilesCopied)
	}
}

func TestParseProgressReadsIncrementalRecursion(t *testing.T) {
	line := "            3,000   0%    0.00kB/s    0:00:00 (xfr#1, ir-chk=1096/1159)"
	got, ok := parseProgress(line)
	if !ok {
		t.Fatalf("parseProgress did not recognise an ir-chk line: %q", line)
	}
	if got.FilesDone != 63 || got.FilesTotal != 1159 {
		t.Errorf("files = %d/%d, want 63/1159", got.FilesDone, got.FilesTotal)
	}
	if got.FilesCopied != 1 {
		t.Errorf("FilesCopied = %d, want 1", got.FilesCopied)
	}
	if !got.Scanning {
		t.Error("an ir-chk line was not marked as still scanning")
	}

	// And the complete-list form must not be marked as scanning, or the page
	// would never stop saying the total might grow.
	done, ok := parseProgress("      7,920,000  99%  537.28MB/s    0:00:00 (xfr#198, to-chk=2/201)")
	if !ok {
		t.Fatal("parseProgress stopped recognising to-chk")
	}
	if done.Scanning {
		t.Error("a to-chk line was marked as still scanning")
	}
}

func TestParseProgressIgnoresEverythingElse(t *testing.T) {
	for _, line := range []string{
		"",
		"Number of files: 12,345 (reg: 11,000, dir: 1,345)",
		"total size is 9,876,543,210  speedup is 7,597.34",
		"sent 1,300,000 bytes  received 9,001 bytes  120,000.00 bytes/sec",
		"rsync: [sender] opendir \"/backup-source/immich\" failed: Permission denied (13)",
	} {
		if _, ok := parseProgress(line); ok {
			t.Errorf("parseProgress claimed %q as progress", line)
		}
	}
}

func TestPercentDone(t *testing.T) {
	cases := []struct {
		done, total int64
		want        int
	}{
		{0, 0, 0}, // the first moments: no file list, so no division
		{5, 0, 0}, // a count with no total is not 500%
		{0, 10, 0},
		{199, 201, 99},
		{10, 10, 100},
		{11, 10, 100}, // never past 100, whatever rsync says
	}
	for _, c := range cases {
		if got := percentDone(c.done, c.total); got != c.want {
			t.Errorf("percentDone(%d, %d) = %d, want %d", c.done, c.total, got, c.want)
		}
	}
}

// rsync separates progress lines with carriage returns, because on a terminal
// each one overwrites the last. Reading the stream as newline-delimited sees a
// single enormous line and reports nothing until the run ends.
func TestProgressWriterSplitsOnCarriageReturns(t *testing.T) {
	var seen []Progress
	w := &progressWriter{report: func(p Progress) { seen = append(seen, p) }}

	stream := "    20,000  10%  1.00MB/s 0:00:01 (xfr#1, to-chk=9/10)\r" +
		"   100,000  50%  1.00MB/s 0:00:01 (xfr#5, to-chk=5/10)\r" +
		"   200,000 100%  1.00MB/s 0:00:02 (xfr#10, to-chk=0/10)\r\n" +
		"notes/one.txt\n" +
		"Number of files: 10 (reg: 10)\n" +
		"Total transferred file size: 200,000 bytes\n"

	// Written in awkward chunks: a real pipe splits wherever it likes, and a
	// parser that assumes each Write is a whole line loses lines that straddle
	// the boundary.
	for i := 0; i < len(stream); i += 7 {
		end := min(i+7, len(stream))
		if _, err := w.Write([]byte(stream[i:end])); err != nil {
			t.Fatal(err)
		}
	}

	// Distinct states, not update count: every line reports, and the ones that
	// are not progress carry the last transfer figures forward on purpose so
	// the bar holds its position instead of dropping to zero between files.
	states := map[string]bool{}
	for _, p := range seen {
		if p.FilesTotal > 0 {
			states[fmt.Sprintf("%d/%d %d", p.FilesDone, p.FilesTotal, p.Bytes)] = true
		}
	}
	want := []string{"1/10 20000", "5/10 100000", "10/10 200000"}
	if len(states) != len(want) {
		t.Fatalf("saw %d distinct transfer states %v, want %v", len(states), states, want)
	}
	for _, state := range want {
		if !states[state] {
			t.Errorf("missing transfer state %q", state)
		}
	}
	if last := seen[len(seen)-1]; last.FilesDone != 10 || last.FilesTotal != 10 {
		t.Errorf("last transfer state = %d/%d, want 10/10", last.FilesDone, last.FilesTotal)
	}

	// One real name, and nothing else. The stream contains "\r\n", which ends
	// a progress line and opens an empty one, and it ends with the --stats
	// block — neither is a file, and the bar is drawn from this number.
	if last := seen[len(seen)-1]; last.Seen != 1 {
		t.Errorf("Seen = %d, want 1 — an empty line or a stats line was counted as a file", last.Seen)
	}

	// The stats block has to survive: it is what the run's numbers come from,
	// and the progress lines must not be in the way.
	out := w.output()
	if strings.Contains(out, "to-chk") {
		t.Errorf("progress lines were kept for parseStats:\n%s", out)
	}
	stats := parseStats(out)
	if stats.FilesTotal != 10 || stats.TransferredSize != 200000 {
		t.Errorf("stats parsed from the kept output = %+v", stats)
	}
}

// The run that has no progress to report at all: --link-dest matched every
// file, so rsync transfers nothing and prints no progress line. Without the
// name lines being counted, the page would sit on "building the file list" for
// the entire pass and look identical to a hung process.
func TestProgressWriterCountsUnchangedFiles(t *testing.T) {
	var last Progress
	updates := 0
	w := &progressWriter{report: func(p Progress) { last = p; updates++ }}

	var stream strings.Builder
	stream.WriteString("created directory /backup/2026-01-02\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&stream, "photos/img%04d.jpg is uptodate\n", i)
	}
	stream.WriteString("Number of files: 2,001 (reg: 2,000, dir: 1)\n")
	stream.WriteString("Total transferred file size: 0 bytes\n")

	if _, err := w.Write([]byte(stream.String())); err != nil {
		t.Fatal(err)
	}

	if updates == 0 {
		t.Fatal("a run of unchanged files reported nothing at all")
	}
	// 2000 files and the "created directory" line. The --stats block that
	// follows is a summary, not more files: counting it pushed the checked
	// total past the found total in the final moment of a run, showing the two
	// numbers disagreeing exactly when someone is watching hardest.
	if last.Seen != 2001 {
		t.Errorf("Seen = %d, want 2001", last.Seen)
	}
	if last.Percent != 0 || last.FilesTotal != 0 {
		t.Errorf("a run with no transfers reported a fraction: %+v", last)
	}

	// And the stats block still has to be readable, from a tail that cannot
	// have held two thousand file names.
	stats := parseStats(w.output())
	if stats.FilesTotal != 2001 {
		t.Errorf("stats from the kept tail = %+v", stats)
	}
}

// The tail is bounded, or a tree with a million files is held in memory to
// find fifteen lines at the end of it.
func TestProgressWriterKeepsOnlyATail(t *testing.T) {
	w := &progressWriter{}
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(w, "some/quite/long/path/to/a/file-%06d.bin is uptodate\n", i)
	}
	// An absolute bound, deliberately not keptLines: comparing against the
	// constant makes the assertion move with it, so raising the constant to
	// hold the whole stream would still pass. The point is that the tail stays
	// small however much rsync prints.
	const smallEnough = 200
	if lines := strings.Count(w.output(), "\n"); lines > smallEnough {
		t.Fatalf("kept %d lines out of 5000, want at most %d", lines, smallEnough)
	}
}

// The denominator comes from the last run when rsync's own is still catching
// up. Measured on a 9,000-file tree, checked ran ahead of found for the whole
// incremental pass and the share sat between 87% and 100% from the first
// second — a bar that says "nearly done" before anything has happened tells
// nobody how much is left.
func TestProgressUsesThePreviousRunAsTheDenominator(t *testing.T) {
	var last Progress
	w := &progressWriter{expected: 9000, report: func(p Progress) { last = p }}

	// rsync has discovered 2,000 so far and already checked 2,100 of them.
	fmt.Fprint(w, " 1,000  0%  1MB/s 0:00:01 (xfr#3, ir-chk=100/2000)\r")
	for range 2100 {
		fmt.Fprint(w, "some/file.bin is uptodate\n")
	}

	if last.Percent != 23 {
		t.Errorf("Percent = %d, want 23 (2,100 checked of the 9,000 last seen)", last.Percent)
	}
	// Against rsync's live total this would already be full.
	if got := percentDone(last.Seen, last.FilesTotal); got != 100 {
		t.Fatalf("the live total gives %d%%, expected the rejected 100%%", got)
	}
}

// A tree that has grown since the last run must not finish the bar early and
// leave it full while rsync is still working.
func TestProgressDenominatorFollowsATreeThatGrew(t *testing.T) {
	var last Progress
	w := &progressWriter{expected: 100, report: func(p Progress) { last = p }}

	fmt.Fprint(w, " 1,000  0%  1MB/s 0:00:01 (xfr#1, to-chk=100/400)\r")
	for range 200 {
		fmt.Fprint(w, "some/file.bin is uptodate\n")
	}

	// 200 checked of the 400 rsync now reports, not of the 100 last time.
	if last.Percent != 50 {
		t.Errorf("Percent = %d, want 50 — the live total is larger and wins", last.Percent)
	}
}
