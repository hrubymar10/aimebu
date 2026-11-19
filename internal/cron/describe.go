package cron

import (
	"fmt"
	"strings"
	"time"
)

// Describe puts the schedule in plain English, for example
// "at 08:00, 12:00, 16:00 and 20:00, every day".
func (s *Spec) Describe() string {
	return s.timePhrase() + ", " + s.dayPhrase()
}

func (s *Spec) timePhrase() string {
	minutes, hours := s.minute.values(), s.hour.values()
	allHours := len(hours) == 24
	if allHours {
		switch step, ok := progression(minutes, 60); {
		case len(minutes) == 60:
			return "every minute"
		case ok && step > 1:
			return fmt.Sprintf("every %d minutes", step)
		case len(minutes) == 1 && minutes[0] == 0:
			return "every hour, on the hour"
		case len(minutes) == 1:
			return fmt.Sprintf("at minute %d of every hour", minutes[0])
		default:
			return "at minutes " + joinAnd(numbers(minutes)) + " of every hour"
		}
	}
	if len(minutes) == 60 {
		return "every minute during " + hourSpan(hours)
	}
	if len(minutes)*len(hours) <= 12 {
		times := make([]string, 0, len(minutes)*len(hours))
		for _, h := range hours {
			for _, m := range minutes {
				times = append(times, fmt.Sprintf("%02d:%02d", h, m))
			}
		}
		return "at " + joinAnd(times)
	}
	return "at minutes " + joinAnd(numbers(minutes)) + " during " + hourSpan(hours)
}

func hourSpan(hours []int) string {
	if len(hours) == 1 {
		return fmt.Sprintf("the %02d:00 hour", hours[0])
	}
	return "hours " + joinAnd(runs(hours, func(v int) string { return fmt.Sprintf("%02d", v) }, "-"))
}

func (s *Spec) dayPhrase() string {
	var day string
	domAll, dowAll := len(s.dom.values()) == 31, len(s.dow.values()) == 7
	dom := "the " + joinAnd(ordinals(s.dom.values())) + " of the month"
	dow := "on " + joinAnd(runs(s.dow.values(), func(v int) string { return time.Weekday(v).String() }, " to "))
	switch {
	case domAll && dowAll:
		day = "every day"
	case s.domStar || s.dowStar:
		// Both must match; a `*` side matches everything, so say the restricted one.
		switch {
		case !domAll && !dowAll:
			day = "on " + dom + ", only when it is " + strings.TrimPrefix(dow, "on ")
		case !domAll:
			day = "on " + dom
		default:
			day = dow
		}
	default:
		day = "on " + dom + " and " + dow + " (either one)"
	}
	if months := s.month.values(); len(months) < 12 {
		name := func(v int) string { return time.Month(v).String() }
		in := "in " + joinAnd(runs(months, name, " to "))
		if day == "every day" {
			return "every day " + in
		}
		return day + ", " + in
	}
	return day
}

// progression reports the step when the values are 0, n, 2n, ... below limit
// with n dividing the limit.
func progression(values []int, limit int) (int, bool) {
	if len(values) < 2 || values[0] != 0 {
		return 0, false
	}
	step := values[1]
	if limit%step != 0 || len(values) != limit/step {
		return 0, false
	}
	for i, v := range values {
		if v != i*step {
			return 0, false
		}
	}
	return step, true
}

// runs renders sorted values, folding three or more consecutive ones into a range.
func runs(values []int, name func(int) string, rangeSep string) []string {
	var out []string
	for i := 0; i < len(values); {
		j := i
		for j+1 < len(values) && values[j+1] == values[j]+1 {
			j++
		}
		if j-i >= 2 {
			out = append(out, name(values[i])+rangeSep+name(values[j]))
		} else {
			for k := i; k <= j; k++ {
				out = append(out, name(values[k]))
			}
		}
		i = j + 1
	}
	return out
}

func numbers(values []int) []string {
	return runs(values, func(v int) string { return fmt.Sprint(v) }, "-")
}

func ordinals(values []int) []string {
	return runs(values, ordinal, "-")
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
