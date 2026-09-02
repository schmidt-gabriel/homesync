package backup

import (
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
	if got.Percent != 99 {
		t.Errorf("Percent = %d, want 99", got.Percent)
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

	if len(seen) != 3 {
		t.Fatalf("saw %d progress updates, want 3", len(seen))
	}
	if last := seen[len(seen)-1]; last.FilesDone != 10 || last.Percent != 100 {
		t.Errorf("last update = %d/%d (%d%%), want 10/10 (100%%)",
			last.FilesDone, last.FilesTotal, last.Percent)
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
