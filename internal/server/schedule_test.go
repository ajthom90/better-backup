package server

import (
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// TestWeekdaySchedule: a schedule of weekdays that repeats through the
// day starts a backup at each of its times and on none of the others.
func TestWeekdaySchedule(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.Local) } // 2026-11-23 is a Monday
	sch := api.Schedule{DailyAt: "06:00", Days: []int{1, 2, 3, 4, 5}, Every: "3h", Until: "18:00"}
	if err := checkSchedule(&sch); err != nil {
		t.Fatal(err)
	}
	p := &api.Policy{Enabled: true, Created: at(20, 19, 0), Schedule: sch}
	slots := slotsOn(&sch, at(23, 0, 0))
	if len(slots) != 5 || !slots[0].Equal(at(23, 6, 0)) || !slots[4].Equal(at(23, 18, 0)) {
		t.Fatalf("Monday's times: %v", slots)
	}
	if got := slotsOn(&sch, at(22, 0, 0)); len(got) != 0 {
		t.Fatalf("nothing runs on Sunday: %v", got)
	}
	// Created on Friday after the last slot: nothing is due over the
	// weekend, and Monday 06:00 is.
	if due(p, time.Time{}, at(22, 23, 0)) {
		t.Fatal("due on a Sunday")
	}
	if !due(p, time.Time{}, at(23, 6, 1)) {
		t.Fatal("not due on Monday morning")
	}
	if due(p, at(23, 6, 1), at(23, 8, 59)) || !due(p, at(23, 6, 1), at(23, 9, 0)) {
		t.Fatal("the 09:00 backup is due at 09:00 and not before")
	}
	if due(p, at(23, 18, 0), at(23, 23, 0)) {
		t.Fatal("due after the last time of the day")
	}
	if n := nextRun(p, at(23, 18, 0), at(23, 19, 0)); n == nil || !n.Equal(at(24, 6, 0)) {
		t.Fatalf("next run after Monday's last: %v", n)
	}
	if n := nextRun(p, at(27, 18, 0), at(27, 19, 0)); n == nil || !n.Equal(at(30, 6, 0)) {
		t.Fatalf("next run after Friday's last is Monday: %v", n)
	}
	// Overdue: a good backup on Friday evening is fine all weekend, and
	// late once Monday's first backup is well past.
	good := at(27, 18, 5)
	if overdue(p, good, at(29, 12, 0)) || overdue(p, good, at(30, 6, 30)) {
		t.Fatal("overdue before a backup was missed")
	}
	if !overdue(p, good, at(30, 7, 31)) {
		t.Fatal("not overdue when Monday's first backup is an hour and a half late")
	}
	// Once a week.
	weekly := &api.Policy{Enabled: true, Created: at(1, 0, 0), Schedule: api.Schedule{DailyAt: "18:30", Days: []int{5}}}
	if overdue(weekly, at(20, 19, 0), at(27, 20, 0)) || !overdue(weekly, at(20, 19, 0), at(31, 7, 0)) {
		t.Fatal("a weekly backup is overdue half a week after the one that was missed")
	}

	for _, bad := range []api.Schedule{
		{Days: []int{1}}, {DailyAt: "06:00", Days: []int{7}}, {DailyAt: "06:00", Until: "09:00"},
		{DailyAt: "06:00", Every: "1m"}, {DailyAt: "06:00", Every: "1h", Until: "05:00"}, {Interval: "1h", DailyAt: "06:00"},
	} {
		if checkSchedule(&bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	all := api.Schedule{DailyAt: "01:00", Days: []int{6, 0, 1, 2, 3, 4, 5, 5}}
	if err := checkSchedule(&all); err != nil || all.Days != nil {
		t.Fatalf("all seven days is every day: %v %v", all.Days, err)
	}
}
