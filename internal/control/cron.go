package control

import (
	"errors"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type cronSchedule struct {
	location                  *time.Location
	fields                    [5]map[int]bool
	dayWildcard, weekWildcard bool
}

func parseCron(expression string) (cronSchedule, error) {
	schedule := cronSchedule{location: time.UTC}
	fields := strings.Fields(expression)
	if len(fields) != 5 || len(expression) > 256 {
		return schedule, errors.New("use five cron fields: minute hour day month weekday")
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for index, field := range fields {
		schedule.fields[index] = map[int]bool{}
		for _, part := range strings.Split(field, ",") {
			base, stepText, hasStep := strings.Cut(part, "/")
			step := 1
			if hasStep {
				var err error
				step, err = strconv.Atoi(stepText)
				if err != nil || step < 1 || step > bounds[index][1]+1 {
					return schedule, errors.New("invalid cron step")
				}
			}
			low, high := bounds[index][0], bounds[index][1]
			if base != "*" {
				start, end, hasRange := strings.Cut(base, "-")
				var err error
				low, err = strconv.Atoi(start)
				if err != nil {
					return schedule, errors.New("cron fields must use numbers, ranges, lists, or steps")
				}
				high = low
				if hasRange {
					high, err = strconv.Atoi(end)
					if err != nil {
						return schedule, errors.New("invalid cron range")
					}
				} else if hasStep {
					high = bounds[index][1]
				}
			}
			if low < bounds[index][0] || high > bounds[index][1] || high < low {
				return schedule, errors.New("cron value out of range; Sunday is 0")
			}
			for value := low; value <= high; value += step {
				schedule.fields[index][value] = true
			}
		}
	}
	schedule.dayWildcard = strings.HasPrefix(fields[2], "*")
	schedule.weekWildcard = strings.HasPrefix(fields[4], "*")
	return schedule, nil
}

func (c cronSchedule) next(after time.Time) (time.Time, error) {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(5, 0, 0)
	for candidate.Before(limit) {
		local := candidate.In(c.location)
		day, week := c.fields[2][local.Day()], c.fields[4][int(local.Weekday())]
		dayMatch := day && week
		if !c.dayWildcard && !c.weekWildcard {
			dayMatch = day || week
		}
		if c.fields[3][int(local.Month())] && dayMatch && c.fields[1][local.Hour()] && c.fields[0][local.Minute()] {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, errors.New("cron has no occurrence in the next five years")
}
