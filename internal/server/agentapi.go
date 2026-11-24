package server

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/repo"
	"github.com/ajthom90/better-backup/internal/tree"
)

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if !decode(w, r, &req) {
		return
	}
	// Claim one use of the token atomically, so a single-use token
	// cannot enroll two agents that race.
	res, err := s.db.Exec(`UPDATE enroll_tokens SET uses = uses - 1 WHERE hash = ? AND uses > 0 AND expires > ?`,
		hashToken(req.Token), ms(time.Now()))
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusForbidden, "enrollment token is invalid, expired or already used")
		return
	}
	id := keys.RandomHex(8)
	cert, fp, err := s.ca.signClient([]byte(req.CSR), id)
	if err != nil {
		fail(w, http.StatusBadRequest, "certificate request rejected: "+err.Error())
		return
	}
	if req.Name == "" {
		req.Name = req.Hostname
	}
	now := ms(time.Now())
	if _, err := s.db.Exec(`INSERT INTO agents (id, name, hostname, os, arch, version, cert_fp, enrolled, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.Name, req.Hostname, req.OS, req.Arch, req.Version, fp, now, now); err != nil {
		s.internal(w, err)
		return
	}
	s.recoveryEnrolled(hashToken(req.Token), id)
	s.log.Info("agent enrolled", "agent", id, "name", req.Name, "os", req.OS, "remote", r.RemoteAddr)
	k := s.repo.Keys()
	reply(w, http.StatusOK, api.EnrollResponse{AgentID: id, Cert: string(cert), IDKey: k.ID[:]})
}

// authAgent identifies the agent from its verified client certificate.
func (s *Server) authAgent(r *http.Request) (*api.Agent, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return nil, errors.New("a client certificate is required; enroll this agent first")
	}
	leaf := r.TLS.PeerCertificates[0]
	var fp string
	a, err := scanAgentFP(s.db.QueryRow(`SELECT `+agentCols+`, cert_fp FROM agents WHERE id = ?`, leaf.Subject.CommonName), &fp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && fp != certFingerprint(leaf.Raw)) {
		return nil, errors.New("this agent is not known to the server")
	}
	if err != nil {
		return nil, err
	}
	if a.Revoked {
		return nil, errors.New("this agent has been revoked")
	}
	return a, nil
}

func scanAgentFP(row *sql.Row, fp *string) (*api.Agent, error) {
	var a api.Agent
	var enrolled, seen int64
	if err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &enrolled, &seen, &a.Revoked, &a.Recovery, fp); err != nil {
		return nil, err
	}
	a.EnrolledAt, a.LastSeen = fromMS(enrolled), fromMS(seen)
	return &a, nil
}

func (s *Server) handleNextJob(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	version := r.URL.Query().Get("version")
	if version == "" {
		version = a.Version
	}
	s.db.Exec(`UPDATE agents SET last_seen = ?, version = ? WHERE id = ?`, ms(time.Now()), version, a.ID)

	// An agent only asks for work when idle, so a job it still holds was
	// lost when the agent restarted.
	if orphans, _ := s.jobs(`WHERE agent_id = ? AND status = ?`, a.ID, api.StatusRunning); len(orphans) > 0 {
		for _, j := range orphans {
			s.finish(j, api.StatusFailed, "the agent restarted while this job was running", nil, nil)
		}
	}
	wait := 25 * time.Second
	if v, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && v >= 0 && v <= 55 {
		wait = time.Duration(v) * time.Second
	}
	s.mu.Lock()
	ch := s.wake[a.ID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.wake[a.ID] = ch
	}
	s.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		j, err := s.dispatch(a)
		if err != nil {
			s.internal(w, err)
			return
		}
		if j != nil {
			reply(w, http.StatusOK, j)
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.stop:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// dispatch hands the agent its oldest queued job, if jobs may start.
func (s *Server) dispatch(a *api.Agent) (*api.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maint || s.running[a.ID] != "" {
		return nil, nil
	}
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE agent_id = ? AND status = ? ORDER BY created LIMIT 1`, a.ID, api.StatusQueued))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if _, err := s.db.Exec(`UPDATE jobs SET status = ?, started = ?, heartbeat = ? WHERE id = ?`, api.StatusRunning, ms(now), ms(now), j.ID); err != nil {
		return nil, err
	}
	j.Status, j.Started = api.StatusRunning, &now
	s.running[a.ID] = j.ID
	if j.Backup != nil {
		if p := s.parentLocked(a.ID, j.PolicyID); p != nil {
			j.Backup.Parent = p.Sources
		}
		if j.Backup.VMwareServer != "" {
			j.Backup.Connection = s.vmwareConnection(j.Backup.VMwareServer)
		}
	}
	if j.Restore != nil && j.Restore.VMwareServer != "" {
		j.Restore.Connection = s.vmwareConnection(j.Restore.VMwareServer)
	}
	s.log.Info("job started", "job", j.ID, "type", j.Type, "agent", a.Name)
	return j, nil
}

// parentLocked returns the newest snapshot of the same agent and policy.
func (s *Server) parentLocked(agentID, policyID string) *tree.Snapshot {
	var best *tree.Snapshot
	for _, sn := range s.snaps {
		if sn.AgentID == agentID && sn.PolicyID == policyID && (best == nil || sn.Time.After(best.Time)) {
			best = sn
		}
	}
	return best
}

// runningJob checks that the request is for the agent's current job.
func (s *Server) runningJob(w http.ResponseWriter, r *http.Request, a *api.Agent) (string, bool) {
	id := r.PathValue("id")
	s.mu.Lock()
	cur := s.running[a.ID]
	s.mu.Unlock()
	if cur == "" || cur != id {
		fail(w, http.StatusConflict, "this job is no longer running")
		return "", false
	}
	return id, true
}

func (s *Server) busy(w http.ResponseWriter, a *api.Agent) bool {
	s.mu.Lock()
	cur := s.running[a.ID]
	s.mu.Unlock()
	if cur == "" {
		fail(w, http.StatusConflict, "chunks can only be transferred during a running job")
		return false
	}
	return true
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	id, ok := s.runningJob(w, r, a)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.db.Exec(`UPDATE jobs SET progress = ?, heartbeat = ? WHERE id = ?`, string(body), ms(time.Now()), id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	id, ok := s.runningJob(w, r, a)
	if !ok {
		return
	}
	var req api.CompleteRequest
	if !decode(w, r, &req) {
		return
	}
	j, err := s.job(id)
	if err != nil || j == nil {
		s.internal(w, errors.Join(err, errors.New("job record missing")))
		return
	}
	status := api.StatusSuccess
	if len(req.Warnings) > 0 {
		status = api.StatusWarning
	}
	switch {
	case req.Error != "":
		err = s.finish(j, api.StatusFailed, req.Error, nil, req.Warnings)
	case j.Type == api.JobBackup:
		var sn *tree.Snapshot
		if sn, err = s.commitSnapshot(a, j, &req); err != nil {
			s.finish(j, api.StatusFailed, err.Error(), nil, req.Warnings)
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		j.SnapshotID = sn.ID
		if req.Stats != nil && req.Stats.Warnings > 0 {
			status = api.StatusWarning
		}
		err = s.finish(j, status, "", sn.Stats, req.Warnings)
		s.applyRetention(j.PolicyID)
	default:
		if req.Restore != nil && req.Restore.Warnings > 0 {
			status = api.StatusWarning
		}
		err = s.finish(j, status, "", req.Restore, req.Warnings)
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// commitSnapshot records a finished backup. The chunks the snapshot's
// roots point at must exist, so an agent cannot commit a snapshot it
// never uploaded.
func (s *Server) commitSnapshot(a *api.Agent, j *api.Job, req *api.CompleteRequest) (*tree.Snapshot, error) {
	if len(req.Sources) == 0 {
		return nil, errors.New("the backup contained no sources")
	}
	for i := range req.Sources {
		e := &req.Sources[i]
		for _, ids := range [][]keys.ID{e.Subtree, e.Content, e.ContentIndex} {
			if e.Type == tree.TypeImage {
				// The zero id marks a block of an image that holds
				// nothing; there is no chunk behind it.
				ids = slices.DeleteFunc(slices.Clone(ids), func(id keys.ID) bool { return id == keys.ID{} })
			}
			missing, err := s.repo.Missing(ids)
			if err != nil {
				return nil, err
			}
			if len(missing) > 0 {
				return nil, errors.New("the backup refers to data that was never uploaded: " + e.Name)
			}
		}
	}
	sn := &tree.Snapshot{ID: keys.RandomHex(8), Time: time.Now().UTC(), AgentID: a.ID, Hostname: a.Hostname,
		OS: a.OS, PolicyID: j.PolicyID, JobID: j.ID, Sources: req.Sources}
	if req.Stats != nil {
		sn.Stats = *req.Stats
	}
	if err := s.repo.SaveSnapshot(sn); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.snaps[sn.ID] = sn
	s.mu.Unlock()
	return sn, nil
}

func (s *Server) handleMissing(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	if !s.busy(w, a) {
		return
	}
	var req struct {
		IDs []keys.ID `json:"ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.IDs) > 8192 {
		fail(w, http.StatusBadRequest, "too many ids in one request")
		return
	}
	missing, err := s.repo.Missing(req.IDs)
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, map[string][]keys.ID{"missing": missing})
}

func (s *Server) handlePutChunk(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	if !s.busy(w, a) {
		return
	}
	id, err := keys.ParseID(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, repo.MaxChunkSize+1<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := s.dec.DecodeAll(body, nil)
	if err != nil {
		fail(w, http.StatusBadRequest, "chunk is not valid zstd: "+err.Error())
		return
	}
	if err := s.repo.Put(id, data); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetChunk(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	if !s.busy(w, a) {
		return
	}
	id, err := keys.ParseID(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := s.repo.Get(id)
	if errors.Is(err, repo.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zstd")
	w.Write(s.enc.EncodeAll(data, nil))
}

// applyRetention forgets snapshots of a policy that its rules no longer
// keep. Their data is reclaimed by the next garbage collection.
func (s *Server) applyRetention(policyID string) {
	if policyID == "" {
		return
	}
	p, err := s.policy(policyID)
	if err != nil || p == nil {
		return
	}
	s.mu.Lock()
	var snaps []*tree.Snapshot
	for _, sn := range s.snaps {
		if sn.PolicyID == policyID {
			snaps = append(snaps, sn)
		}
	}
	s.mu.Unlock()
	for _, sn := range forget(snaps, p.Retention) {
		if err := s.deleteSnapshot(sn.ID); err != nil {
			s.log.Error("retention: could not forget snapshot", "snapshot", sn.ID, "err", err)
			continue
		}
		s.log.Info("retention: forgot snapshot", "snapshot", sn.ID, "policy", p.Name, "time", sn.Time)
	}
}

func (s *Server) deleteSnapshot(id string) error {
	if err := s.repo.DeleteSnapshot(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.snaps, id)
	s.mu.Unlock()
	return s.setSetting("gc_needed", "1")
}

// forget returns the snapshots a retention policy does not keep. A
// policy with no rules keeps everything, and the newest snapshot is
// always kept.
func forget(snaps []*tree.Snapshot, r api.Retention) []*tree.Snapshot {
	if r.KeepLast <= 0 && r.KeepDaily <= 0 && r.KeepWeekly <= 0 && r.KeepMonthly <= 0 && r.KeepYearly <= 0 {
		return nil
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	rules := []struct {
		n      int
		bucket func(time.Time) string
		last   string
	}{
		{r.KeepLast, func(t time.Time) string { return t.Format(time.RFC3339Nano) }, ""},
		{r.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") }, ""},
		{r.KeepWeekly, func(t time.Time) string { y, w := t.ISOWeek(); return strconv.Itoa(y) + "-" + strconv.Itoa(w) }, ""},
		{r.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") }, ""},
		{r.KeepYearly, func(t time.Time) string { return t.Format("2006") }, ""},
	}
	var drop []*tree.Snapshot
	for i, sn := range snaps {
		keep := i == 0
		t := sn.Time.Local()
		for k := range rules {
			rule := &rules[k]
			if b := rule.bucket(t); rule.n > 0 && b != rule.last {
				rule.n--
				rule.last = b
				keep = true
			}
		}
		if !keep {
			drop = append(drop, sn)
		}
	}
	return drop
}
