package server

import (
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/tree"
)

func TestForget(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.Local)
	var snaps []*tree.Snapshot
	for d := 0; d < 60; d++ { // two a day for 60 days
		for _, h := range []int{2, 14} {
			snaps = append(snaps, &tree.Snapshot{Time: now.AddDate(0, 0, -d).Add(time.Duration(h-12) * time.Hour)})
		}
	}
	if got := forget(append([]*tree.Snapshot(nil), snaps...), api.Retention{}); got != nil {
		t.Fatalf("no rules must keep everything, dropped %d", len(got))
	}
	dropped := forget(append([]*tree.Snapshot(nil), snaps...), api.Retention{KeepLast: 3, KeepDaily: 7, KeepMonthly: 2})
	kept := len(snaps) - len(dropped)
	// 7 dailies (the newest of each day), plus the second snapshot of
	// today from keep-last, plus May's newest from keep-monthly.
	if kept != 9 {
		t.Fatalf("kept %d snapshots, want 9", kept)
	}
	for _, d := range dropped {
		if d.Time.Equal(snaps[0].Time) {
			t.Fatal("the newest snapshot was dropped")
		}
	}
	if got := forget([]*tree.Snapshot{snaps[5]}, api.Retention{KeepYearly: 1}); len(got) != 0 {
		t.Fatal("the only snapshot was dropped")
	}
}

func TestDue(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 6, 30, h, m, 0, 0, time.Local) }
	every := &api.Policy{Enabled: true, Schedule: api.Schedule{Interval: "6h"}}
	daily := &api.Policy{Enabled: true, Schedule: api.Schedule{DailyAt: "02:00"}, Created: at(1, 0).AddDate(0, 0, -3)}
	cases := []struct {
		name      string
		p         *api.Policy
		last, now time.Time
		want      bool
	}{
		{"interval never run", every, time.Time{}, at(9, 0), true},
		{"interval too soon", every, at(4, 0), at(9, 0), false},
		{"interval elapsed", every, at(3, 0), at(9, 0), true},
		{"daily before slot, ran yesterday", daily, at(2, 1).AddDate(0, 0, -1), at(1, 59), false},
		{"daily after slot, ran yesterday", daily, at(2, 1).AddDate(0, 0, -1), at(2, 0), true},
		{"daily already ran today", daily, at(2, 0), at(15, 0), false},
		{"daily missed while server was down", daily, at(2, 0).AddDate(0, 0, -2), at(15, 0), true},
		{"new daily policy waits for its slot", &api.Policy{Enabled: true, Schedule: api.Schedule{DailyAt: "02:00"}, Created: at(10, 0)}, time.Time{}, at(15, 0), false},
		{"disabled", &api.Policy{Schedule: api.Schedule{Interval: "1h"}}, time.Time{}, at(9, 0), false},
		{"manual", &api.Policy{Enabled: true}, time.Time{}, at(9, 0), false},
	}
	for _, c := range cases {
		if got := due(c.p, c.last, c.now); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}
