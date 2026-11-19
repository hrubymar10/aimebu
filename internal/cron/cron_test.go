package cron

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestParseErrorsNameTheField(t *testing.T) {
	for _, tc := range []struct{ expr, field, contains string }{
		{"", "expression", "empty"},
		{"* * * *", "expression", "needs 5 fields"},
		{"* * * * * *", "expression", "needs 5 fields"},
		{"@reboot", "expression", "unknown macro"},
		{"60 * * * *", "minute", "out of range 0-59"},
		{"-1 * * * *", "minute", "missing value"},
		{"+1 * * * *", "minute", "not a number"},
		{"* 24 * * *", "hour", "out of range 0-23"},
		{"* * 0 * *", "day-of-month", "out of range 1-31"},
		{"* * 32 * *", "day-of-month", "out of range 1-31"},
		{"* * * 13 *", "month", "out of range 1-12"},
		{"* * * foo *", "month", "not a number or a name"},
		{"* * * * 8", "day-of-week", "out of range 0-7"},
		{"* * * * funday", "day-of-week", "not a number or a name"},
		{"*/0 * * * *", "minute", "bad step"},
		{"*/x * * * *", "minute", "bad step"},
		{"5-1 * * * *", "minute", "backwards"},
		{"1,,2 * * * *", "minute", "empty list item"},
		{"1- * * * *", "minute", "missing value"},
		{"* * * mar-jan *", "month", "backwards"},
		{"0 8 * * 1-", "day-of-week", "missing value"},
		{strings.Repeat("*", MaxExprLength+1), "expression", "longer"},
	} {
		_, err := Parse(tc.expr)
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("%q: error %v, want *Error", tc.expr, err)
			continue
		}
		if ce.Field != tc.field || !strings.Contains(ce.Error(), tc.contains) {
			t.Errorf("%q: error %q, want field %q containing %q", tc.expr, ce, tc.field, tc.contains)
		}
	}
}

func TestParseAcceptsTheSyntax(t *testing.T) {
	for _, expr := range []string{
		"* * * * *", "0 8,12,16,20 * * *", "*/15 9-17 * * mon-fri", "0 0 1,15 jan,JUL *", "5/20 * * * *", "0 0 * * 7", "0 0 * * 5-7",
		"@hourly", "@Daily", "@weekly", "@monthly", "@yearly", "@annually", "@midnight", "  0   8  *  *  *  ", "0-30/10 * * * *",
	} {
		if _, err := Parse(expr); err != nil {
			t.Errorf("%q: %v", expr, err)
		}
	}
}

func TestParsedSets(t *testing.T) {
	spec, err := Parse("5/20 */6 1-3,31 feb,dec 0,7,tue-thu")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		got  bitset
		want []int
	}{
		"minute": {spec.minute, []int{5, 25, 45}},
		"hour":   {spec.hour, []int{0, 6, 12, 18}},
		"dom":    {spec.dom, []int{1, 2, 3, 31}},
		"month":  {spec.month, []int{2, 12}},
		"dow":    {spec.dow, []int{0, 2, 3, 4}},
	} {
		got := tc.got.values()
		if len(got) != len(tc.want) {
			t.Errorf("%s = %v, want %v", name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s = %v, want %v", name, got, tc.want)
			}
		}
	}
	if s, _ := Parse("@weekly"); s.String() != "@weekly" || !s.dow.has(0) || len(s.dow.values()) != 1 {
		t.Errorf("@weekly = %q %v", s.String(), s.dow.values())
	}
	if s, _ := Parse("0   8 * *  *"); s.String() != "0 8 * * *" {
		t.Errorf("normalised = %q", s.String())
	}
	star, _ := Parse("0 0 * * */2")
	if got := star.dow.values(); len(got) != 4 || got[1] != 2 || got[3] != 6 {
		t.Errorf("*/2 weekdays = %v", got)
	}
}

func TestNextInUTC(t *testing.T) {
	utc := time.UTC
	for _, tc := range []struct{ name, expr, after, want string }{
		{"every minute", "* * * * *", "2026-03-10T10:00:00Z", "2026-03-10T10:01:00Z"},
		{"strictly after, with seconds", "* * * * *", "2026-03-10T10:00:30Z", "2026-03-10T10:01:00Z"},
		{"list within a day", "0 8,12,16,20 * * *", "2026-03-10T08:00:00Z", "2026-03-10T12:00:00Z"},
		{"list rolls to next day", "0 8,12,16,20 * * *", "2026-03-10T20:00:00Z", "2026-03-11T08:00:00Z"},
		{"step", "*/15 * * * *", "2026-03-10T10:46:00Z", "2026-03-10T11:00:00Z"},
		{"range and step", "0-30/10 9 * * *", "2026-03-10T09:11:00Z", "2026-03-10T09:20:00Z"},
		{"weekdays only", "0 9 * * mon-fri", "2026-03-13T09:00:00Z", "2026-03-16T09:00:00Z"}, // Friday -> Monday
		{"sunday as 7", "0 9 * * 7", "2026-03-10T00:00:00Z", "2026-03-15T09:00:00Z"},
		{"sunday as 0", "0 9 * * 0", "2026-03-10T00:00:00Z", "2026-03-15T09:00:00Z"},
		{"month name", "0 0 1 jul *", "2026-03-10T00:00:00Z", "2026-07-01T00:00:00Z"},
		{"month rollover to next year", "0 0 1 jan *", "2026-03-10T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"hourly", "@hourly", "2026-03-10T10:30:00Z", "2026-03-10T11:00:00Z"},
		{"daily", "@daily", "2026-03-10T10:30:00Z", "2026-03-11T00:00:00Z"},
		{"weekly is Sunday", "@weekly", "2026-03-10T10:30:00Z", "2026-03-15T00:00:00Z"},
		{"monthly", "@monthly", "2026-03-10T10:30:00Z", "2026-04-01T00:00:00Z"},
		{"yearly", "@yearly", "2026-03-10T10:30:00Z", "2027-01-01T00:00:00Z"},
		{"day 31 skips short months", "0 0 31 * *", "2026-04-01T00:00:00Z", "2026-05-31T00:00:00Z"},
		{"leap day", "0 0 29 feb *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		// Vixie: both day fields restricted -> either matches.
		{"dom or dow, dom first", "0 0 13 * fri", "2026-03-10T00:00:00Z", "2026-03-13T00:00:00Z"}, // Fri 13th: both
		{"dom or dow, dow first", "0 0 20 * fri", "2026-03-10T00:00:00Z", "2026-03-13T00:00:00Z"},
		{"dom or dow, dom later", "0 0 14 * fri", "2026-03-13T00:00:00Z", "2026-03-14T00:00:00Z"},
		// A star day field means the other one alone decides.
		{"dow only", "0 0 * * fri", "2026-03-10T00:00:00Z", "2026-03-13T00:00:00Z"},
		{"dom only", "0 0 14 * *", "2026-03-10T00:00:00Z", "2026-03-14T00:00:00Z"},
		{"step star dom is a star: both must match", "0 0 */10 * mon", "2026-03-10T00:00:00Z", "2026-05-11T00:00:00Z"}, // days 1,11,21,31 and a Monday
	} {
		spec, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, ok := spec.Next(at(t, tc.after), utc)
		if !ok || !got.Equal(at(t, tc.want)) {
			t.Errorf("%s: %q after %s = %s ok=%v, want %s", tc.name, tc.expr, tc.after, got.Format(time.RFC3339), ok, tc.want)
		}
	}
}

func TestNextNeverMatches(t *testing.T) {
	for _, expr := range []string{"0 0 30 feb *", "0 0 31 apr *"} {
		spec, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := spec.Next(at(t, "2026-01-01T00:00:00Z"), time.UTC); ok {
			t.Errorf("%q matched %s", expr, got)
		}
	}
}

// Europe/Prague changes at 02:00 CET -> 03:00 CEST on 2026-03-29 (01:00Z) and at
// 03:00 CEST -> 02:00 CET on 2026-10-25 (01:00Z).
func TestNextAcrossDST(t *testing.T) {
	prague := mustLoc(t, "Europe/Prague")
	for _, tc := range []struct{ name, expr, after, want string }{
		{"ordinary winter time", "30 2 * * *", "2026-03-27T12:00:00Z", "2026-03-28T01:30:00Z"},
		{"ordinary summer time", "30 2 * * *", "2026-06-01T12:00:00Z", "2026-06-02T00:30:00Z"},
		// Spring gap: 02:30 does not exist on the 29th; it runs at the first valid minute, 03:00 CEST.
		{"gap: skipped time runs after the gap", "30 2 * * *", "2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"},
		{"gap: the next day is back to normal", "30 2 * * *", "2026-03-29T01:00:00Z", "2026-03-30T00:30:00Z"},
		{"gap: 02:00 exactly", "0 2 * * *", "2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"},
		{"gap: last skipped minute", "59 2 * * *", "2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"},
		{"gap: first valid minute is its own match", "0 3 * * *", "2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"},
		{"gap: before the gap is untouched", "30 1 * * *", "2026-03-28T12:00:00Z", "2026-03-29T00:30:00Z"},
		{"gap: after the gap is untouched", "30 3 * * *", "2026-03-28T12:00:00Z", "2026-03-29T01:30:00Z"},
		{"gap: two skipped slots run once", "15,45 2 * * *", "2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"},
		{"gap: two skipped slots, second search", "15,45 2 * * *", "2026-03-29T01:00:00Z", "2026-03-30T00:15:00Z"},
		{"gap: every 15 minutes passes through", "*/15 * * * *", "2026-03-29T00:45:00Z", "2026-03-29T01:00:00Z"},
		{"gap: hourly passes through", "@hourly", "2026-03-29T00:00:00Z", "2026-03-29T01:00:00Z"},
		// Autumn overlap: 02:00-02:59 happens twice on the 25th; it runs once, at the first occurrence.
		{"overlap: first occurrence", "30 2 * * *", "2026-10-24T12:00:00Z", "2026-10-25T00:30:00Z"},
		{"overlap: not again an hour later", "30 2 * * *", "2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z"},
		{"overlap: searching from the second pass", "30 2 * * *", "2026-10-25T01:10:00Z", "2026-10-26T01:30:00Z"},
		{"overlap: 03:00 after it", "0 3 * * *", "2026-10-24T12:00:00Z", "2026-10-25T02:00:00Z"},
		{"overlap: before it is untouched", "30 1 * * *", "2026-10-24T12:00:00Z", "2026-10-24T23:30:00Z"},
		{"overlap: hourly runs at 02:00 local once", "@hourly", "2026-10-24T23:00:00Z", "2026-10-25T00:00:00Z"},
		{"overlap: hourly does not run in the repeated hour", "@hourly", "2026-10-25T00:00:00Z", "2026-10-25T02:00:00Z"},
		{"weekly across spring", "0 9 * * sun", "2026-03-22T09:00:00Z", "2026-03-29T07:00:00Z"},
	} {
		spec, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, ok := spec.Next(at(t, tc.after), prague)
		if !ok || !got.Equal(at(t, tc.want)) {
			t.Errorf("%s: %q after %s = %s ok=%v, want %s", tc.name, tc.expr, tc.after, got.Format(time.RFC3339), ok, tc.want)
		}
	}
}

func TestDaySequenceAcrossGapAndOverlapRunsOncePerDay(t *testing.T) {
	prague := mustLoc(t, "Europe/Prague")
	spec, _ := Parse("30 2 * * *")
	for _, tc := range []struct {
		from string
		days int
	}{{"2026-03-27T12:00:00Z", 5}, {"2026-10-23T12:00:00Z", 5}} {
		runs := spec.Upcoming(at(t, tc.from), prague, tc.days)
		if len(runs) != tc.days {
			t.Fatalf("got %d runs", len(runs))
		}
		days := map[string]bool{}
		for _, run := range runs {
			key := run.In(prague).Format("2006-01-02")
			if days[key] {
				t.Errorf("two runs on %s: %v", key, runs)
			}
			days[key] = true
		}
	}
}

func TestOtherZones(t *testing.T) {
	// America/New_York: 02:00 EST -> 03:00 EDT on 2026-03-08 (07:00Z); 02:00 EDT -> 01:00 EST on 2026-11-01 (06:00Z).
	ny := mustLoc(t, "America/New_York")
	spec, _ := Parse("30 2 * * *")
	if got, _ := spec.Next(at(t, "2026-03-07T12:00:00Z"), ny); !got.Equal(at(t, "2026-03-08T07:00:00Z")) {
		t.Errorf("New York gap = %s", got)
	}
	spec, _ = Parse("30 1 * * *")
	if got, _ := spec.Next(at(t, "2026-10-31T12:00:00Z"), ny); !got.Equal(at(t, "2026-11-01T05:30:00Z")) {
		t.Errorf("New York overlap first = %s", got)
	}
	if got, _ := spec.Next(at(t, "2026-11-01T05:30:00Z"), ny); !got.Equal(at(t, "2026-11-02T06:30:00Z")) {
		t.Errorf("New York overlap runs once = %s", got)
	}
	// Australia/Lord_Howe shifts by thirty minutes at 02:00 (to 02:30) in October.
	lh := mustLoc(t, "Australia/Lord_Howe")
	spec, _ = Parse("15 2 * * *")
	if got, _ := spec.Next(at(t, "2026-10-03T12:00:00Z"), lh); !got.Equal(at(t, "2026-10-03T15:30:00Z")) {
		t.Errorf("Lord Howe half-hour gap = %s", got) // 02:00 LHST (+10:30) is 15:30Z on the 3rd; 02:30 LHDT (+11) follows
	}
	// A zone without DST and a fixed odd offset.
	kathmandu := mustLoc(t, "Asia/Kathmandu")
	spec, _ = Parse("0 8 * * *")
	if got, _ := spec.Next(at(t, "2026-03-10T00:00:00Z"), kathmandu); !got.Equal(at(t, "2026-03-10T02:15:00Z")) {
		t.Errorf("Kathmandu = %s", got)
	}
	// Midnight gap: America/Sao_Paulo had 00:00 -> 01:00 until 2019.
	sp := mustLoc(t, "America/Sao_Paulo")
	spec, _ = Parse("30 0 * * *")
	if got, _ := spec.Next(at(t, "2018-11-03T12:00:00Z"), sp); !got.Equal(at(t, "2018-11-04T03:00:00Z")) {
		t.Errorf("Sao Paulo midnight gap = %s", got)
	}
}

func TestNilLocationIsUTC(t *testing.T) {
	spec, _ := Parse("0 8 * * *")
	if got, _ := spec.Next(at(t, "2026-03-10T00:00:00Z"), nil); !got.Equal(at(t, "2026-03-10T08:00:00Z")) {
		t.Errorf("got %s", got)
	}
}

func TestDescribe(t *testing.T) {
	for expr, want := range map[string]string{
		"0 8,12,16,20 * * *": "at 08:00, 12:00, 16:00 and 20:00, every day",
		"* * * * *":          "every minute, every day",
		"*/5 * * * *":        "every 5 minutes, every day",
		"0 * * * *":          "every hour, on the hour, every day",
		"@hourly":            "every hour, on the hour, every day",
		"15 * * * *":         "at minute 15 of every hour, every day",
		"0,45 * * * *":       "at minutes 0 and 45 of every hour, every day",
		"0,30 * * * *":       "every 30 minutes, every day",
		"30 9 * * *":         "at 09:30, every day",
		"@daily":             "at 00:00, every day",
		"0 9 * * mon-fri":    "at 09:00, on Monday to Friday",
		"0 9 * * 1,3":        "at 09:00, on Monday and Wednesday",
		"@weekly":            "at 00:00, on Sunday",
		"@monthly":           "at 00:00, on the 1st of the month",
		"@yearly":            "at 00:00, on the 1st of the month, in January",
		"0 0 1,15 * *":       "at 00:00, on the 1st and 15th of the month",
		"0 0 22 * *":         "at 00:00, on the 22nd of the month",
		"0 0 3 * *":          "at 00:00, on the 3rd of the month",
		"0 0 11-13 * *":      "at 00:00, on the 11th-13th of the month",
		"0 8 * jun-aug *":    "at 08:00, every day in June to August",
		"0 8 * jan,jul *":    "at 08:00, every day in January and July",
		"0 0 13 * fri":       "at 00:00, on the 13th of the month and on Friday (either one)",
		"0 0 */10 * mon":     "at 00:00, on the 1st, 11th, 21st and 31st of the month, only when it is Monday",
		"* 9-17 * * *":       "every minute during hours 09-17, every day",
		"* 9 * * *":          "every minute during the 09:00 hour, every day",
		"*/10 9-17 * * *":    "at minutes 0, 10, 20, 30, 40 and 50 during hours 09-17, every day",
		"0 9-17 * * 1-5":     "at 09:00, 10:00, 11:00, 12:00, 13:00, 14:00, 15:00, 16:00 and 17:00, on Monday to Friday",
		"0,15 8-17 * * *":    "at minutes 0 and 15 during hours 08-17, every day",
	} {
		spec, err := Parse(expr)
		if err != nil {
			t.Errorf("%q: %v", expr, err)
			continue
		}
		if got := spec.Describe(); got != want {
			t.Errorf("%q described as\n  %q\nwant\n  %q", expr, got, want)
		}
	}
}
