package backup

import (
	"testing"
	"time"
)

func TestParseScheduleRejects(t *testing.T) {
	// Every one of these is something a person can type into the admin UI, and
	// each used to be accepted by silently dropping the field it appeared in —
	// which leaves the job firing at a time nobody chose.
	for _, spec := range []string{
		"",
		"0 3 * *",       // four fields
		"0 3 * * * *",   // six
		"60 3 * * *",    // minute out of range
		"0 24 * * *",    // hour out of range
		"0 3 0 * *",     // day of month starts at 1
		"0 3 * 13 *",    // month out of range
		"0 3 * * 8",     // weekday out of range
		"0 3 * * MON",   // names are not supported
		"@daily",        // shorthands are not supported
		"0 3 * * ",      // trailing field missing
		"0 3 5-1 * *",   // backwards range
		"0 3 * * *,",    // empty list entry
		"*/0 3 * * *",   // zero step
		"0 3 * * */abc", // step is not a number
	} {
		if _, err := ParseSchedule(spec); err == nil {
			t.Errorf("ParseSchedule(%q) accepted an expression it should refuse", spec)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	// A Wednesday, so the weekday cases have somewhere to move to.
	from := time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC)

	cases := []struct {
		spec string
		want time.Time
	}{
		// The default: 03:00 tomorrow, since today's has passed.
		{"0 3 * * *", time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 9, 2, 10, 45, 0, 0, time.UTC)},
		{"0 * * * *", time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)},
		{"30 10 * * *", time.Date(2026, 9, 3, 10, 30, 0, 0, time.UTC)}, // now, so: tomorrow
		{"0 0 1 * *", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"0 4 * * 0", time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)},   // next Sunday
		{"0 4 * * 7", time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)},   // 7 is Sunday too
		{"0 2 29 2 *", time.Date(2028, 2, 29, 2, 0, 0, 0, time.UTC)}, // next leap year
		// Both day fields restricted: cron ORs them, so this is the 10th or
		// any Friday — Friday the 4th comes first.
		{"0 5 10 * 5", time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)},
	}

	for _, c := range cases {
		schedule, err := ParseSchedule(c.spec)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", c.spec, err)
		}
		got, ok := schedule.Next(from)
		if !ok {
			t.Errorf("Next(%q) found nothing", c.spec)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("Next(%q) = %s, want %s", c.spec, got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
	}
}

// A schedule must never return the moment it was handed, or the scheduler
// would fire in a tight loop: it computes the next run from the moment the
// last one finished.
func TestScheduleNextIsStrictlyAfter(t *testing.T) {
	schedule, err := ParseSchedule("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	exactly := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	got, ok := schedule.Next(exactly)
	if !ok {
		t.Fatal("Next found nothing")
	}
	if !got.After(exactly) {
		t.Fatalf("Next(%s) = %s, which is not later", exactly, got)
	}
}

func TestScheduleNeverFires(t *testing.T) {
	// The 30th of February: parses, matches nothing, and the scheduler has to
	// idle rather than spin looking for it.
	schedule, err := ParseSchedule("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := schedule.Next(time.Now()); ok {
		t.Fatal("Next found an occurrence of 30 February")
	}
}
