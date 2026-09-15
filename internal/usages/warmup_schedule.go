package usages

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// warmupSchedule is the deliberately small cron subset used by Claude warmup.
// Every field accepts either * or one integer. Lists, ranges, and steps are
// rejected so accepted schedules have unambiguous behavior without a cron
// dependency.
type warmupSchedule struct {
	minute     cronValue
	hour       cronValue
	dayOfMonth cronValue
	month      cronValue
	dayOfWeek  cronValue
}

type cronValue struct {
	any   bool
	value int
}

func parseWarmupSchedule(raw string) (*warmupSchedule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return nil, newWarmupScheduleError("must have 5 fields: minute hour day-of-month month day-of-week")
	}
	specs := []struct {
		name     string
		min, max int
	}{
		{name: "minute", min: 0, max: 59},
		{name: "hour", min: 0, max: 23},
		{name: "day-of-month", min: 1, max: 31},
		{name: "month", min: 1, max: 12},
		{name: "day-of-week", min: 0, max: 7},
	}
	parsed := make([]cronValue, len(fields))
	for i, field := range fields {
		if field == "*" {
			parsed[i] = cronValue{any: true}
			continue
		}
		value, err := strconv.Atoi(field)
		if err != nil || value < specs[i].min || value > specs[i].max {
			return nil, newWarmupScheduleError(fmt.Sprintf("%s must be * or an integer from %d to %d", specs[i].name, specs[i].min, specs[i].max))
		}
		if i == 4 && value == 7 {
			value = 0 // both 0 and 7 mean Sunday in crontab
		}
		parsed[i] = cronValue{value: value}
	}
	return &warmupSchedule{
		minute: parsed[0], hour: parsed[1], dayOfMonth: parsed[2],
		month: parsed[3], dayOfWeek: parsed[4],
	}, nil
}

func parseWarmupSchedules(raw string) ([]*warmupSchedule, error) {
	lines := strings.Split(raw, "\n")
	schedules := make([]*warmupSchedule, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		schedule, err := parseWarmupSchedule(line)
		if err != nil {
			return nil, fmt.Errorf("warmup_schedule line %d: %w", i+1, err)
		}
		schedules = append(schedules, schedule)
	}
	return schedules, nil
}

func warmupSchedulesMatch(schedules []*warmupSchedule, now time.Time) bool {
	for _, schedule := range schedules {
		if schedule.matches(now) {
			return true
		}
	}
	return false
}

func newWarmupScheduleError(detail string) error {
	return fmt.Errorf("warmup_schedule: %s", detail)
}

func (v cronValue) matches(value int) bool { return v.any || v.value == value }

func (s *warmupSchedule) matches(now time.Time) bool {
	if s == nil {
		return true
	}
	if !s.minute.matches(now.Minute()) || !s.hour.matches(now.Hour()) || !s.month.matches(int(now.Month())) {
		return false
	}
	domMatch := s.dayOfMonth.matches(now.Day())
	dowMatch := s.dayOfWeek.matches(int(now.Weekday()))
	// Standard cron treats restricted day-of-month and day-of-week fields as OR.
	if !s.dayOfMonth.any && !s.dayOfWeek.any {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}
