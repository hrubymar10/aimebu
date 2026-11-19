// Package cron parses five-field cron expressions and finds their run times
// in a named time zone. It follows Vixie cron: `minute hour day-of-month month
// day-of-week` with `*`, lists, ranges, steps, month and day names and the
// `@hourly`-style macros; when both day fields are restricted, a day matches if
// either of them does.
//
// Times are civil (wall-clock) times in the zone. A local time a spring DST gap
// skips runs at the first valid minute after the gap; a local time an autumn
// overlap repeats runs once, at its first occurrence.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxExprLength bounds an expression so a stored value stays small.
const MaxExprLength = 200

// Spec is a parsed expression. The zero value matches nothing; get one from Parse.
type Spec struct {
	minute, hour, dom, month, dow bitset
	domStar, dowStar              bool
	expr                          string
}

type bitset uint64

func (b bitset) has(v int) bool { return b&(1<<uint(v)) != 0 }

// values lists the set values in order.
func (b bitset) values() []int {
	var out []int
	for v := 0; v < 64; v++ {
		if b.has(v) {
			out = append(out, v)
		}
	}
	return out
}

// Error names the field of an expression that cannot be used.
type Error struct {
	Field string // "minute", "hour", "day-of-month", "month", "day-of-week" or "expression"
	Msg   string
}

func (e *Error) Error() string { return "cron " + e.Field + ": " + e.Msg }

type fieldDef struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

	minuteField = fieldDef{"minute", 0, 59, nil}
	hourField   = fieldDef{"hour", 0, 23, nil}
	domField    = fieldDef{"day-of-month", 1, 31, nil}
	monthField  = fieldDef{"month", 1, 12, monthNames}
	// Day-of-week accepts 0-7: both 0 and 7 are Sunday.
	dowField = fieldDef{"day-of-week", 0, 7, dayNames}
)

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

// Parse reads an expression. The error is an *Error naming the bad field.
func Parse(expr string) (*Spec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, &Error{"expression", "is empty"}
	}
	if len(expr) > MaxExprLength {
		return nil, &Error{"expression", fmt.Sprintf("is longer than %d characters", MaxExprLength)}
	}
	if strings.HasPrefix(expr, "@") {
		full, ok := macros[strings.ToLower(expr)]
		if !ok {
			return nil, &Error{"expression", fmt.Sprintf("unknown macro %q (use @hourly, @daily, @weekly, @monthly or @yearly)", expr)}
		}
		spec, err := Parse(full)
		if spec != nil {
			spec.expr = expr
		}
		return spec, err
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, &Error{"expression", fmt.Sprintf("needs 5 fields (minute hour day-of-month month day-of-week), got %d", len(fields))}
	}
	spec := &Spec{expr: strings.Join(fields, " ")}
	var err error
	if spec.minute, err = parseField(fields[0], minuteField); err != nil {
		return nil, err
	}
	if spec.hour, err = parseField(fields[1], hourField); err != nil {
		return nil, err
	}
	if spec.dom, err = parseField(fields[2], domField); err != nil {
		return nil, err
	}
	if spec.month, err = parseField(fields[3], monthField); err != nil {
		return nil, err
	}
	if spec.dow, err = parseField(fields[4], dowField); err != nil {
		return nil, err
	}
	if spec.dow.has(7) {
		spec.dow = spec.dow&^(1<<7) | 1
	}
	spec.domStar = strings.HasPrefix(fields[2], "*")
	spec.dowStar = strings.HasPrefix(fields[4], "*")
	return spec, nil
}

// String is the expression as parsed, with its fields separated by one space.
func (s *Spec) String() string { return s.expr }

func parseField(text string, def fieldDef) (bitset, error) {
	fail := func(format string, args ...any) (bitset, error) {
		return 0, &Error{def.name, fmt.Sprintf(format, args...)}
	}
	var set bitset
	for _, part := range strings.Split(text, ",") {
		if part == "" {
			return fail("empty list item in %q", text)
		}
		rangePart, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return fail("bad step %q (a positive number is expected)", stepText)
			}
			step = n
		}
		lo, hi := def.min, def.max
		if def.name == "day-of-week" {
			hi = 6 // `*/2` runs Sunday, Tuesday, ...; 7 is only an alias for 0 in explicit values
		}
		if rangePart != "*" {
			loText, hiText, isRange := strings.Cut(rangePart, "-")
			var err error
			if lo, err = parseValue(loText, def); err != nil {
				return 0, err
			}
			switch {
			case isRange:
				if hi, err = parseValue(hiText, def); err != nil {
					return 0, err
				}
				if lo > hi {
					return fail("range %q runs backwards", rangePart)
				}
			case hasStep:
				hi = def.max // `5/15` means 5-max/15
			default:
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

func parseValue(text string, def fieldDef) (int, error) {
	if text == "" {
		return 0, &Error{def.name, "missing value"}
	}
	if v, ok := def.names[strings.ToLower(text)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || strings.HasPrefix(text, "+") {
		hint := ""
		if def.names != nil {
			hint = " or a name"
		}
		return 0, &Error{def.name, fmt.Sprintf("%q is not a number%s", text, hint)}
	}
	if n < def.min || n > def.max {
		return 0, &Error{def.name, fmt.Sprintf("%d is out of range %d-%d", n, def.min, def.max)}
	}
	return n, nil
}

func (s *Spec) dayMatches(t time.Time) bool {
	dom, dow := s.dom.has(t.Day()), s.dow.has(int(t.Weekday()))
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// Next returns the first run strictly after `after`, in loc, as a UTC instant.
// ok is false when the expression never matches within eight years (for
// example February 30).
func (s *Spec) Next(after time.Time, loc *time.Location) (next time.Time, ok bool) {
	if loc == nil {
		loc = time.UTC
	}
	minutes, hours := s.minute.values(), s.hour.values()
	if len(minutes) == 0 || len(hours) == 0 {
		return time.Time{}, false
	}
	local := after.In(loc)
	y, m, d := local.Date()
	// Start one day early: a gap can move a time forward, never backward, but
	// the day of `after` in loc and the day of the candidate may differ.
	day := time.Date(y, m, d-1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 366*8+2; i, day = i+1, day.AddDate(0, 0, 1) {
		if !s.month.has(int(day.Month())) || !s.dayMatches(day) {
			continue
		}
		for _, h := range hours {
			for _, mi := range minutes {
				at := civilInstant(day.Year(), day.Month(), day.Day(), h, mi, loc)
				if at.After(after) {
					return at.UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}

// Upcoming lists the next n runs after `after`.
func (s *Spec) Upcoming(after time.Time, loc *time.Location, n int) []time.Time {
	var out []time.Time
	for len(out) < n {
		next, ok := s.Next(after, loc)
		if !ok {
			break
		}
		out = append(out, next)
		after = next
	}
	return out
}

// civilInstant maps a wall-clock time in loc to an instant. In an overlap it is
// the first occurrence; in a gap it is the first instant after the gap.
func civilInstant(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	wall := time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	// The offsets in force a day either side cover the offsets on this date.
	_, before := wall.Add(-26 * time.Hour).In(loc).Zone()
	_, after := wall.Add(26 * time.Hour).In(loc).Zone()
	_, here := time.Date(y, mo, d, h, mi, 0, 0, loc).Zone()
	var best time.Time
	for _, off := range []int{before, after, here} {
		candidate := wall.Add(-time.Duration(off) * time.Second)
		c := candidate.In(loc)
		if c.Year() != y || c.Month() != mo || c.Day() != d || c.Hour() != h || c.Minute() != mi {
			continue
		}
		if best.IsZero() || candidate.Before(best) {
			best = candidate
		}
	}
	if !best.IsZero() {
		return best
	}
	// The time does not exist: use the end of the zone period that precedes it.
	early := wall.Add(-time.Duration(max(before, after)) * time.Second)
	if _, end := early.In(loc).ZoneBounds(); !end.IsZero() {
		return end
	}
	return early
}
