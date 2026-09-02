package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression: minute, hour, day of month,
// month, day of week.
//
// A person types this into the admin UI, so parsing is strict: a field that
// does not make sense is an error they can see and correct, not a value
// quietly dropped that leaves the job firing at a time nobody chose.
type Schedule struct {
	minutes  map[int]bool
	hours    map[int]bool
	days     map[int]bool
	months   map[int]bool
	weekdays map[int]bool

	// cron ORs day-of-month and day-of-week when both are restricted, and ANDs
	// them otherwise. "0 3 13 * 5" means the 13th *or* any Friday.
	domRestricted bool
	dowRestricted bool
}

// ParseSchedule reads a five-field expression. Ranges (1-5), lists (1,3,5),
// steps (*/15, 0-30/10) and * are supported; names like "MON" and the
// @daily shorthands are not.
func ParseSchedule(spec string) (Schedule, error) {
	fields := strings.Fields(strings.TrimSpace(spec))
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf(
			"expected 5 fields (minute hour day-of-month month day-of-week), got %d", len(fields))
	}

	var s Schedule
	var err error
	if s.minutes, err = parseField(fields[0], 0, 59, "minute"); err != nil {
		return Schedule{}, err
	}
	if s.hours, err = parseField(fields[1], 0, 23, "hour"); err != nil {
		return Schedule{}, err
	}
	if s.days, err = parseField(fields[2], 1, 31, "day of month"); err != nil {
		return Schedule{}, err
	}
	if s.months, err = parseField(fields[3], 1, 12, "month"); err != nil {
		return Schedule{}, err
	}

	// 0 and 7 are both Sunday, so the set is folded before it is used.
	raw, err := parseField(fields[4], 0, 7, "day of week")
	if err != nil {
		return Schedule{}, err
	}
	s.weekdays = make(map[int]bool, len(raw))
	for day := range raw {
		s.weekdays[day%7] = true
	}

	s.domRestricted = fields[2] != "*"
	s.dowRestricted = fields[4] != "*"
	return s, nil
}

func parseField(spec string, low, high int, name string) (map[int]bool, error) {
	values := make(map[int]bool)

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%s: empty value in %q", name, spec)
		}

		step := 1
		if base, rawStep, found := strings.Cut(part, "/"); found {
			parsed, err := strconv.Atoi(rawStep)
			if err != nil || parsed < 1 {
				return nil, fmt.Errorf("%s: %q is not a step", name, rawStep)
			}
			part, step = base, parsed
		}

		var start, end int
		switch {
		case part == "*":
			start, end = low, high
		case strings.Contains(part, "-"):
			rawStart, rawEnd, _ := strings.Cut(part, "-")
			var err error
			if start, err = strconv.Atoi(rawStart); err != nil {
				return nil, fmt.Errorf("%s: %q is not a number", name, rawStart)
			}
			if end, err = strconv.Atoi(rawEnd); err != nil {
				return nil, fmt.Errorf("%s: %q is not a number", name, rawEnd)
			}
			if start > end {
				return nil, fmt.Errorf("%s: range %q runs backwards", name, part)
			}
		default:
			value, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a number", name, part)
			}
			start, end = value, value
			// A single value with a step means "from here to the end of the
			// field", which is how cron reads "5/10".
			if step > 1 {
				end = high
			}
		}

		if start < low || end > high {
			return nil, fmt.Errorf("%s: %q is outside %d-%d", name, part, low, high)
		}
		for value := start; value <= end; value += step {
			values[value] = true
		}
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("%s: %q matches nothing", name, spec)
	}
	return values, nil
}

// Next returns the first minute strictly after `after` that the schedule
// matches, in after's location. The second result is false when nothing
// matches at all — "0 0 30 2 *", the 30th of February.
func (s Schedule) Next(after time.Time) (time.Time, bool) {
	// Truncate to the minute and step past it, so calling Next with a time
	// that matches returns the following occurrence rather than itself.
	moment := after.Truncate(time.Minute).Add(time.Minute)
	if moment.Before(after) { // Truncate works on UTC; guard the edge anyway
		moment = moment.Add(time.Minute)
	}

	// Five years, not one: 29 February is a legal schedule and can be four
	// years away, and a search that gave up after a year would report it as an
	// expression that never fires.
	limit := moment.AddDate(5, 0, 1)
	for moment.Before(limit) {
		if !s.months[int(moment.Month())] {
			// Skip to the first minute of the next month rather than walking
			// through every minute of one that cannot match.
			moment = time.Date(moment.Year(), moment.Month(), 1, 0, 0, 0, 0, moment.Location()).
				AddDate(0, 1, 0)
			continue
		}
		if !s.matchesDay(moment) {
			moment = time.Date(moment.Year(), moment.Month(), moment.Day(), 0, 0, 0, 0, moment.Location()).
				AddDate(0, 0, 1)
			continue
		}
		if !s.hours[moment.Hour()] {
			moment = moment.Truncate(time.Minute).Add(time.Hour - time.Duration(moment.Minute())*time.Minute)
			continue
		}
		if s.minutes[moment.Minute()] {
			return moment, true
		}
		moment = moment.Add(time.Minute)
	}
	return time.Time{}, false
}

func (s Schedule) matchesDay(moment time.Time) bool {
	dom := s.days[moment.Day()]
	dow := s.weekdays[int(moment.Weekday())]
	if s.domRestricted && s.dowRestricted {
		return dom || dow
	}
	return dom && dow
}
