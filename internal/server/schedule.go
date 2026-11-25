package server

import (
	"errors"
	"slices"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// checkSchedule says what is wrong with a schedule, or nil.
func checkSchedule(sch *api.Schedule) error {
	if sch.Interval != "" && sch.DailyAt != "" {
		return errors.New("set either interval or daily_at, not both")
	}
	if sch.Interval != "" {
		if d, err := time.ParseDuration(sch.Interval); err != nil || d < time.Minute {
			return errors.New("interval must be a duration of at least 1m, such as 24h")
		}
	}
	if sch.DailyAt == "" {
		if len(sch.Days) > 0 || sch.Every != "" || sch.Until != "" {
			return errors.New("days of the week and repeating through the day need a start time (daily_at)")
		}
		return nil
	}
	h, m, err := parseDailyAt(sch.DailyAt)
	if err != nil {
		return err
	}
	slices.Sort(sch.Days)
	sch.Days = slices.Compact(sch.Days)
	for _, d := range sch.Days {
		if d < 0 || d > 6 {
			return errors.New("days are numbers from 0 for Sunday to 6 for Saturday")
		}
	}
	if len(sch.Days) == 7 {
		sch.Days = nil
	}
	if sch.Every == "" {
		if sch.Until != "" {
			return errors.New("until needs every: how often the backup repeats through the day")
		}
		return nil
	}
	if d, err := time.ParseDuration(sch.Every); err != nil || d < 5*time.Minute {
		return errors.New("a backup can repeat through the day every 5 minutes or more, such as 3h")
	}
	if sch.Until != "" {
		uh, um, err := parseDailyAt(sch.Until)
		if err != nil {
			return err
		}
		if uh*60+um <= h*60+m {
			return errors.New("the time the repeating ends must be later in the day than the time it starts")
		}
	}
	return nil
}

// slotsOn returns the times a schedule with a start time begins a
// backup on the day of day, in order.
func slotsOn(sch *api.Schedule, day time.Time) []time.Time {
	if sch.DailyAt == "" || (len(sch.Days) > 0 && !slices.Contains(sch.Days, int(day.Weekday()))) {
		return nil
	}
	h, m, err := parseDailyAt(sch.DailyAt)
	if err != nil {
		return nil
	}
	at := func(h, m int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
	}
	out := []time.Time{at(h, m)}
	every, err := time.ParseDuration(sch.Every)
	if sch.Every == "" || err != nil || every < time.Minute {
		return out
	}
	end := at(23, 59)
	if uh, um, err := parseDailyAt(sch.Until); sch.Until != "" && err == nil {
		end = at(uh, um)
	}
	// Minutes of the day, not instants: a change of the clocks moves no slot.
	for mins := h*60 + m + int(every/time.Minute); mins < 24*60; mins += int(every / time.Minute) {
		t := at(mins/60, mins%60)
		if t.After(end) {
			break
		}
		out = append(out, t)
	}
	return out
}

// prevSlot is the latest time at or before now that the schedule
// starts a backup, or zero.
func prevSlot(sch *api.Schedule, now time.Time) time.Time {
	for back := 0; back <= 7; back++ {
		ss := slotsOn(sch, now.AddDate(0, 0, -back))
		for i := len(ss) - 1; i >= 0; i-- {
			if !ss[i].After(now) {
				return ss[i]
			}
		}
	}
	return time.Time{}
}

// nextSlot is the first time after after that the schedule starts a
// backup, or zero.
func nextSlot(sch *api.Schedule, after time.Time) time.Time {
	for on := 0; on <= 8; on++ {
		for _, t := range slotsOn(sch, after.AddDate(0, 0, on)) {
			if t.After(after) {
				return t
			}
		}
	}
	return time.Time{}
}

// nextRun says when a policy's schedule next starts a backup. last is
// when its most recent backup was queued.
func nextRun(p *api.Policy, last, now time.Time) *time.Time {
	if !p.Enabled {
		return nil
	}
	var t time.Time
	switch {
	case p.Schedule.Interval != "":
		d, err := time.ParseDuration(p.Schedule.Interval)
		if err != nil {
			return nil
		}
		if t = last.Add(d); last.IsZero() || t.Before(now) {
			t = now
		}
	case p.Schedule.DailyAt != "":
		t = nextSlot(&p.Schedule, now)
	}
	if t.IsZero() {
		return nil
	}
	return &t
}
