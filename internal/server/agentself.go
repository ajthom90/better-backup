package server

import (
	"net/http"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// What an agent may ask about, and do for, its own machine. The agent
// shows this on the machine it backs up, where there is no admin token.

// handleAgentSummary tells an agent about its policies and how their
// last backups went.
func (s *Server) handleAgentSummary(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	ps, err := s.policiesOf(a.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := api.AgentSummary{Name: a.Name, Policies: []api.PolicySummary{}}
	for _, p := range ps {
		sum := api.PolicySummary{ID: p.ID, Name: p.Name, Paused: !p.Enabled}
		switch {
		case p.Schedule.DailyAt != "":
			sum.Schedule = "daily at " + p.Schedule.DailyAt
		case p.Schedule.Interval != "":
			sum.Schedule = "every " + p.Schedule.Interval
		}
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE policy_id = ? AND agent_id = ? AND type = ? AND status IN (?, ?)`,
			p.ID, a.ID, api.JobBackup, api.StatusQueued, api.StatusRunning).Scan(&n)
		sum.Pending = n > 0
		var finished int64
		if s.db.QueryRow(`SELECT status, error, finished FROM jobs WHERE policy_id = ? AND agent_id = ? AND type = ? AND finished > 0 ORDER BY finished DESC LIMIT 1`,
			p.ID, a.ID, api.JobBackup).Scan(&sum.LastStatus, &sum.LastError, &finished) == nil {
			t := fromMS(finished)
			sum.LastFinished = &t
		}
		if s.db.QueryRow(`SELECT finished FROM jobs WHERE policy_id = ? AND agent_id = ? AND type = ? AND status IN (?, ?) ORDER BY finished DESC LIMIT 1`,
			p.ID, a.ID, api.JobBackup, api.StatusSuccess, api.StatusWarning).Scan(&finished) == nil {
			t := fromMS(finished)
			sum.LastSuccess = &t
		}
		out.Policies = append(out.Policies, sum)
	}
	s.db.Exec(`UPDATE agents SET last_seen = ? WHERE id = ?`, ms(time.Now()), a.ID)
	reply(w, http.StatusOK, out)
}

// handleAgentBackup runs every policy of the agent's machine now, as
// "Run now" in the web UI does: someone at the machine asked for it.
func (s *Server) handleAgentBackup(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	ps, err := s.policiesOf(a.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := api.BackupNow{Policies: len(ps)}
	for _, p := range ps {
		j, err := s.enqueueBackup(p, a.ID)
		if err != nil {
			s.internal(w, err)
			return
		}
		if j != nil {
			out.Started++
		}
	}
	if out.Started > 0 {
		s.log.Info("backup requested at the machine", "agent", a.Name, "policies", out.Started)
	}
	reply(w, http.StatusOK, out)
}
