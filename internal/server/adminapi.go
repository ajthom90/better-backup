package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.repo.Stats()
	if err != nil {
		s.internal(w, err)
		return
	}
	out := api.Status{Version: Version, RepoID: s.repo.ID(), Fingerprint: s.fp, Packs: st.Packs,
		StoredBytes: st.StoredBytes, RawBytes: st.RawBytes, Chunks: st.Chunks}
	s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE revoked = 0`).Scan(&out.Agents)
	s.mu.Lock()
	out.Snapshots, out.RunningJobs = len(s.snaps), len(s.running)
	s.mu.Unlock()
	reply(w, http.StatusOK, out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TTL  string `json:"ttl"`
		Uses int    `json:"uses"`
	}
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	ttl := 24 * time.Hour
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			fail(w, http.StatusBadRequest, "ttl must be a positive duration such as 24h")
			return
		}
		ttl = d
	}
	if req.Uses <= 0 {
		req.Uses = 1
	}
	tok := api.EnrollToken{Token: keys.RandomHex(16), Expires: time.Now().Add(ttl), Uses: req.Uses, Fingerprint: s.fp}
	s.db.Exec(`DELETE FROM enroll_tokens WHERE expires < ? OR uses <= 0`, ms(time.Now()))
	if _, err := s.db.Exec(`INSERT INTO enroll_tokens (hash, expires, uses) VALUES (?, ?, ?)`, hashToken(tok.Token), ms(tok.Expires), tok.Uses); err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusCreated, tok)
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY name`)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	out := []*api.Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			s.internal(w, err)
			return
		}
		out = append(out, a)
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) handleRevokeAgent(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Exec(`UPDATE agents SET revoked = 1 WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "no such agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	ps, err := s.policies(`ORDER BY created`)
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, ps)
}

func parseDailyAt(v string) (h, m int, err error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, 0, fmt.Errorf("daily_at must be HH:MM in 24-hour time, got %q", v)
	}
	return t.Hour(), t.Minute(), nil
}

func (s *Server) handleSavePolicy(w http.ResponseWriter, r *http.Request) {
	var p api.Policy
	if !decode(w, r, &p) {
		return
	}
	bad := func(msg string) { fail(w, http.StatusBadRequest, msg) }
	if id := r.PathValue("id"); id != "" {
		old, err := s.policy(id)
		if err != nil {
			s.internal(w, err)
			return
		}
		if old == nil {
			fail(w, http.StatusNotFound, "no such policy")
			return
		}
		p.ID, p.Created = old.ID, old.Created
	} else {
		p.ID, p.Created = keys.RandomHex(6), time.Now()
	}
	if strings.TrimSpace(p.Name) == "" {
		bad("a policy needs a name")
		return
	}
	if len(p.Paths) == 0 && len(p.Volumes) == 0 && len(p.VMs) == 0 && !p.SystemState {
		bad("a policy needs at least one path, volume or virtual machine, or the system state")
		return
	}
	if p.VMTracking != "" && p.VMTracking != "on" && p.VMTracking != "check" {
		bad(`vm_tracking must be empty, "on" or "check"`)
		return
	}
	if p.Hypervisor != "" && !slices.Contains(api.Hypervisors, p.Hypervisor) {
		bad("hypervisor must be empty or one of: " + strings.Join(api.Hypervisors, ", "))
		return
	}
	if a, err := s.agent(p.AgentID); err == nil && a != nil && p.SystemState && a.OS != "windows" {
		bad("the system state can only be backed up on a Windows machine")
		return
	}
	if a, err := s.agent(p.AgentID); err != nil || a == nil || a.Revoked {
		bad("agent_id does not name an active agent")
		return
	}
	if p.Schedule.Interval != "" && p.Schedule.DailyAt != "" {
		bad("set either interval or daily_at, not both")
		return
	}
	if p.Schedule.Interval != "" {
		if d, err := time.ParseDuration(p.Schedule.Interval); err != nil || d < time.Minute {
			bad("interval must be a duration of at least 1m, such as 24h")
			return
		}
	}
	if p.Schedule.DailyAt != "" {
		if _, _, err := parseDailyAt(p.Schedule.DailyAt); err != nil {
			bad(err.Error())
			return
		}
	}
	rt := p.Retention
	if rt.KeepLast < 0 || rt.KeepDaily < 0 || rt.KeepWeekly < 0 || rt.KeepMonthly < 0 || rt.KeepYearly < 0 {
		bad("retention counts cannot be negative")
		return
	}
	spec, _ := json.Marshal(p)
	if _, err := s.db.Exec(`INSERT INTO policies (id, agent_id, created, spec) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET agent_id = excluded.agent_id, spec = excluded.spec`, p.ID, p.AgentID, ms(p.Created), string(spec)); err != nil {
		s.internal(w, err)
		return
	}
	s.kickBackground()
	reply(w, http.StatusOK, p)
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Exec(`DELETE FROM policies WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "no such policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// enqueueBackup queues a backup for a policy unless one is already
// queued or running.
func (s *Server) enqueueBackup(p *api.Policy) (*api.Job, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE policy_id = ? AND status IN (?, ?)`, p.ID, api.StatusQueued, api.StatusRunning).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, nil
	}
	j := &api.Job{Type: api.JobBackup, AgentID: p.AgentID, PolicyID: p.ID, Backup: &api.BackupSpec{Paths: p.Paths, Excludes: p.Excludes, Volumes: p.Volumes, VMs: p.VMs, Hypervisor: p.Hypervisor, VMTracking: p.VMTracking, SystemState: p.SystemState, NoSnapshot: p.NoSnapshot}}
	return j, s.enqueue(j)
}

func (s *Server) handleRunPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.policy(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if p == nil {
		fail(w, http.StatusNotFound, "no such policy")
		return
	}
	j, err := s.enqueueBackup(p)
	if err != nil {
		s.internal(w, err)
		return
	}
	if j == nil {
		fail(w, http.StatusConflict, "a backup for this policy is already queued or running")
		return
	}
	reply(w, http.StatusAccepted, j)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where, args := []string{"1 = 1"}, []any{}
	for _, f := range []string{"agent_id", "policy_id", "status", "type"} {
		if v := q.Get(f); v != "" {
			where = append(where, f+" = ?")
			args = append(args, v)
		}
	}
	limit := 50
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	js, err := s.jobs(`WHERE `+strings.Join(where, " AND ")+` ORDER BY created DESC LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, j := range js {
		if j.Restore != nil {
			j.Restore.Sources = nil // bulky and already in the snapshot
		}
	}
	reply(w, http.StatusOK, js)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.job(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if j == nil {
		fail(w, http.StatusNotFound, "no such job")
		return
	}
	if j.Restore != nil {
		j.Restore.Sources = nil
	}
	reply(w, http.StatusOK, j)
}

func (s *Server) snapshot(id string) *tree.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snaps[id]
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out := []*tree.Snapshot{}
	s.mu.Lock()
	for _, sn := range s.snaps {
		if (q.Get("agent_id") == "" || sn.AgentID == q.Get("agent_id")) && (q.Get("policy_id") == "" || sn.PolicyID == q.Get("policy_id")) {
			out = append(out, sn)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	reply(w, http.StatusOK, out)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	sn := s.snapshot(r.PathValue("id"))
	if sn == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	reply(w, http.StatusOK, sn)
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.snapshot(r.PathValue("id")) == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	if err := s.deleteSnapshot(r.PathValue("id")); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleList lists a directory inside a snapshot. With no path it lists
// the snapshot's sources.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	sn := s.snapshot(r.PathValue("id"))
	if sn == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	entries, err := listPath(s.repo.Get, sn, r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	out := make([]api.ListEntry, len(entries))
	for i, e := range entries {
		out[i] = api.ListEntry{Name: e.Name, Type: e.Type, Size: e.Size, ModTime: time.Unix(0, e.ModTime), Mode: e.Mode}
	}
	reply(w, http.StatusOK, out)
}

func listPath(get tree.Getter, sn *tree.Snapshot, path string) ([]tree.Entry, error) {
	if path == "" {
		return sn.Sources, nil
	}
	sep, eq := "/", func(a, b string) bool { return a == b }
	if sn.OS == "windows" {
		sep, eq = `\`, strings.EqualFold
	}
	for i := range sn.Sources {
		src := &sn.Sources[i]
		base := strings.TrimRight(src.Name, sep)
		var rest string
		switch {
		case eq(path, src.Name) || eq(strings.TrimRight(path, sep), base):
		case len(path) > len(base) && eq(path[:len(base)], base) && strings.HasPrefix(path[len(base):], sep):
			rest = strings.Trim(path[len(base):], sep)
		default:
			continue
		}
		cur := src
		if rest != "" {
			for _, name := range strings.Split(rest, sep) {
				if cur.Type != tree.TypeDir && cur.Type != tree.TypeVM {
					return nil, fmt.Errorf("%s is not a directory in this snapshot", path)
				}
				var next *tree.Entry
				for _, p := range cur.Subtree {
					es, err := tree.ReadPage(get, p)
					if err != nil {
						return nil, err
					}
					for k := range es {
						if eq(es[k].Name, name) {
							next = &es[k]
							break
						}
					}
					if next != nil {
						break
					}
				}
				if next == nil {
					return nil, fmt.Errorf("%s is not in this snapshot", path)
				}
				cur = next
			}
		}
		if cur.Type != tree.TypeDir && cur.Type != tree.TypeVM {
			return []tree.Entry{*cur}, nil
		}
		var all []tree.Entry
		for _, p := range cur.Subtree {
			es, err := tree.ReadPage(get, p)
			if err != nil {
				return nil, err
			}
			all = append(all, es...)
		}
		return all, nil
	}
	return nil, fmt.Errorf("%s is not in this snapshot", path)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req api.RestoreRequest
	if !decode(w, r, &req) {
		return
	}
	sn := s.snapshot(req.SnapshotID)
	if sn == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	if req.AgentID == "" {
		req.AgentID = sn.AgentID
	}
	a, err := s.agent(req.AgentID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if a == nil || a.Revoked {
		fail(w, http.StatusBadRequest, "agent_id does not name an active agent")
		return
	}
	switch req.Overwrite {
	case "", restore.OverwriteIfChanged, restore.OverwriteAlways, restore.OverwriteNever:
	default:
		fail(w, http.StatusBadRequest, "overwrite must be if-changed, always or never")
		return
	}
	if req.Target == "" && a.OS != sn.OS {
		fail(w, http.StatusBadRequest, fmt.Sprintf("a %s backup cannot be restored in place on a %s agent; set a target directory", sn.OS, a.OS))
		return
	}
	if req.SystemState != nil {
		has := false
		for _, e := range sn.Sources {
			has = has || e.Name == tree.SystemStateName
		}
		switch {
		case !has:
			fail(w, http.StatusBadRequest, "this snapshot holds no system state")
			return
		case req.Target != "":
			fail(w, http.StatusBadRequest, "a system state is restored in place or to a folder: leave out either system_state or target")
			return
		case a.ID != sn.AgentID:
			fail(w, http.StatusBadRequest, "a system state can only be restored in place on the machine it was backed up from")
			return
		}
		for _, dn := range req.SystemState.AuthoritativeSubtrees {
			if !strings.Contains(dn, "=") || strings.ContainsAny(dn, "\"\r\n") {
				fail(w, http.StatusBadRequest, "authoritative_subtrees must be distinguished names such as OU=Sales,DC=example,DC=com")
				return
			}
		}
		// Nothing else in the snapshot rides along.
		req.Include = []string{tree.SystemStateName}
	}
	j := &api.Job{Type: api.JobRestore, AgentID: a.ID, SnapshotID: sn.ID, Restore: &api.RestoreSpec{
		SnapshotID: sn.ID, Sources: sn.Sources, SourceOS: sn.OS, Include: req.Include, Target: req.Target, Overwrite: req.Overwrite, VMName: req.VMName,
		SystemState: req.SystemState}}
	if err := s.enqueue(j); err != nil {
		s.internal(w, err)
		return
	}
	j.Restore.Sources = nil
	reply(w, http.StatusAccepted, j)
}

func (s *Server) handleMaintenance(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j := &api.Job{Type: kind}
		if err := s.enqueue(j); err != nil {
			s.internal(w, err)
			return
		}
		reply(w, http.StatusAccepted, j)
	}
}
