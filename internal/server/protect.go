package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/tree"
)

// This file guards the backups against being deleted on purpose: by
// ransomware that reached a backed-up machine, or by someone who got hold
// of an administrator's password.
//
//   - A snapshot younger than the retention lock cannot be deleted at all.
//   - A deleted snapshot is only set aside. Its data stays, and it can be
//     brought back, until the delay has passed.
//   - Deleting needs an administrator who types their password again.
//   - Settings that weaken any of this take effect days later, after
//     everyone who gets alerts has been told, and can be cancelled.
//
// Agents never could delete anything: their API has no call for it. What
// an agent can do is finish a backup, which makes the policy's retention
// rules forget older snapshots; those now go through the same delay.

const (
	protectionKey        = "protection"
	protectionPendingKey = "protection_pending"
	// protectionWait is how long a change that weakens the protection
	// waits before it takes effect.
	protectionWait = 72 * time.Hour
	maxProtectDays = 3650
)

// retentionUser is who the list of deleted snapshots names when a
// policy's retention rules deleted one.
const retentionUser = "retention"

type pendingProtection struct {
	Settings api.ProtectionSettings `json:"settings"`
	At       time.Time              `json:"at"`
	By       string                 `json:"by"`
}

// protection returns the settings in force. A server that was never
// told otherwise keeps deleted snapshots for a week and watches backups.
func (s *Server) protection() api.ProtectionSettings {
	p := api.ProtectionSettings{DeleteDelayDays: 7, Detect: true}
	if v := s.setting(protectionKey); v != "" {
		json.Unmarshal([]byte(v), &p)
	}
	return p
}

func (s *Server) pendingProtection() *pendingProtection {
	v := s.setting(protectionPendingKey)
	if v == "" {
		return nil
	}
	var p pendingProtection
	if json.Unmarshal([]byte(v), &p) != nil {
		return nil
	}
	return &p
}

func (s *Server) protectionWait() time.Duration {
	switch {
	case s.cfg.ProtectionWait < 0:
		return 0
	case s.cfg.ProtectionWait > 0:
		return s.cfg.ProtectionWait
	}
	return protectionWait
}

func (s *Server) protectionStatus() api.ProtectionStatus {
	st := api.ProtectionStatus{ProtectionSettings: s.protection(), WaitHours: int(s.protectionWait() / time.Hour)}
	if p := s.pendingProtection(); p != nil {
		st.Pending, st.PendingAt, st.PendingBy = &p.Settings, &p.At, p.By
	}
	s.mu.Lock()
	st.Deleted = len(s.trashed)
	s.mu.Unlock()
	s.db.QueryRow(`SELECT COUNT(*) FROM suspicions WHERE reviewed = 0`).Scan(&st.Suspicious)
	return st
}

func (s *Server) handleProtection(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.protectionStatus())
}

func describeProtection(p api.ProtectionSettings) string {
	days := func(n int) string {
		if n == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", n)
	}
	lock, delay, detect := "no retention lock", "deleted snapshots are gone at once", "backups are not checked for ransomware"
	if p.LockDays > 0 {
		lock = "snapshots locked for " + days(p.LockDays)
	}
	if p.DeleteDelayDays > 0 {
		delay = "deleted snapshots kept " + days(p.DeleteDelayDays)
	}
	if p.Detect {
		detect = "backups are checked for ransomware"
	}
	return lock + "; " + delay + "; " + detect
}

// handleSaveProtection changes the protection settings. What makes the
// protection stronger applies at once. What makes it weaker waits, so
// that a stolen password cannot switch the protection off and delete
// the backups in one visit.
func (s *Server) handleSaveProtection(w http.ResponseWriter, r *http.Request) {
	var req api.ProtectionRequest
	if !decode(w, r, &req) {
		return
	}
	want := req.ProtectionSettings
	if want.LockDays < 0 || want.LockDays > maxProtectDays || want.DeleteDelayDays < 0 || want.DeleteDelayDays > maxProtectDays {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the days must be between 0 and %d", maxProtectDays))
		return
	}
	if !s.confirmPassword(w, r, req.Password) {
		return
	}
	cur := s.protection()
	now := cur
	now.LockDays, now.DeleteDelayDays = max(cur.LockDays, want.LockDays), max(cur.DeleteDelayDays, want.DeleteDelayDays)
	now.Detect = cur.Detect || want.Detect
	wait := s.protectionWait()
	if wait == 0 {
		now = want
	}
	b, _ := json.Marshal(now)
	if err := s.setSetting(protectionKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	if now == want {
		// Nothing is left to wait for, and an older request to weaken
		// the settings is overtaken.
		s.setSetting(protectionPendingKey, "")
		note(r, "%s", describeProtection(now))
	} else {
		p := pendingProtection{Settings: want, At: time.Now().Add(wait), By: who(r).name}
		b, _ := json.Marshal(p)
		if err := s.setSetting(protectionPendingKey, string(b)); err != nil {
			s.internal(w, err)
			return
		}
		note(r, "asked for weaker protection from %s: %s", p.At.Local().Format("2006-01-02 15:04"), describeProtection(want))
		s.securityAlert("The ransomware protection is going to be weakened",
			fmt.Sprintf("%s, signed in from %s, asked for weaker protection of the backups on %s.\n\nNow:  %s\nFrom %s: %s\n\nIf this was not meant, cancel the change under “Ransomware protection” in the web UI before then, and change that user's password.",
				who(r).name, remoteHost(r), serverName(), describeProtection(now), p.At.Local().Format("2006-01-02 15:04 MST"), describeProtection(want)))
	}
	s.kickBackground()
	reply(w, http.StatusOK, s.protectionStatus())
}

func (s *Server) handleCancelProtection(w http.ResponseWriter, r *http.Request) {
	p := s.pendingProtection()
	if p == nil {
		fail(w, http.StatusNotFound, "no change is waiting")
		return
	}
	if err := s.setSetting(protectionPendingKey, ""); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "%s, asked for by %s", describeProtection(p.Settings), p.By)
	s.securityAlert("The weakening of the ransomware protection was cancelled",
		fmt.Sprintf("%s cancelled the change %s had asked for on %s. The protection stays as it is: %s.", who(r).name, p.By, serverName(), describeProtection(s.protection())))
	reply(w, http.StatusOK, s.protectionStatus())
}

// applyPendingProtection puts a waiting change into force when its time
// has come.
func (s *Server) applyPendingProtection(now time.Time) {
	p := s.pendingProtection()
	if p == nil || now.Before(p.At) {
		return
	}
	b, _ := json.Marshal(p.Settings)
	if err := s.setSetting(protectionKey, string(b)); err != nil {
		s.log.Error("applying the protection settings", "err", err)
		return
	}
	s.setSetting(protectionPendingKey, "")
	s.audit("server", "", "Weakened the ransomware protection, as asked for earlier", describeProtection(p.Settings)+", asked for by "+p.By)
	s.securityAlert("The ransomware protection was weakened",
		fmt.Sprintf("The change %s asked for is now in force on %s: %s.", p.By, serverName(), describeProtection(p.Settings)))
}

// securityAlert tells whoever gets alerts about something that touches
// the safety of the backups themselves.
func (s *Server) securityAlert(subject, body string) {
	s.alert(api.AlertSecurity, "[better-backup] Security: "+subject, body+"\n\nTime: "+time.Now().Format("2006-01-02 15:04 MST")+"\n")
}

// lockedUntil is when a snapshot may first be deleted; the zero time
// when it is not locked.
func lockedUntil(sn *tree.Snapshot, p api.ProtectionSettings, now time.Time) time.Time {
	if p.LockDays <= 0 {
		return time.Time{}
	}
	until := sn.Time.Add(time.Duration(p.LockDays) * 24 * time.Hour)
	if !until.After(now) {
		return time.Time{}
	}
	return until
}

// removeSnapshot deletes a snapshot the way the settings say: set aside
// for the delay, or gone at once. by is who did it. The caller has
// checked the retention lock.
func (s *Server) removeSnapshot(id, by string) error {
	p := s.protection()
	if p.DeleteDelayDays <= 0 {
		return s.deleteSnapshot(id)
	}
	now := time.Now()
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO trash (snapshot_id, deleted, purge, by) VALUES (?, ?, ?, ?)`,
		id, ms(now), ms(now.Add(time.Duration(p.DeleteDelayDays)*24*time.Hour)), by); err != nil {
		return err
	}
	s.mu.Lock()
	if sn := s.snaps[id]; sn != nil {
		s.trashed[id] = sn
		delete(s.snaps, id)
	}
	s.mu.Unlock()
	s.stateDirty.Store(true)
	s.kickIndex()
	return nil
}

// sortTrash moves the snapshots the database lists as deleted out of
// the live ones. It runs once, at start, and drops rows whose snapshot
// is no longer in the repository.
func (s *Server) sortTrash() error {
	rows, err := s.db.Query(`SELECT snapshot_id FROM trash`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if sn := s.snaps[id]; sn != nil {
			s.trashed[id] = sn
			delete(s.snaps, id)
		} else {
			s.db.Exec(`DELETE FROM trash WHERE snapshot_id = ?`, id)
		}
	}
	return nil
}

// purgeTrash gives up the deleted snapshots whose delay has passed.
// Those of a policy with a backup that looks like ransomware stay until
// someone has looked.
func (s *Server) purgeTrash(now time.Time) {
	rows, err := s.db.Query(`SELECT snapshot_id FROM trash WHERE purge <= ?`, ms(now))
	if err != nil {
		s.log.Error("reading the deleted snapshots", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.mu.Lock()
		sn := s.trashed[id]
		s.mu.Unlock()
		if sn != nil && s.held(sn.PolicyID) {
			continue
		}
		if sn != nil {
			if err := s.repo.DeleteSnapshot(id); err != nil {
				s.log.Error("giving up a deleted snapshot", "snapshot", id, "err", err)
				continue
			}
			s.mu.Lock()
			delete(s.trashed, id)
			s.mu.Unlock()
			s.setSetting("gc_needed", "1")
			s.log.Info("a deleted snapshot's time is up; its data will be reclaimed", "snapshot", id)
		}
		s.db.Exec(`DELETE FROM trash WHERE snapshot_id = ?`, id)
	}
}

func (s *Server) handleDeletedSnapshots(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT snapshot_id, deleted, purge, by FROM trash ORDER BY deleted DESC`)
	if err != nil {
		s.internal(w, err)
		return
	}
	type row struct {
		id             string
		deleted, purge int64
		by             string
	}
	var all []row
	for rows.Next() {
		var x row
		rows.Scan(&x.id, &x.deleted, &x.purge, &x.by)
		all = append(all, x)
	}
	rows.Close()
	names, held := map[string]string{}, map[string]bool{}
	out := []api.DeletedSnapshot{}
	for _, x := range all {
		s.mu.Lock()
		sn := s.trashed[x.id]
		s.mu.Unlock()
		if sn == nil {
			continue
		}
		if _, ok := names[sn.PolicyID]; !ok {
			names[sn.PolicyID] = ""
			if p, _ := s.policy(sn.PolicyID); p != nil {
				names[sn.PolicyID] = p.Name
			}
			held[sn.PolicyID] = s.held(sn.PolicyID)
		}
		d := api.DeletedSnapshot{ID: sn.ID, Time: sn.Time, Hostname: sn.Hostname, PolicyID: sn.PolicyID, Policy: names[sn.PolicyID], Paths: []string{},
			Bytes: sn.Stats.Bytes, Deleted: fromMS(x.deleted), By: x.by, Purge: fromMS(x.purge), Held: held[sn.PolicyID]}
		for _, e := range sn.Sources {
			d.Paths = append(d.Paths, e.Name)
		}
		out = append(out, d)
	}
	reply(w, http.StatusOK, out)
}

// handleUndelete brings a deleted snapshot back.
func (s *Server) handleUndelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	sn := s.trashed[id]
	if sn != nil {
		s.snaps[id] = sn
		delete(s.trashed, id)
	}
	s.mu.Unlock()
	if sn == nil {
		fail(w, http.StatusNotFound, "no such deleted snapshot")
		return
	}
	if _, err := s.db.Exec(`DELETE FROM trash WHERE snapshot_id = ?`, id); err != nil {
		s.internal(w, err)
		return
	}
	s.kickIndex()
	note(r, "%s of %s, %s", id, sn.Hostname, sn.Time.Local().Format("2006-01-02 15:04"))
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteSnapshot deletes a snapshot by hand.
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sn := s.snapshot(id)
	if sn == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	p := s.protection()
	if until := lockedUntil(sn, p, time.Now()); !until.IsZero() {
		fail(w, http.StatusConflict, fmt.Sprintf("This snapshot is locked until %s: the retention lock keeps every snapshot for %d days.", until.Local().Format("2006-01-02 15:04"), p.LockDays))
		return
	}
	if !s.confirmPassword(w, r, r.Header.Get("X-BB-Password")) {
		return
	}
	if err := s.removeSnapshot(id, who(r).name); err != nil {
		s.internal(w, err)
		return
	}
	// A deleted backup needs no more looking at.
	s.db.Exec(`UPDATE suspicions SET reviewed = ?, reviewed_by = ? WHERE snapshot_id = ? AND reviewed = 0`, ms(time.Now()), who(r).name+" (deleted it)", id)
	what := fmt.Sprintf("snapshot %s of %s, taken %s", id, sn.Hostname, sn.Time.Local().Format("2006-01-02 15:04"))
	after := "It is gone for good: the server is set to keep deleted snapshots for 0 days."
	if p.DeleteDelayDays > 0 {
		after = fmt.Sprintf("It can be brought back under “Ransomware protection” in the web UI for %d days.", p.DeleteDelayDays)
	}
	s.securityAlert("A snapshot was deleted", fmt.Sprintf("%s, signed in from %s, deleted %s on %s.\n\n%s", who(r).name, remoteHost(r), what, serverName(), after))
	w.WriteHeader(http.StatusNoContent)
}

// held reports whether a policy has a backup that looks like ransomware
// and that nobody has looked at. Nothing of such a policy is forgotten.
func (s *Server) held(policyID string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM suspicions WHERE policy_id = ? AND reviewed = 0`, policyID).Scan(&n)
	return n > 0
}

func (s *Server) suspicions(where string, args ...any) ([]api.Suspicion, error) {
	rows, err := s.db.Query(`SELECT snapshot_id, policy_id, agent_id, time, reasons, last_good, reviewed_by, reviewed FROM suspicions `+where, args...)
	if err != nil {
		return nil, err
	}
	out := []api.Suspicion{}
	for rows.Next() {
		var x api.Suspicion
		var at, reviewed int64
		var reasons string
		if err := rows.Scan(&x.SnapshotID, &x.PolicyID, &x.AgentID, &at, &reasons, &x.LastGood, &x.ReviewedBy, &reviewed); err != nil {
			rows.Close()
			return nil, err
		}
		x.Time = fromMS(at)
		json.Unmarshal([]byte(reasons), &x.Reasons)
		if reviewed != 0 {
			t := fromMS(reviewed)
			x.Reviewed = &t
		}
		out = append(out, x)
	}
	rows.Close()
	for i := range out {
		if p, _ := s.policy(out[i].PolicyID); p != nil {
			out[i].Policy = p.Name
		}
		if a, _ := s.agent(out[i].AgentID); a != nil {
			out[i].Agent = a.Name
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, rows.Err()
}

func (s *Server) handleSuspicions(w http.ResponseWriter, r *http.Request) {
	out, err := s.suspicions(`WHERE reviewed = 0 OR time > ?`, ms(time.Now().Add(-90*24*time.Hour)))
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, out)
}

// handleReviewSuspicion records that someone looked at a backup that
// seemed wrong and found it fine. The policy's retention goes on.
func (s *Server) handleReviewSuspicion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.db.Exec(`UPDATE suspicions SET reviewed = ?, reviewed_by = ? WHERE snapshot_id = ? AND reviewed = 0`, ms(time.Now()), who(r).name, id)
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "no such backup is waiting to be looked at")
		return
	}
	var policy string
	if err := s.db.QueryRow(`SELECT policy_id FROM suspicions WHERE snapshot_id = ?`, id).Scan(&policy); err == nil || errors.Is(err, sql.ErrNoRows) {
		if p, _ := s.policy(policy); p != nil {
			note(r, "snapshot %s of policy %s", id, p.Name)
		}
	}
	s.stateDirty.Store(true)
	s.kickBackground()
	w.WriteHeader(http.StatusNoContent)
}

// suspect records what was found wrong with a backup and tells people.
func (s *Server) suspect(sn *tree.Snapshot, parent *tree.Snapshot, a *api.Agent, reasons []string) {
	b, _ := json.Marshal(reasons)
	last := ""
	if parent != nil {
		last = parent.ID
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO suspicions (snapshot_id, policy_id, agent_id, time, reasons, last_good) VALUES (?, ?, ?, ?, ?, ?)`,
		sn.ID, sn.PolicyID, sn.AgentID, ms(sn.Time), string(b), last); err != nil {
		s.log.Error("recording a suspicious backup", "err", err)
	}
	s.stateDirty.Store(true)
	name := sn.PolicyID
	if p, _ := s.policy(sn.PolicyID); p != nil {
		name = p.Name
	}
	var body strings.Builder
	fmt.Fprintf(&body, "The backup of %s that just finished (policy %s, snapshot %s) looks as if ransomware had been at the files:\n\n", a.Name, name, sn.ID)
	for _, r := range reasons {
		fmt.Fprintf(&body, "  - %s\n", r)
	}
	body.WriteString("\nUntil someone has looked, the server forgets no older backup of this policy, whatever its retention rules say.\n")
	if parent != nil {
		fmt.Fprintf(&body, "The last backup from before this one is snapshot %s, taken %s.\n", parent.ID, parent.Time.Local().Format("2006-01-02 15:04"))
	}
	body.WriteString("\nLook at the machine. If the change was expected, choose “This was expected” under “Ransomware protection” in the web UI. If it was not, restore from an older snapshot.\n")
	s.securityAlert("A backup of "+a.Name+" looks like ransomware", body.String())
	s.log.Warn("a backup looks like ransomware", "snapshot", sn.ID, "agent", a.Name, "reasons", strings.Join(reasons, " "))
}
