package providers

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type warmupCronField struct {
	any   bool
	value int
}

type WarmupCron [5]warmupCronField

// parseWarmupSchedule deliberately accepts only stars and single integers.
func ParseWarmupSchedule(schedule string) ([]WarmupCron, error) {
	var out []WarmupCron
	for i, line := range strings.Split(schedule, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 5 {
			return nil, fmt.Errorf("warmup schedule line %d: expected five fields", i+1)
		}
		var cron WarmupCron
		for j, part := range parts {
			if part == "*" {
				cron[j].any = true
				continue
			}
			value, err := strconv.Atoi(part)
			limits := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
			if err != nil || value < limits[j][0] || value > limits[j][1] {
				return nil, fmt.Errorf("warmup schedule line %d: invalid field %d", i+1, j+1)
			}
			if j == 4 && value == 7 {
				value = 0
			}
			cron[j].value = value
		}
		out = append(out, cron)
	}
	return out, nil
}

func WarmupScheduleMatches(lines []WarmupCron, now time.Time) bool {
	for _, c := range lines {
		values := [5]int{now.Minute(), now.Hour(), now.Day(), int(now.Month()), int(now.Weekday())}
		if !c[0].any && c[0].value != values[0] || !c[1].any && c[1].value != values[1] || !c[3].any && c[3].value != values[3] {
			continue
		}
		day := c[2].any || c[2].value == values[2]
		week := c[4].any || c[4].value == values[4]
		if !c[2].any && !c[4].any {
			if day || week {
				return true
			}
			continue
		}
		if day && week {
			return true
		}
	}
	return false
}

func ValidateWarmup(mode, schedule string) error {
	if mode != "off" && mode != "force" && mode != "scheduled" && mode != "smart" {
		return fmt.Errorf("warmup_mode must be off, force, scheduled, or smart")
	}
	lines, err := ParseWarmupSchedule(schedule)
	if err != nil {
		return err
	}
	if mode == "scheduled" && len(lines) == 0 {
		return fmt.Errorf("warmup_schedule is required for scheduled mode")
	}
	return nil
}
