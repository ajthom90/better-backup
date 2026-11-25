package server

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

// Reports, graphs and the restore browser: what happened, how fast, and
// what the backups hold at a point in time.

// report turns a finished or running job into a line of a report.
func (s *Server) report(j *api.Job, names func(agentID, policyID string) (string, string)) api.Report {
	r := api.Report{ID: j.ID, Type: j.Type, Status: j.Status, PolicyID: j.PolicyID, AgentID: j.AgentID, SnapshotID: j.SnapshotID,
		Created: j.Created, Started: j.Started, Finished: j.Finished, Error: j.Error, Warnings: len(j.Warnings)}
	r.Agent, r.Policy = names(j.AgentID, j.PolicyID)
	if j.Status == api.StatusFailed {
		r.Errors = 1
	}
	if j.Started != nil {
		end := time.Now()
		if j.Finished != nil {
			end = *j.Finished
		}
		r.Seconds = end.Sub(*j.Started).Seconds()
	}
	raw := j.Result
	if len(raw) == 0 || string(raw) == "null" {
		raw = j.Progress
	}
	switch j.Type {
	case api.JobBackup:
		var st tree.Stats
		if json.Unmarshal(raw, &st) == nil {
			r.Files, r.Folders, r.Bytes = st.Files, st.Dirs, st.Bytes
			r.FilesNew, r.FilesModified, r.FilesRemoved = st.FilesNew, max(st.FilesChanged-st.FilesNew, 0), st.FilesRemoved
			r.BytesChanged, r.BytesRead, r.BytesStored = st.BytesChanged, st.BytesRead, st.BytesUploaded
			if st.Warnings > int64(r.Warnings) {
				r.Warnings = int(st.Warnings)
			}
		}
	case api.JobRestore:
		var st restore.Stats
		if json.Unmarshal(raw, &st) == nil {
			r.Files, r.Folders, r.Bytes, r.Skipped = st.Files, st.Dirs, st.Bytes, st.Skipped
			r.BytesRead = st.Bytes
			if st.Warnings > int64(r.Warnings) {
				r.Warnings = int(st.Warnings)
			}
		}
		if j.Restore != nil {
			r.Target = j.Restore.Target
			r.SnapshotID = j.Restore.SnapshotID
		}
	}
	// A run that took no time to speak of has no speed worth showing.
	if r.Seconds >= 0.05 {
		r.ReadRate = float64(r.BytesRead) / r.Seconds
		r.StoreRate = float64(r.BytesStored) / r.Seconds
		r.ScanRate = float64(r.Files+r.Folders) / r.Seconds
	}
	return r
}

// namer returns a function that names agents and policies, reading
// each once.
func (s *Server) namer() func(agentID, policyID string) (string, string) {
	agents, policies := map[string]string{}, map[string]string{}
	return func(agentID, policyID string) (string, string) {
		an, ok := agents[agentID]
		if !ok && agentID != "" {
			if a, _ := s.agent(agentID); a != nil {
				an = a.Name
			}
			agents[agentID] = an
		}
		pn, ok := policies[policyID]
		if !ok && policyID != "" {
			if p, _ := s.policy(policyID); p != nil {
				pn = p.Name
			}
			policies[policyID] = pn
		}
		return an, pn
	}
}

// dayStart reads a day ("2006-01-02") or a time as the server's clock
// has it.
func dayStart(v string) (time.Time, bool) {
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// handleReports lists backups or restores, newest first: every run with
// what it found, how long it took and how fast it went. format=csv
// gives the same as a file for a spreadsheet.
func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ := q.Get("type")
	if typ == "" {
		typ = api.JobBackup
	}
	if typ != api.JobBackup && typ != api.JobRestore {
		fail(w, http.StatusBadRequest, "type is backup or restore")
		return
	}
	where, args := []string{"type = ?"}, []any{typ}
	for _, f := range []string{"agent_id", "policy_id", "status"} {
		if v := q.Get(f); v != "" {
			where, args = append(where, f+" = ?"), append(args, v)
		}
	}
	if v := q.Get("from"); v != "" {
		t, ok := dayStart(v)
		if !ok {
			fail(w, http.StatusBadRequest, "from is a day such as 2026-01-31")
			return
		}
		where, args = append(where, "created >= ?"), append(args, ms(t))
	}
	if v := q.Get("to"); v != "" {
		t, ok := dayStart(v)
		if !ok {
			fail(w, http.StatusBadRequest, "to is a day such as 2026-01-31")
			return
		}
		if len(v) == len("2006-01-02") {
			t = t.AddDate(0, 0, 1) // through the end of that day
		}
		where, args = append(where, "created < ?"), append(args, ms(t))
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	csvOut := q.Get("format") == "csv"
	if limit <= 0 || limit > 5000 {
		limit = 200
		if csvOut {
			limit = 5000
		}
	}
	js, err := s.jobs(`WHERE `+strings.Join(where, " AND ")+` ORDER BY created DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		s.internal(w, err)
		return
	}
	names := s.namer()
	out := make([]api.Report, len(js))
	for i, j := range js {
		out[i] = s.report(j, names)
	}
	if !csvOut {
		reply(w, http.StatusOK, out)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+typ+`-report-`+time.Now().Format("2006-01-02")+`.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }
	t := func(v *time.Time) string {
		if v == nil {
			return ""
		}
		return v.Local().Format("2006-01-02 15:04:05")
	}
	if typ == api.JobBackup {
		cw.Write([]string{"Started", "Finished", "Schedule", "Machine", "Status", "Seconds", "Files", "Added", "Changed", "Removed", "Size (bytes)", "Added and changed (bytes)", "Read (bytes)", "New data stored (bytes)", "Read speed (bytes/s)", "Items per second", "Errors", "Warnings", "Error", "Snapshot"})
		for _, r := range out {
			cw.Write([]string{t(r.Started), t(r.Finished), r.Policy, r.Agent, r.Status, f(r.Seconds), n(r.Files), n(r.FilesNew), n(r.FilesModified), n(r.FilesRemoved), n(r.Bytes), n(r.BytesChanged), n(r.BytesRead), n(r.BytesStored), f(r.ReadRate), f(r.ScanRate), strconv.Itoa(r.Errors), strconv.Itoa(r.Warnings), r.Error, r.SnapshotID})
		}
		return
	}
	cw.Write([]string{"Started", "Finished", "Machine", "Status", "Seconds", "Files", "Folders", "Size (bytes)", "Skipped", "Speed (bytes/s)", "Warnings", "Error", "Restored to", "Snapshot"})
	for _, r := range out {
		cw.Write([]string{t(r.Started), t(r.Finished), r.Agent, r.Status, f(r.Seconds), n(r.Files), n(r.Folders), n(r.Bytes), n(r.Skipped), f(r.ReadRate), strconv.Itoa(r.Warnings), r.Error, r.Target, r.SnapshotID})
	}
}

// handleGraphs gives the numbers the graphs are drawn from: the size of
// the repository day by day, what the backups of each day read and
// stored, the speed of every backup, and what the offsite copy sent.
func (s *Server) handleGraphs(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 1100 {
		days = 30
	}
	now := time.Now()
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	out := api.Graphs{Days: days, Usage: []api.UsagePoint{}, Daily: []api.DayPoint{}, Runs: []api.RunPoint{}}
	rows, err := s.db.Query(`SELECT day, stored, raw, free, total FROM usage WHERE day >= ? ORDER BY day`, since.Format("2006-01-02"))
	if err != nil {
		s.internal(w, err)
		return
	}
	for rows.Next() {
		var u api.UsagePoint
		rows.Scan(&u.Day, &u.Stored, &u.Raw, &u.Free, &u.Total)
		out.Usage = append(out.Usage, u)
	}
	rows.Close()
	// What the snapshots that exist now hold, before anything is shared
	// between them: the measure the saving is taken against.
	s.mu.Lock()
	latest := map[string]*tree.Snapshot{}
	for _, sn := range s.snaps {
		k := sn.PolicyID + "/" + sn.AgentID
		if l := latest[k]; l == nil || sn.Time.After(l.Time) {
			latest[k] = sn
		}
		out.Protected += sn.Stats.Bytes
	}
	for _, sn := range latest {
		out.Sources += sn.Stats.Bytes
	}
	s.mu.Unlock()
	if st, err := s.repo.Stats(); err == nil {
		out.Stored, out.Raw = st.StoredBytes, st.RawBytes
	}

	byDay := map[string]*api.DayPoint{}
	for d := since; !d.After(now); d = d.AddDate(0, 0, 1) {
		p := &api.DayPoint{Day: d.Format("2006-01-02")}
		byDay[p.Day] = p
	}
	js, err := s.jobs(`WHERE type IN (?, ?) AND created >= ? AND started != 0 AND finished != 0 ORDER BY created`, api.JobBackup, api.JobOffsite, ms(since))
	if err != nil {
		s.internal(w, err)
		return
	}
	names := s.namer()
	for _, j := range js {
		p := byDay[j.Started.Local().Format("2006-01-02")]
		if p == nil {
			continue
		}
		secs := j.Finished.Sub(*j.Started).Seconds()
		if j.Type == api.JobOffsite {
			var res struct {
				UploadedBytes int64 `json:"uploaded_bytes"`
			}
			json.Unmarshal(j.Result, &res)
			p.OffsiteBytes += res.UploadedBytes
			p.OffsiteSeconds += secs
			continue
		}
		rep := s.report(j, names)
		p.Backups++
		switch j.Status {
		case api.StatusFailed:
			p.Failed++
		case api.StatusWarning:
			p.Warned++
		}
		p.BytesRead += rep.BytesRead
		p.BytesStored += rep.BytesStored
		p.BytesChanged += rep.BytesChanged
		p.Seconds += secs
		if j.Status == api.StatusSuccess || j.Status == api.StatusWarning {
			out.Runs = append(out.Runs, api.RunPoint{ID: j.ID, Time: *j.Started, Policy: rep.Policy, Agent: rep.Agent, Seconds: rep.Seconds,
				BytesRead: rep.BytesRead, BytesStored: rep.BytesStored, ReadRate: rep.ReadRate, ScanRate: rep.ScanRate})
		}
	}
	keys := make([]string, 0, len(byDay))
	for k := range byDay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Daily = append(out.Daily, *byDay[k])
	}
	if len(out.Runs) > 2000 {
		out.Runs = out.Runs[len(out.Runs)-2000:]
	}
	reply(w, http.StatusOK, out)
}

// handleMachineOff switches the backups of one machine of a policy off
// or on again, without touching the rest of the policy.
func (s *Server) handleMachineOff(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Off bool `json:"off"`
	}
	if !decode(w, r, &req) {
		return
	}
	p, err := s.policy(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	agentID := r.PathValue("agent")
	if p == nil || !p.Covers(agentID) {
		fail(w, http.StatusNotFound, "no such policy, or the machine is not in it")
		return
	}
	off := p.Off[:0:0]
	for _, id := range p.Off {
		if id != agentID {
			off = append(off, id)
		}
	}
	if req.Off {
		off = append(off, agentID)
	}
	p.Off, p.NextRun = off, nil
	spec, _ := json.Marshal(p)
	if _, err := s.db.Exec(`UPDATE policies SET spec = ? WHERE id = ?`, string(spec), p.ID); err != nil {
		s.internal(w, err)
		return
	}
	name := agentID
	if a, _ := s.agent(agentID); a != nil {
		name = a.Name
	}
	word := "on"
	if req.Off {
		word = "off"
	}
	note(r, "%s of %s: %s", p.Name, name, word)
	s.kickBackground()
	reply(w, http.StatusOK, p)
}

// The restore browser.

// history returns the snapshots of an agent taken at or before at,
// newest first.
func (s *Server) history(agentID string, at time.Time) []*tree.Snapshot {
	var out []*tree.Snapshot
	s.mu.Lock()
	for _, sn := range s.snaps {
		if sn.AgentID == agentID && !sn.Time.After(at) {
			out = append(out, sn)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// removedDepth is how many earlier snapshots are read for what has
// gone since.
const removedDepth = 40

func (s *Server) browseAt(r *http.Request) (agentID string, at time.Time, ok bool) {
	q := r.URL.Query()
	agentID, at = q.Get("agent_id"), time.Now()
	if v := q.Get("at"); v != "" {
		t, good := dayStart(v)
		if !good {
			return "", at, false
		}
		if len(v) == len("2006-01-02") {
			t = t.AddDate(0, 0, 1).Add(-time.Second) // the end of that day
		}
		at = t
	}
	return agentID, at, agentID != ""
}

// handleBrowse lists a folder of a machine as its backups had it at a
// point in time, across all its policies: each entry comes from the
// newest snapshot at or before that time that holds it. With no path it
// lists what was backed up. removed=1 adds what earlier snapshots held
// there and the newest do not.
func (s *Server) handleBrowseAt(w http.ResponseWriter, r *http.Request) {
	agentID, at, ok := s.browseAt(r)
	if !ok {
		fail(w, http.StatusBadRequest, "name the machine with agent_id, and the day with at, such as 2026-01-31")
		return
	}
	path := r.URL.Query().Get("path")
	snaps := s.history(agentID, at)
	out := api.RestoreListing{Path: path, At: at, Entries: []api.RestoreEntry{}}
	if len(snaps) == 0 {
		reply(w, http.StatusOK, out)
		return
	}
	out.OS = snaps[0].OS
	done, err := s.beginRead()
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	defer done()
	fold := func(n string) string { return n }
	if out.OS == "windows" {
		fold = strings.ToLower
	}
	// The newest snapshot of each policy shows the machine as it was;
	// the ones before only add what has gone.
	current := map[string]bool{}
	seen := map[string]int{}
	found := false
	withRemoved := r.URL.Query().Get("removed") == "1"
	for i, sn := range snaps {
		isCurrent := !current[sn.PolicyID]
		current[sn.PolicyID] = true
		if !isCurrent && (!withRemoved || i >= removedDepth) {
			if !withRemoved {
				continue
			}
			out.More = true
			break
		}
		es, err := listPath(s.repo.Get, sn, path)
		if err != nil {
			continue // this snapshot does not hold the folder
		}
		if path != "" {
			if e, err := findEntry(s.repo.Get, sn, path); err != nil || !isFolder(e) {
				continue
			}
		}
		found = true
		for k := range es {
			e := &es[k]
			key := fold(e.Name)
			if at, ok := seen[key]; ok {
				if !out.Entries[at].Removed {
					out.Entries[at].Snapshots++
				}
				continue
			}
			seen[key] = len(out.Entries)
			out.Entries = append(out.Entries, api.RestoreEntry{Name: e.Name, Type: e.Type, Size: e.Size, ModTime: time.Unix(0, e.ModTime),
				SnapshotID: sn.ID, SnapshotTime: sn.Time, Removed: !isCurrent, Snapshots: 1})
		}
	}
	if !found && path != "" {
		fail(w, http.StatusNotFound, "no backup of this machine holds that folder at that time")
		return
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := &out.Entries[i], &out.Entries[j]
		if fa, fb := a.Type != tree.TypeFile && a.Type != tree.TypeSymlink, b.Type != tree.TypeFile && b.Type != tree.TypeSymlink; fa != fb {
			return fa
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	reply(w, http.StatusOK, out)
}

// handleVersions lists the versions the backups of a machine hold of
// one path, newest first: a run of snapshots in which it did not change
// is one version.
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	agentID, at, ok := s.browseAt(r)
	path := r.URL.Query().Get("path")
	if !ok || path == "" {
		fail(w, http.StatusBadRequest, "name the machine with agent_id and the file with path")
		return
	}
	done, err := s.beginRead()
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	defer done()
	out := []api.FileVersion{}
	snaps := s.history(agentID, at)
	const most = 400
	for i, sn := range snaps {
		if i >= most {
			break
		}
		e, err := findEntry(s.repo.Get, sn, path)
		if err != nil {
			continue
		}
		v := api.FileVersion{SnapshotID: sn.ID, SnapshotTime: sn.Time, Oldest: sn.Time, Type: e.Type, Size: e.Size, ModTime: time.Unix(0, e.ModTime), Snapshots: 1}
		if n := len(out); n > 0 && out[n-1].Type == v.Type && out[n-1].Size == v.Size && out[n-1].ModTime.Equal(v.ModTime) && !isFolder(e) {
			out[n-1].Snapshots++
			out[n-1].Oldest = sn.Time
			continue
		}
		out = append(out, v)
	}
	reply(w, http.StatusOK, out)
}

// handleLargeFiles lists the largest files in the newest backups, from
// the index of file names.
func (s *Server) handleLargeFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out := api.LargeFiles{Files: []api.SearchHit{}}
	if s.index == nil || s.index.Meta("built") == "" {
		out.Building = true
		reply(w, http.StatusOK, out)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	hits, err := s.index.Largest(r.Context(), q.Get("agent_id"), strings.TrimSpace(q.Get("q")), limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hits {
		sn := s.snaps[h.Newest]
		if sn == nil {
			continue
		}
		sep := "/"
		if sn.OS == "windows" {
			sep = `\`
		}
		full := h.Path[0]
		if len(h.Path) > 1 {
			full = strings.TrimRight(full, sep) + sep + strings.Join(h.Path[1:], sep)
		}
		out.Files = append(out.Files, api.SearchHit{SnapshotID: sn.ID, Time: sn.Time, Oldest: sn.Time, Snapshots: h.Snapshots, AgentID: sn.AgentID, Hostname: sn.Hostname,
			Path: full, Name: lastPart(full), Type: h.Type, Size: h.Size, ModTime: h.ModTime})
	}
	reply(w, http.StatusOK, out)
}
