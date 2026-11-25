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
	s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE revoked = 0 AND recovery = 0`).Scan(&out.Agents)
	s.mu.Lock()
	out.Snapshots, out.RunningJobs = len(s.snaps), len(s.running)
	s.mu.Unlock()
	out.KeySaved = s.setting("key_saved") != ""
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
	rows.Close()
	for _, a := range out {
		if !a.Revoked && !a.Recovery {
			a.Update = s.updateState(a)
		}
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
	now := time.Now()
	for _, p := range ps {
		var last int64
		s.db.QueryRow(`SELECT COALESCE(MAX(created), 0) FROM jobs WHERE policy_id = ? AND type = ?`, p.ID, api.JobBackup).Scan(&last)
		p.NextRun = nextRun(p, fromMS(last), now)
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
	if msg := s.checkM365(&p); msg != "" {
		bad(msg)
		return
	}
	if !p.BareMetal {
		p.MachineDisks = nil
	}
	p.NextRun = nil
	// The machines: agent_ids, or the single agent_id of older clients.
	var machines []string
	for _, id := range p.Machines() {
		if id != "" && !slices.Contains(machines, id) {
			machines = append(machines, id)
		}
	}
	if len(machines) == 0 {
		bad("choose at least one machine to back up")
		return
	}
	p.AgentIDs, p.AgentID = machines, machines[0]
	var agents []*api.Agent
	for _, id := range machines {
		a, err := s.agent(id)
		if err != nil || a == nil || a.Revoked || a.Recovery {
			bad("agent_id does not name an active agent")
			return
		}
		agents = append(agents, a)
	}
	if len(agents) > 1 && (p.M365 != nil || p.Hypervisor == "vmware") {
		bad("Microsoft 365 and VMware are read over the network, by one machine: choose a single machine for this policy")
		return
	}
	// What to back up, machine by machine: a machine named in
	// per_machine has its own list, the others share the policy's.
	for id, src := range p.PerMachine {
		if !slices.Contains(machines, id) || src == nil {
			delete(p.PerMachine, id)
			continue
		}
		if !src.BareMetal {
			src.MachineDisks = nil
		}
	}
	if len(p.PerMachine) == 0 {
		p.PerMachine = nil
	}
	p.Off = slices.DeleteFunc(p.Off, func(id string) bool { return !slices.Contains(machines, id) })
	if len(p.PerMachine) > 0 && (p.M365 != nil || p.Hypervisor == "vmware") {
		bad("Microsoft 365 and VMware policies have one machine and one list of what to back up")
		return
	}
	for _, a := range agents {
		src := p.SourcesFor(a.ID)
		if src.Empty() && p.M365 == nil {
			if len(agents) == 1 {
				bad("a policy needs at least one path, volume or virtual machine, the system state, the whole machine, the TrueNAS configuration or Microsoft 365")
			} else {
				bad("nothing is chosen to back up on " + a.Name + ": choose something, or take the machine out of the policy")
			}
			return
		}
		if src.BareMetal && a.OS != "windows" && a.OS != "linux" {
			bad("a whole machine can be backed up for bare-metal recovery on Windows and Linux only; " + a.Name + " runs " + a.OS)
			return
		}
		if src.SystemState && a.OS != "windows" {
			bad("the system state can only be backed up on a Windows machine; " + a.Name + " runs " + a.OS)
			return
		}
		if src.TrueNASConfig && a.OS != "linux" {
			bad("the TrueNAS configuration can only be backed up by an agent running on TrueNAS SCALE")
			return
		}
	}
	if p.VMTracking != "" && p.VMTracking != "on" && p.VMTracking != "check" {
		bad(`vm_tracking must be empty, "on" or "check"`)
		return
	}
	p.VMScratchDir = strings.TrimSpace(p.VMScratchDir)
	if p.VMScratchDir != "" && !strings.HasPrefix(p.VMScratchDir, "/") {
		bad("the scratch folder for virtual machine backups is a full path on the host, such as /mnt/fast/scratch")
		return
	}
	if p.Hypervisor != "" && !slices.Contains(api.Hypervisors, p.Hypervisor) {
		bad("hypervisor must be empty or one of: " + strings.Join(api.Hypervisors, ", "))
		return
	}
	if p.Hypervisor != "vmware" {
		p.VMwareServer = ""
	} else {
		if s.vmwareServer(p.VMwareServer) == nil {
			bad("choose the VMware server the machines are on; servers are added under VMware servers")
			return
		}
		if p.VMTracking != "" {
			bad("machines on VMware are read in full every time; leave vm_tracking empty")
			return
		}
	}
	if err := checkSchedule(&p.Schedule); err != nil {
		bad(err.Error())
		return
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
	note(r, "%s", p.Name)
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

// enqueueBackup queues a backup of one of a policy's machines unless
// one is already queued or running.
func (s *Server) enqueueBackup(p *api.Policy, agentID string) (*api.Job, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE policy_id = ? AND agent_id = ? AND status IN (?, ?)`, p.ID, agentID, api.StatusQueued, api.StatusRunning).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, nil
	}
	src := p.SourcesFor(agentID)
	j := &api.Job{Type: api.JobBackup, AgentID: agentID, PolicyID: p.ID, Backup: &api.BackupSpec{Paths: src.Paths, Excludes: p.Excludes, Volumes: src.Volumes, VMs: src.VMs, Hypervisor: p.Hypervisor, VMwareServer: p.VMwareServer, VMTracking: p.VMTracking, VMScratchDir: p.VMScratchDir, SystemState: src.SystemState, TrueNASConfig: src.TrueNASConfig, BareMetal: src.BareMetal, MachineDisks: src.MachineDisks, NoSnapshot: p.NoSnapshot, M365: p.M365}}
	return j, s.enqueue(j)
}

// enqueuePolicy queues a backup of every machine of a policy that has
// none waiting, and returns the new jobs. Machines whose agent was
// revoked, or whose backups are switched off, are left out. only, if
// set, is the one machine to back up, switched off or not.
func (s *Server) enqueuePolicy(p *api.Policy, only string) ([]*api.Job, error) {
	var out []*api.Job
	for _, id := range p.Machines() {
		if only != "" && id != only || only == "" && p.IsOff(id) {
			continue
		}
		if a, err := s.agent(id); err != nil || a == nil || a.Revoked {
			if len(p.Machines()) > 1 {
				continue
			}
		}
		j, err := s.enqueueBackup(p, id)
		if err != nil {
			return out, err
		}
		if j != nil {
			out = append(out, j)
		}
	}
	return out, nil
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
	js, err := s.enqueuePolicy(p, r.URL.Query().Get("agent_id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(js) == 0 {
		fail(w, http.StatusConflict, "a backup for this policy is already queued or running")
		return
	}
	// The first job, as before; a policy of several machines started
	// one for each, which the list of jobs shows.
	reply(w, http.StatusAccepted, js[0])
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
	cur, err := findEntry(get, sn, path)
	if err != nil {
		return nil, err
	}
	if !isFolder(cur) {
		return []tree.Entry{*cur}, nil
	}
	return children(get, cur)
}

func isFolder(e *tree.Entry) bool {
	return e.Type == tree.TypeDir || e.Type == tree.TypeVM || e.Type == tree.TypeMachine
}

// children reads everything a folder holds.
func children(get tree.Getter, e *tree.Entry) ([]tree.Entry, error) {
	var all []tree.Entry
	for _, p := range e.Subtree {
		es, err := tree.ReadPage(get, p)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
	}
	return all, nil
}

// findEntry finds what a snapshot holds at a path, written the way the
// snapshot's machine writes paths.
func findEntry(get tree.Getter, sn *tree.Snapshot, path string) (*tree.Entry, error) {
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
				if !isFolder(cur) {
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
		return cur, nil
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
	if len(req.Disks) > 0 {
		// Onto disks: everything on them is replaced, so the machine must
		// be named, and only the whole machine can go there.
		if req.AgentID == "" {
			fail(w, http.StatusBadRequest, "name the machine whose disks are to be written")
			return
		}
		if machineEntry(sn) == nil {
			fail(w, http.StatusBadRequest, "this snapshot holds no whole-machine backup")
			return
		}
		req.Include, req.Target = []string{tree.MachineName}, ""
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
	if a.Recovery && len(req.Disks) == 0 {
		fail(w, http.StatusBadRequest, "a machine started from the recovery disc can only receive a whole machine onto its disks")
		return
	}
	if req.VMwareServer == "" {
		req.VMwareServer = s.restoreVMware(sn)
	} else if s.vmwareServer(req.VMwareServer) == nil {
		fail(w, http.StatusBadRequest, "vmware_server does not name a VMware server")
		return
	}
	// A machine on VMware does not go onto the agent at all: without a
	// target it goes back to the datastore it was on.
	if req.Target == "" && len(req.Disks) == 0 && a.OS != sn.OS && req.VMwareServer == "" {
		fail(w, http.StatusBadRequest, fmt.Sprintf("a %s backup cannot be restored in place on a %s agent; set a target directory", sn.OS, a.OS))
		return
	}
	if req.Target == "" && len(req.Disks) == 0 && coversM365(sn, req.Include) {
		fail(w, http.StatusBadRequest, "mail and files from Microsoft 365 are restored into a folder: say where to put them, or choose only other parts of the backup")
		return
	}
	if req.VMReplace {
		switch {
		case len(req.Disks) > 0 || req.SystemState != nil:
			fail(w, http.StatusBadRequest, "vm_replace does not go with disks or system_state")
			return
		case !holdsVM(sn):
			fail(w, http.StatusBadRequest, "this snapshot holds no virtual machine to put back over the original")
			return
		}
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
		SnapshotID: sn.ID, Sources: sn.Sources, SourceOS: sn.OS, Include: req.Include, Target: req.Target, Overwrite: req.Overwrite, VMName: req.VMName, VMReplace: req.VMReplace, Disks: req.Disks,
		VMwareServer: req.VMwareServer, SystemState: req.SystemState}}
	if err := s.enqueue(j); err != nil {
		s.internal(w, err)
		return
	}
	j.Restore.Sources = nil
	where := "in place"
	switch {
	case len(req.Disks) > 0:
		where = "onto its disks"
	case req.Target != "":
		where = "to " + req.Target
	}
	what := "everything"
	if len(req.Include) > 0 {
		what = strings.Join(req.Include, ", ")
	}
	if req.VMReplace {
		where += ", replacing the virtual machine of the same name"
	}
	note(r, "%s from snapshot %s of %s onto %s, %s", what, sn.ID, sn.Hostname, a.Name, where)
	reply(w, http.StatusAccepted, j)
}

// holdsVM says whether a snapshot has a virtual machine among its
// sources.
func holdsVM(sn *tree.Snapshot) bool {
	for _, e := range sn.Sources {
		if e.Type == tree.TypeVM {
			return true
		}
	}
	return false
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
