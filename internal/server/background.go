package server

import (
	"context"
	"errors"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// heartbeatTimeout is how long a running job may go without a progress
// report before the agent is considered gone.
const heartbeatTimeout = 5 * time.Minute

// background runs schedules, timeouts and repository maintenance.
func (s *Server) background(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		s.expireJobs()
		s.expireRecovery()
		s.schedule(time.Now())
		s.scheduleOffsite(time.Now())
		s.checkMissed(time.Now())
		s.scheduleVerify(time.Now())
		s.recordUsage(time.Now())
		s.sendSummary(time.Now())
		s.applyPendingProtection(time.Now())
		s.purgeTrash(time.Now())
		s.maintain()
		s.maybeSaveState()
		s.runOffsite(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

func (s *Server) expireJobs() {
	stale, err := s.jobs(`WHERE status = ? AND agent_id != '' AND heartbeat < ?`, api.StatusRunning, ms(time.Now().Add(-heartbeatTimeout)))
	if err != nil {
		s.log.Error("checking for stalled jobs", "err", err)
		return
	}
	for _, j := range stale {
		s.finish(j, api.StatusFailed, "the agent stopped reporting progress", nil, nil)
	}
}

// due reports whether a policy's schedule calls for a backup now. last
// is when its most recent backup was queued.
func due(p *api.Policy, last, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	switch {
	case p.Schedule.Interval != "":
		d, err := time.ParseDuration(p.Schedule.Interval)
		return err == nil && (last.IsZero() || now.Sub(last) >= d)
	case p.Schedule.DailyAt != "":
		h, m, err := parseDailyAt(p.Schedule.DailyAt)
		if err != nil {
			return false
		}
		slot := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if now.Before(slot) {
			slot = slot.AddDate(0, 0, -1)
		}
		// A new policy waits for its first slot instead of running at once.
		if last.IsZero() {
			last = p.Created
		}
		return last.Before(slot)
	}
	return false
}

func (s *Server) schedule(now time.Time) {
	ps, err := s.policies("")
	if err != nil {
		s.log.Error("loading policies", "err", err)
		return
	}
	for _, p := range ps {
		var last int64
		s.db.QueryRow(`SELECT COALESCE(MAX(created), 0) FROM jobs WHERE policy_id = ? AND type = ?`, p.ID, api.JobBackup).Scan(&last)
		if !due(p, fromMS(last), now) {
			continue
		}
		if j, err := s.enqueueBackup(p); err != nil {
			s.log.Error("scheduling backup", "policy", p.Name, "err", err)
		} else if j != nil {
			s.log.Info("scheduled backup", "policy", p.Name, "job", j.ID)
		}
	}
	// Reclaim space once a day when snapshots have been forgotten.
	if s.setting("gc_needed") == "1" {
		var last int64
		s.db.QueryRow(`SELECT COALESCE(MAX(created), 0) FROM jobs WHERE type = ?`, api.JobGC).Scan(&last)
		if now.Sub(fromMS(last)) >= 24*time.Hour {
			s.enqueue(&api.Job{Type: api.JobGC})
		}
	}
}

// maintain runs queued GC and verify jobs while no agent job is active.
// New agent jobs wait until it is done.
func (s *Server) maintain() {
	for {
		js, err := s.jobs(`WHERE agent_id = '' AND type != ? AND status = ? ORDER BY created LIMIT 1`, api.JobOffsite, api.StatusQueued)
		if err != nil || len(js) == 0 {
			return
		}
		s.mu.Lock()
		j := js[0]
		if len(s.running) > 0 || s.offsiteBusy || (j.Type == api.JobGC && s.reading > 0) {
			s.mu.Unlock()
			return // retried when a job or a download finishes
		}
		s.maint, s.gcRunning = true, j.Type == api.JobGC
		s.mu.Unlock()

		s.db.Exec(`UPDATE jobs SET status = ?, started = ? WHERE id = ?`, api.StatusRunning, ms(time.Now()), j.ID)
		s.log.Info("maintenance started", "job", j.ID, "type", j.Type)
		var result any
		switch j.Type {
		case api.JobGC:
			result, err = s.gc()
		case api.JobVerify:
			st, verr := s.repo.Verify()
			result, err = st, verr
			if err == nil && len(st.Errors) > 0 {
				err = errors.New("verification found damaged data; see the job result")
			}
		default:
			err = errors.New("unknown job type " + j.Type)
		}
		if err != nil {
			s.finish(j, api.StatusFailed, err.Error(), result, nil)
		} else {
			s.finish(j, api.StatusSuccess, "", result, nil)
		}
		s.mu.Lock()
		s.maint, s.gcRunning = false, false
		s.mu.Unlock()
		s.wakeAll()
	}
}

func (s *Server) gc() (any, error) {
	s.mu.Lock()
	snaps := make([]*tree.Snapshot, 0, len(s.snaps))
	for _, sn := range s.snaps {
		snaps = append(snaps, sn)
	}
	// Deleted snapshots keep their data until their time is up.
	for _, sn := range s.trashed {
		snaps = append(snaps, sn)
	}
	s.mu.Unlock()
	st, err := s.repo.GC(func(mark func(keys.ID) error) error {
		seen := map[keys.ID]struct{}{}
		for _, sn := range snaps {
			if err := tree.Mark(s.repo.Get, sn.Sources, seen, mark); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.setSetting("gc_needed", "0")
	}
	return st, err
}
