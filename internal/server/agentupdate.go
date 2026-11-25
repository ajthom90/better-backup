package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// Keeping agents up to date. The server holds the agent installers it
// hands out under "Add an agent". An agent asks now and then, and each
// time it connects, whether one of them is newer than itself; the server
// answers with the installer's name and SHA-256, the agent fetches it
// over the connection it already trusts, checks it and installs it while
// no job runs. If the new version does not come back, the agent's
// updater puts the earlier program back and says so.

// updateHoldFor is how long jobs wait for an agent that said it is
// installing an update and has not been heard from since.
const updateHoldFor = 15 * time.Minute

// updateRetryAfter is how long the server waits before it offers an
// agent the same version again by itself after an update failed.
var updateRetryAfter = 24 * time.Hour

// updateHold is an agent that is installing an update: it gets no jobs,
// which the installer would cut short.
type updateHold struct {
	since time.Time
	from  string // the version it had
}

type installerSum struct {
	size  int64
	mtime time.Time
	sum   string
}

var (
	sumMu    sync.Mutex
	sumCache = map[string]installerSum{}
)

// installerSHA256 returns the SHA-256 of an installer the server holds.
func installerSHA256(path string) (string, int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	sumMu.Lock()
	defer sumMu.Unlock()
	if c, ok := sumCache[path]; ok && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.sum, c.size, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	sumCache[path] = installerSum{size: n, mtime: st.ModTime(), sum: sum}
	return sum, n, nil
}

var versionNumbers = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// parseVersion reads the three numbers a version starts with.
func parseVersion(v string) (n [3]int, ok bool) {
	m := versionNumbers.FindStringSubmatch(v)
	if m == nil {
		return n, false
	}
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	return n, true
}

// newerVersion says whether version a is newer than b. Versions that do
// not start with three numbers (a build from source says "dev") are
// neither newer nor older than anything.
func newerVersion(a, b string) bool {
	x, ok1 := parseVersion(a)
	y, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// installerFor returns the newest installer the server holds for a kind
// of machine and a kind of package.
func (s *Server) installerFor(goos, arch, format string) (name, path, version string) {
	for n, p := range s.localInstallers() {
		m := installerName.FindStringSubmatch(n)
		// The macOS package is one for both kinds of Mac.
		if m[2] != goos || m[4] != format || (m[3] != arch && m[3] != "universal") {
			continue
		}
		if name == "" || newerVersion(m[1], version) || (!newerVersion(version, m[1]) && n > name) {
			name, path, version = n, p, m[1]
		}
	}
	return
}

// newestInstaller is the newest agent version the server has an
// installer of.
func (s *Server) newestInstaller() string {
	newest := ""
	for n := range s.localInstallers() {
		if v := installerName.FindStringSubmatch(n)[1]; newest == "" || newerVersion(v, newest) {
			newest = v
		}
	}
	return newest
}

// autoUpdate is the server's setting: agents update by themselves.
func (s *Server) autoUpdate() bool { return s.setting("agent_auto_update") != "off" }

// updateRow is what the server remembers about one agent's updates.
type updateRow struct {
	install, mode, state, target, errText, failed string
	at, failedAt                                  int64
	asked                                         bool
}

func (s *Server) updateRow(agentID string) updateRow {
	var u updateRow
	err := s.db.QueryRow(`SELECT install, mode, state, target, error, at, asked, failed, failed_at FROM agent_updates WHERE agent_id = ?`, agentID).
		Scan(&u.install, &u.mode, &u.state, &u.target, &u.errText, &u.at, &u.asked, &u.failed, &u.failedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Error("reading an agent's update state", "err", err)
	}
	return u
}

// setUpdate changes columns of an agent's row, making the row first if
// there is none. set is like "state = ?, target = ?".
func (s *Server) setUpdate(agentID, set string, args ...any) {
	s.db.Exec(`INSERT OR IGNORE INTO agent_updates (agent_id) VALUES (?)`, agentID)
	if _, err := s.db.Exec(`UPDATE agent_updates SET `+set+` WHERE agent_id = ?`, append(args, agentID)...); err != nil {
		s.log.Error("saving an agent's update state", "err", err)
	}
}

func canInstall(kind string) bool {
	switch kind {
	case api.InstallMSI, api.InstallPkg, api.InstallDeb, api.InstallRPM:
		return true
	}
	return false
}

// updateState describes an agent's updates for the web UI.
func (s *Server) updateState(a *api.Agent) *api.AgentUpdateState {
	u := s.updateRow(a.ID)
	st := &api.AgentUpdateState{Install: u.install, Mode: u.mode, State: u.state, Target: u.target, Error: u.errText, Asked: u.asked}
	if u.at != 0 {
		t := fromMS(u.at)
		st.At = &t
	}
	st.Auto = u.mode == api.AutoUpdateOn || (u.mode == api.AutoUpdateDefault && s.autoUpdate())
	switch {
	case u.install == "":
		st.Why = "This agent is older than automatic updates. Install the newest agent on the machine once by hand; from then on it updates itself."
	case u.install == api.InstallContainer:
		st.Why = "This agent runs from a container image (Docker, or an app on TrueNAS). Update it where the container is set up: pull the newer image, or on TrueNAS update the app."
	case !canInstall(u.install):
		st.Why = "This agent was set up by hand, without an installer, so it cannot install a newer one. Install the newest agent on the machine by hand."
	default:
		_, _, v := s.installerFor(a.OS, a.Arch, u.install)
		switch {
		case v == "":
			st.Why = "The server has no installer for this kind of machine, so there is nothing to update from."
		case v == a.Version:
		case newerVersion(v, a.Version):
			st.Can, st.Available = true, v
		case !isNumbered(a.Version):
			// A build from source: only when someone asks for it.
			st.Can, st.Available, st.Auto = true, v, false
		}
	}
	if !st.Can {
		st.Auto = st.Auto && st.Why == ""
	}
	// A hold that ran out means the agent never came back to say how it
	// went.
	if st.State == api.UpdateInstalling && st.At != nil && time.Since(*st.At) > updateHoldFor {
		st.State, st.Error = api.UpdateFailed, "The agent started installing the update and has not been heard from since."
	}
	return st
}

func isNumbered(v string) bool { _, ok := parseVersion(v); return ok }

// handleUpdateCheck answers an agent that asks whether to update itself.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	q := r.URL.Query()
	version, install := q.Get("version"), q.Get("install")
	if version == "" {
		version = a.Version
	}
	u := s.updateRow(a.ID)
	if u.install != install {
		s.setUpdate(a.ID, "install = ?", install)
		u.install = install
	}
	// The agent is back and asking: whatever it was installing is over.
	s.releaseUpdateHold(a.ID)
	if u.state == api.UpdateInstalling && u.target == version {
		s.updateFinished(a, api.AgentUpdateReport{State: api.UpdateDone, Target: version})
		u = s.updateRow(a.ID)
	}
	if !canInstall(install) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	name, path, target := s.installerFor(a.OS, a.Arch, install)
	want := false
	switch {
	case name == "" || target == version:
	case u.asked:
		// "Update now": anything but an older version.
		want = !newerVersion(version, target)
	case newerVersion(target, version):
		want = u.mode == api.AutoUpdateOn || (u.mode == api.AutoUpdateDefault && s.autoUpdate())
		// Not the version that just failed, over and over.
		if want && u.failed == target && time.Since(fromMS(u.failedAt)) < updateRetryAfter {
			want = false
		}
	}
	if u.asked && !want {
		s.setUpdate(a.ID, "asked = 0")
	}
	if !want {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sum, size, err := installerSHA256(path)
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, api.AgentUpdate{Version: target, Name: name, Size: size, SHA256: sum})
}

func (s *Server) releaseUpdateHold(agentID string) {
	s.mu.Lock()
	_, held := s.updating[agentID]
	delete(s.updating, agentID)
	s.mu.Unlock()
	if held {
		s.wakeAgent(agentID) // its jobs may start again
	}
}

// updateHeldLocked says whether an agent is installing an update. The caller
// holds s.mu.
func (s *Server) updateHeldLocked(agentID string) bool {
	h, ok := s.updating[agentID]
	if ok && time.Since(h.since) > updateHoldFor {
		delete(s.updating, agentID)
		return false
	}
	return ok
}

// updateFinished records how an update ended.
func (s *Server) updateFinished(a *api.Agent, rep api.AgentUpdateReport) {
	now := ms(time.Now())
	if len(rep.Error) > 2000 {
		rep.Error = rep.Error[:2000]
	}
	if rep.State == api.UpdateDone {
		s.setUpdate(a.ID, "state = ?, target = ?, error = '', at = ?, asked = 0, failed = '', failed_at = 0", api.UpdateDone, rep.Target, now)
		s.audit("server", "", "An agent updated itself", a.Name+": now version "+rep.Target)
		return
	}
	s.setUpdate(a.ID, "state = ?, target = ?, error = ?, at = ?, asked = 0, failed = ?, failed_at = ?", api.UpdateFailed, rep.Target, rep.Error, now, rep.Target, now)
	s.audit("server", "", "An agent's update failed", a.Name+": version "+rep.Target+": "+rep.Error)
	s.alert("The update of the agent on "+a.Name+" failed",
		"The agent on "+a.Name+" tried to install version "+rep.Target+" and could not:\n\n"+rep.Error+
			"\n\nIt carries on with the version it had ("+a.Version+"), and its backups run as before. The server offers it the update again tomorrow; "+
			"\"Update now\" under \"Agent updates\" in the web UI tries at once. If it keeps failing, install the newest agent on the machine by hand.")
}

// handleUpdateReport hears from an agent how its update goes.
func (s *Server) handleUpdateReport(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	var rep api.AgentUpdateReport
	if !decode(w, r, &rep) {
		return
	}
	switch rep.State {
	case api.UpdateInstalling:
		// From here the installer stops the agent, so it must not hold a
		// job, and none may start until it is back.
		s.mu.Lock()
		if s.running[a.ID] != "" {
			s.mu.Unlock()
			fail(w, http.StatusConflict, "a job is running on this agent; update after it")
			return
		}
		s.updating[a.ID] = updateHold{since: time.Now(), from: rep.From}
		s.mu.Unlock()
		s.setUpdate(a.ID, "state = ?, target = ?, error = '', at = ?", api.UpdateInstalling, rep.Target, ms(time.Now()))
		s.log.Info("agent is installing an update", "agent", a.Name, "from", rep.From, "to", rep.Target)
	case api.UpdateDone, api.UpdateFailed:
		s.releaseUpdateHold(a.ID)
		s.updateFinished(a, rep)
		if rep.State == api.UpdateDone {
			s.db.Exec(`UPDATE agents SET version = ? WHERE id = ?`, rep.Target, a.ID)
		}
	default:
		fail(w, http.StatusBadRequest, "unknown state")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentUpdateSettings() api.AgentUpdateSettings {
	return api.AgentUpdateSettings{Auto: s.autoUpdate(), Newest: s.newestInstaller()}
}

func (s *Server) handleAgentUpdateSettings(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.agentUpdateSettings())
}

func (s *Server) handleSaveAgentUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req api.AgentUpdateSettings
	if !decode(w, r, &req) {
		return
	}
	v := "on"
	if !req.Auto {
		v = "off"
	}
	if err := s.setSetting("agent_auto_update", v); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "agents update by themselves: %s", v)
	if req.Auto {
		s.askAllToUpdate()
	}
	reply(w, http.StatusOK, s.agentUpdateSettings())
}

// tellAgent sends an agent something to do without waiting for what it
// says.
func (s *Server) tellAgent(agentID, action string) {
	q := s.queries.ask(agentID, api.AgentQuery{Action: action})
	go func() {
		select {
		case <-q.answer:
		case <-time.After(time.Minute):
		case <-s.stop:
		}
		s.queries.drop(q)
	}()
}

// askAllToUpdate has every connected agent ask now whether to update.
func (s *Server) askAllToUpdate() {
	rows, err := s.db.Query(`SELECT id FROM agents WHERE revoked = 0 AND recovery = 0 AND last_seen > ?`, ms(time.Now().Add(-90*time.Second)))
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.tellAgent(id, api.ActionUpdate)
	}
}

func (s *Server) liveAgent(w http.ResponseWriter, r *http.Request) *api.Agent {
	a, err := s.agent(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return nil
	}
	if a == nil || a.Revoked || a.Recovery {
		fail(w, http.StatusNotFound, "no such agent")
		return nil
	}
	return a
}

// handleAgentUpdateMode sets whether one agent updates by itself.
func (s *Server) handleAgentUpdateMode(w http.ResponseWriter, r *http.Request) {
	a := s.liveAgent(w, r)
	if a == nil {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if !decode(w, r, &req) {
		return
	}
	said := map[string]string{api.AutoUpdateDefault: "as the server's setting says", api.AutoUpdateOn: "always", api.AutoUpdateOff: "never by itself"}[req.Mode]
	if said == "" {
		fail(w, http.StatusBadRequest, `mode must be "", "on" or "off"`)
		return
	}
	s.setUpdate(a.ID, "mode = ?", req.Mode)
	note(r, "%s updates itself: %s", a.Name, said)
	if a.Online {
		s.tellAgent(a.ID, api.ActionUpdate)
	}
	reply(w, http.StatusOK, s.updateState(a))
}

// handleAgentUpdateNow has one agent install the newest version now,
// whatever the settings say.
func (s *Server) handleAgentUpdateNow(w http.ResponseWriter, r *http.Request) {
	a := s.liveAgent(w, r)
	if a == nil {
		return
	}
	st := s.updateState(a)
	if !st.Can {
		why := st.Why
		if why == "" {
			why = a.Name + " already runs the newest version the server has (" + a.Version + ")."
		}
		fail(w, http.StatusConflict, why)
		return
	}
	s.setUpdate(a.ID, "asked = 1")
	note(r, "%s: from version %s to %s", a.Name, a.Version, st.Available)
	if a.Online {
		s.tellAgent(a.ID, api.ActionUpdate)
	}
	st = s.updateState(a)
	reply(w, http.StatusAccepted, st)
}
