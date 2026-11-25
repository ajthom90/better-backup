package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

const monitorSettingsKey = "monitor"

// monitorSettings returns the stored settings, or the defaults: check
// all data every 30 days, warn when under a tenth of the disk is free.
func (s *Server) monitorSettings() api.MonitorSettings {
	m := api.MonitorSettings{VerifyEveryDays: 30, VerifyAt: "01:00", DiskWarnPercent: 10}
	if v := s.setting(monitorSettingsKey); v != "" {
		json.Unmarshal([]byte(v), &m)
	}
	return m
}

func (s *Server) handleMonitorSettings(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.monitorSettings())
}

func (s *Server) handleSaveMonitorSettings(w http.ResponseWriter, r *http.Request) {
	var m api.MonitorSettings
	if !decode(w, r, &m) {
		return
	}
	m.VerifyAt = strings.TrimSpace(m.VerifyAt)
	if m.VerifyAt == "" {
		m.VerifyAt = "01:00"
	}
	if _, _, err := parseDailyAt(m.VerifyAt); err != nil {
		fail(w, http.StatusBadRequest, "the time must look like 01:00")
		return
	}
	if m.VerifyEveryDays < 0 || m.VerifyEveryDays > 3650 || m.DiskWarnPercent < 0 || m.DiskWarnPercent > 90 {
		fail(w, http.StatusBadRequest, "days must be between 0 and 3650, and the free space warning between 0 and 90 percent")
		return
	}
	b, _ := json.Marshal(m)
	if err := s.setSetting(monitorSettingsKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "check all data every %d days at %s, warn under %d%% free", m.VerifyEveryDays, m.VerifyAt, m.DiskWarnPercent)
	reply(w, http.StatusOK, m)
}

// handleCancelJob stops a job that is waiting or running. An agent
// notices within seconds, because the server refuses the job's next
// report or upload.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.job(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if j == nil {
		fail(w, http.StatusNotFound, "no such job")
		return
	}
	if j.Status != api.StatusQueued && j.Status != api.StatusRunning {
		fail(w, http.StatusConflict, "this job has already finished")
		return
	}
	if j.Status == api.StatusRunning && j.AgentID == "" {
		switch j.Type {
		case api.JobOffsite:
			s.mu.Lock()
			cancel := s.cancelOffsite
			s.mu.Unlock()
			if cancel != nil {
				cancel(errCancelled{who(r).name})
			}
			note(r, "offsite copy %s", j.ID)
			reply(w, http.StatusAccepted, j)
			return
		default:
			fail(w, http.StatusConflict, "Reclaiming space and checking data cannot be stopped once they have started: stopping halfway would leave the work to be done again from the start.")
			return
		}
	}
	name := "server"
	if a, _ := s.agent(j.AgentID); a != nil {
		name = a.Name
	}
	note(r, "%s %s on %s", j.Type, j.ID, name)
	if err := s.finish(j, api.StatusCancelled, "Cancelled by "+who(r).name, nil, nil); err != nil {
		s.internal(w, err)
		return
	}
	s.wakeAll()
	j, _ = s.job(j.ID)
	reply(w, http.StatusOK, j)
}

// errCancelled is why a server-side job's context ended.
type errCancelled struct{ by string }

func (e errCancelled) Error() string { return "Cancelled by " + e.by }

// scheduleVerify queues the regular check of all data when it is due.
func (s *Server) scheduleVerify(now time.Time) {
	next := s.nextVerify(now)
	if next == nil || now.Before(*next) {
		return
	}
	var busy int
	s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = ? AND status IN (?, ?)`, api.JobVerify, api.StatusQueued, api.StatusRunning).Scan(&busy)
	if busy > 0 {
		return
	}
	if err := s.enqueue(&api.Job{Type: api.JobVerify}); err != nil {
		s.log.Error("scheduling the check of all data", "err", err)
	}
}

// nextVerify is when the next regular check is due: at the set time of
// day, the set number of days after the day the last one was started,
// by hand or by this schedule. A server
// that has never run one waits for the first such time after it was
// first seen, so a new server is not checked while it is still empty.
func (s *Server) nextVerify(now time.Time) *time.Time {
	m := s.monitorSettings()
	if m.VerifyEveryDays <= 0 {
		return nil
	}
	h, min, err := parseDailyAt(m.VerifyAt)
	if err != nil {
		return nil
	}
	var last int64
	s.db.QueryRow(`SELECT COALESCE(MAX(created), 0) FROM jobs WHERE type = ?`, api.JobVerify).Scan(&last)
	from := fromMS(last)
	if from.IsZero() {
		if t := settingTime(s.setting("first_seen")); t != nil {
			from = *t
		} else {
			from = now
			s.setSetting("first_seen", now.UTC().Format(time.RFC3339))
		}
	}
	from = from.In(now.Location())
	day := from.AddDate(0, 0, m.VerifyEveryDays)
	next := time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, now.Location())
	return &next
}

func (s *Server) disk() api.DiskUsage {
	total, free, err := diskSpace(s.cfg.RepoDir)
	if err != nil || total <= 0 {
		return api.DiskUsage{}
	}
	return api.DiskUsage{Total: total, Free: free, Known: true}
}

// recordUsage keeps one row a day of how big the repository is, and
// warns once when the disk is nearly full.
func (s *Server) recordUsage(now time.Time) {
	if now.Sub(s.lastUsage) < 10*time.Minute {
		return
	}
	s.lastUsage = now
	st, err := s.repo.Stats()
	if err != nil {
		return
	}
	d := s.disk()
	s.db.Exec(`INSERT INTO usage (day, stored, raw, free, total) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET stored = excluded.stored, raw = excluded.raw, free = excluded.free, total = excluded.total`,
		now.Format("2006-01-02"), st.StoredBytes, st.RawBytes, d.Free, d.Total)
	s.db.Exec(`DELETE FROM usage WHERE day < ?`, now.AddDate(-3, 0, 0).Format("2006-01-02"))

	m := s.monitorSettings()
	low := d.Known && m.DiskWarnPercent > 0 && d.Free*100 < d.Total*int64(m.DiskWarnPercent)
	switch was := s.setting("alert_disk") != ""; {
	case low && !was:
		s.setSetting("alert_disk", "1")
		s.alert("[better-backup] The backup disk is nearly full on "+serverName(),
			fmt.Sprintf("The disk that holds the repository (%s) on %s has %s free of %s, which is less than %d%%.\n\nWhen it is full, backups fail. Add space, keep fewer snapshots in the policies, or choose \"Reclaim space\" after snapshots were deleted.\n\nThis alert is sent once, and again if the disk fills up after having had room.\n",
				s.cfg.RepoDir, serverName(), byteSize(d.Free), byteSize(d.Total), m.DiskWarnPercent))
	case !low && was && (!d.Known || m.DiskWarnPercent == 0 || d.Free*100 > d.Total*int64(m.DiskWarnPercent+2)):
		s.setSetting("alert_disk", "")
	}
}

func byteSize(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// health gathers the state of every policy and of the server itself.
func (s *Server) health(now time.Time) (*api.Health, error) {
	h := &api.Health{State: "ok", Issues: []api.HealthIssue{}, Policies: []api.PolicyHealth{}, Usage: []api.UsagePoint{}, Jobs: map[string]int{}, Disk: s.disk()}
	issue := func(level, format string, a ...any) {
		h.Issues = append(h.Issues, api.HealthIssue{Level: level, Text: fmt.Sprintf(format, a...)})
		if level == "failed" || h.State == "ok" {
			h.State = level
		}
	}
	ps, err := s.policies("")
	if err != nil {
		return nil, err
	}
	sort.Slice(ps, func(i, j int) bool { return strings.ToLower(ps[i].Name) < strings.ToLower(ps[j].Name) })
	type snapInfo struct {
		n       int
		size    int64
		newest  time.Time
		new7    int64
		hasSnap bool
	}
	bySnap := map[string]*snapInfo{}
	s.mu.Lock()
	for _, sn := range s.snaps {
		i := bySnap[sn.PolicyID+"/"+sn.AgentID]
		if i == nil {
			i = &snapInfo{}
			bySnap[sn.PolicyID+"/"+sn.AgentID] = i
		}
		i.n++
		if sn.Time.After(i.newest) {
			i.newest, i.size = sn.Time, sn.Stats.Bytes
		}
		if now.Sub(sn.Time) < 7*24*time.Hour {
			i.new7 += sn.Stats.BytesUploaded
		}
	}
	s.mu.Unlock()
	type pair struct {
		p     *api.Policy
		agent string
	}
	var pairs []pair
	for _, p := range ps {
		for _, id := range p.Machines() {
			pairs = append(pairs, pair{p, id})
		}
	}
	for _, pr := range pairs {
		p, agentID := pr.p, pr.agent
		ph := api.PolicyHealth{PolicyID: p.ID, Policy: p.Name, AgentID: agentID, Agent: agentID}
		ag, _ := s.agent(agentID)
		if ag != nil {
			ph.Agent = ag.Name
		}
		if i := bySnap[p.ID+"/"+agentID]; i != nil {
			ph.Size, ph.New7Days, ph.Snapshots = i.size, i.new7, i.n
		}
		var good int64
		s.db.QueryRow(`SELECT COALESCE(MAX(finished), 0) FROM jobs WHERE policy_id = ? AND agent_id = ? AND type = ? AND status IN (?, ?)`,
			p.ID, agentID, api.JobBackup, api.StatusSuccess, api.StatusWarning).Scan(&good)
		if good != 0 {
			t := fromMS(good)
			ph.LastGood = &t
		}
		last, _ := s.jobs(`WHERE policy_id = ? AND agent_id = ? AND type = ? AND status NOT IN (?, ?) ORDER BY created DESC LIMIT 1`, p.ID, agentID, api.JobBackup, api.StatusQueued, api.StatusCancelled)
		var running int
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE policy_id = ? AND agent_id = ? AND type = ? AND status = ?`, p.ID, agentID, api.JobBackup, api.StatusRunning).Scan(&running)
		scheduled := p.Enabled && (p.Schedule.Interval != "" || p.Schedule.DailyAt != "")
		switch {
		case ag == nil || ag.Revoked:
			ph.State, ph.Detail = "failed", "Its agent was revoked, so it cannot run."
		case running > 0 || (len(last) > 0 && last[0].Status == api.StatusRunning):
			ph.State = "running"
		case len(last) > 0 && last[0].Status == api.StatusFailed:
			ph.State, ph.Detail = "failed", last[0].Error
		case overdue(p, fromMS(good), now):
			ph.State = "overdue"
			ph.Detail = "A backup was due and did not run."
			if !ag.Online {
				ph.Detail = "A backup was due and did not run: the agent is offline, last seen " + ag.LastSeen.Local().Format("2006-01-02 15:04") + "."
			}
		case good == 0:
			ph.State = "never"
			if !scheduled {
				ph.Detail = "No backup yet, and none is scheduled."
			}
		case len(last) > 0 && last[0].Status == api.StatusWarning:
			ph.State, ph.Detail = "warning", fmt.Sprintf("The last backup finished with %d warnings, such as skipped files.", len(last[0].Warnings))
		case !scheduled:
			ph.State, ph.Detail = "paused", "Runs only when started by hand."
		default:
			ph.State = "ok"
		}
		switch ph.State {
		case "failed":
			issue("failed", "The backup \"%s\" of %s failed: %s", p.Name, ph.Agent, ph.Detail)
		case "overdue":
			issue("failed", "The backup \"%s\" of %s is overdue. %s", p.Name, ph.Agent, ph.Detail)
		case "warning":
			issue("warning", "The backup \"%s\" of %s finished with warnings.", p.Name, ph.Agent)
		case "never":
			if !scheduled {
				issue("warning", "The policy \"%s\" of %s has never run and is not scheduled.", p.Name, ph.Agent)
			}
		}
		h.Policies = append(h.Policies, ph)
	}

	// Agents nothing backs up.
	covered := map[string]bool{}
	for _, p := range ps {
		for _, id := range p.Machines() {
			covered[id] = true
		}
	}
	rows, err := s.db.Query(`SELECT id, name FROM agents WHERE revoked = 0 AND recovery = 0`)
	if err == nil {
		var bare []string
		for rows.Next() {
			var id, name string
			rows.Scan(&id, &name)
			if !covered[id] {
				bare = append(bare, name)
			}
		}
		rows.Close()
		sort.Strings(bare)
		for _, name := range bare {
			issue("warning", "%s has an agent but no backup policy.", name)
		}
	}

	m := s.monitorSettings()
	if sus, err := s.suspicions(`WHERE reviewed = 0`); err == nil {
		for _, x := range sus {
			issue("failed", "A backup of %s on %s looks like ransomware (policy \"%s\"). Older backups of the policy are kept until someone has looked at it under “Ransomware protection”.", x.Agent, x.Time.Local().Format("2006-01-02 15:04"), x.Policy)
		}
	}
	if p := s.pendingProtection(); p != nil {
		issue("warning", "The ransomware protection will be weakened on %s, as %s asked. It can be cancelled until then.", p.At.Local().Format("2006-01-02 15:04"), p.By)
	}
	if d := h.Disk; d.Known && m.DiskWarnPercent > 0 && d.Free*100 < d.Total*int64(m.DiskWarnPercent) {
		issue("failed", "The disk that holds the repository is nearly full: %s free of %s.", byteSize(d.Free), byteSize(d.Total))
	}
	if o := s.offsiteStatus(); o.Kind != "" && o.Enabled {
		switch {
		case o.LastJob != nil && o.LastJob.Status == api.StatusFailed:
			issue("failed", "The offsite copy failed: %s", o.LastJob.Error)
		case o.LastSuccess != nil && o.DailyAt != "" && now.Sub(*o.LastSuccess) > 49*time.Hour:
			issue("warning", "The offsite copy was last brought up to date on %s.", o.LastSuccess.Local().Format("2006-01-02 15:04"))
		case o.LastSuccess == nil && (o.LastJob == nil || o.LastJob.Status != api.StatusRunning):
			issue("warning", "The offsite copy is set up but has not been made yet.")
		}
	} else {
		issue("warning", "There is no offsite copy. If this server is lost, so are the backups.")
	}
	if s.setting("key_saved") == "" {
		issue("warning", "Nobody has downloaded the repository key. Without a copy of it the backups cannot be read after the server is lost.")
	}
	if a := s.alertSettings(); !a.Enabled && a.Webhook == "" {
		issue("warning", "Email alerts are off and no webhook is set, so nobody is told when a backup fails.")
	}
	if vs, _ := s.jobs(`WHERE type = ? AND status IN (?, ?) ORDER BY created DESC LIMIT 1`, api.JobVerify, api.StatusSuccess, api.StatusFailed); len(vs) > 0 {
		if vs[0].Status == api.StatusFailed {
			issue("failed", "The last check of all data found a problem: %s", vs[0].Error)
		}
	}
	if vs, _ := s.jobs(`WHERE type = ? AND status = ? ORDER BY created DESC LIMIT 1`, api.JobVerify, api.StatusSuccess); len(vs) > 0 {
		h.LastVerify = vs[0].Finished
	}
	h.NextVerify = s.nextVerify(now)

	urows, err := s.db.Query(`SELECT day, stored, free FROM (SELECT day, stored, free FROM usage ORDER BY day DESC LIMIT 90) ORDER BY day`)
	if err == nil {
		for urows.Next() {
			var u api.UsagePoint
			urows.Scan(&u.Day, &u.Stored, &u.Free)
			h.Usage = append(h.Usage, u)
		}
		urows.Close()
	}
	if n := len(h.Usage); n >= 2 {
		first := h.Usage[max(0, n-31)]
		lastU := h.Usage[n-1]
		a, _ := time.Parse("2006-01-02", first.Day)
		b, _ := time.Parse("2006-01-02", lastU.Day)
		if days := int64(b.Sub(a).Hours() / 24); days > 0 {
			h.GrowthPerDay = (lastU.Stored - first.Stored) / days
			if h.GrowthPerDay > 0 && h.Disk.Known {
				h.DaysLeft = int(min(h.Disk.Free/h.GrowthPerDay, 100000))
			}
		}
	}
	jrows, err := s.db.Query(`SELECT status, COUNT(*) FROM jobs WHERE finished > ? GROUP BY status`, ms(now.Add(-24*time.Hour)))
	if err == nil {
		for jrows.Next() {
			var st string
			var n int
			jrows.Scan(&st, &n)
			h.Jobs[st] = n
		}
		jrows.Close()
	}
	return h, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h, err := s.health(time.Now())
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, h)
}

// summaryText is the report sent by email: what needs attention first,
// then every policy, then the repository.
func (s *Server) summaryText(now time.Time, period string) (subject, body string, err error) {
	h, err := s.health(now)
	if err != nil {
		return "", "", err
	}
	since := now.Add(-24 * time.Hour)
	if period == "weekly" {
		since = now.AddDate(0, 0, -7)
	}
	counts := map[string]int{}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM jobs WHERE type = ? AND finished > ? GROUP BY status`, api.JobBackup, ms(since))
	if err == nil {
		for rows.Next() {
			var st string
			var n int
			rows.Scan(&st, &n)
			counts[st] = n
		}
		rows.Close()
	}
	verdict := map[string]string{"ok": "All backups are in order", "warning": "Backups need a look", "failed": "Backups need attention"}[h.State]
	var b strings.Builder
	fmt.Fprintf(&b, "%s on the better-backup server %s.\n\n", verdict, serverName())
	span := "24 hours"
	if period == "weekly" {
		span = "7 days"
	}
	fmt.Fprintf(&b, "Backups in the last %s: %d succeeded, %d with warnings, %d failed.\n", span, counts[api.StatusSuccess], counts[api.StatusWarning], counts[api.StatusFailed])
	if len(h.Issues) > 0 {
		b.WriteString("\nNeeds attention:\n")
		for _, i := range h.Issues {
			fmt.Fprintf(&b, "  - %s\n", i.Text)
		}
	}
	if len(h.Policies) > 0 {
		b.WriteString("\nEvery backup:\n")
		for _, p := range h.Policies {
			last := "never"
			if p.LastGood != nil {
				last = p.LastGood.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&b, "  %-8s %s on %s. Last good backup: %s. Size %s, %d snapshots.\n", strings.ToUpper(p.State), p.Policy, p.Agent, last, byteSize(p.Size), p.Snapshots)
		}
	}
	if st, serr := s.repo.Stats(); serr == nil {
		fmt.Fprintf(&b, "\nRepository: %s used", byteSize(st.StoredBytes))
		if h.Disk.Known {
			fmt.Fprintf(&b, ", %s free of %s", byteSize(h.Disk.Free), byteSize(h.Disk.Total))
		}
		b.WriteString(".\n")
		if h.GrowthPerDay > 0 {
			fmt.Fprintf(&b, "It grows by about %s a day", byteSize(h.GrowthPerDay))
			if h.DaysLeft > 0 {
				fmt.Fprintf(&b, "; at that rate the disk is full in about %d days", h.DaysLeft)
			}
			b.WriteString(".\n")
		}
	}
	if h.LastVerify != nil {
		fmt.Fprintf(&b, "All data was last checked and found good on %s.\n", h.LastVerify.Local().Format("2006-01-02"))
	}
	name := map[string]string{"daily": "Daily", "weekly": "Weekly"}[period]
	return fmt.Sprintf("[better-backup] %s summary: %s", name, strings.ToLower(verdict)), b.String(), nil
}

// sendSummary mails the daily or weekly report when its time has come.
func (s *Server) sendSummary(now time.Time) {
	a := s.alertSettings()
	if a.Summary == "" || (!a.Enabled && a.Webhook == "") {
		return
	}
	at := a.SummaryAt
	if at == "" {
		at = "07:00"
	}
	h, m, err := parseDailyAt(at)
	if err != nil {
		return
	}
	slot := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if now.Before(slot) {
		slot = slot.AddDate(0, 0, -1)
	}
	if a.Summary == "weekly" {
		for slot.Weekday() != time.Monday {
			slot = slot.AddDate(0, 0, -1)
		}
	}
	key := slot.Format("2006-01-02 15:04")
	sent := s.setting("summary_sent")
	if sent == "" {
		// Newly turned on: the first report goes out at the next slot.
		s.setSetting("summary_sent", key)
		return
	}
	if sent >= key {
		return
	}
	s.setSetting("summary_sent", key)
	subject, body, err := s.summaryText(now, a.Summary)
	if err != nil {
		s.log.Error("writing the summary", "err", err)
		return
	}
	s.alert(subject, body)
}

func (s *Server) handleTestSummary(w http.ResponseWriter, r *http.Request) {
	a := s.alertSettings()
	period := a.Summary
	if period == "" {
		period = "daily"
	}
	subject, body, err := s.summaryText(time.Now(), period)
	if err != nil {
		s.internal(w, err)
		return
	}
	if r.URL.Query().Get("send") == "" {
		reply(w, http.StatusOK, map[string]string{"subject": subject, "body": body})
		return
	}
	if a.Host == "" || a.From == "" || len(a.To) == 0 {
		fail(w, http.StatusBadRequest, "save the mail server, from address and recipients first")
		return
	}
	if err := sendMail(a, subject, body); err != nil {
		fail(w, http.StatusBadGateway, "the message was not sent: "+err.Error())
		return
	}
	reply(w, http.StatusOK, map[string]string{"subject": subject, "body": body})
}

func checkWebhook(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the webhook must be an address that starts with https:// or http://")
	}
	return nil
}

// postWebhook sends one alert as JSON. "text" is what Slack, Microsoft
// Teams and Mattermost show, "content" what Discord shows; the other
// fields are for anything that wants the parts.
func postWebhook(ctx context.Context, hook, subject, body string) error {
	text := subject + "\n\n" + body
	content := text
	if len(content) > 1900 { // Discord refuses more than 2000 characters
		content = content[:1900] + "…"
	}
	b, _ := json.Marshal(map[string]string{"text": text, "content": content, "subject": subject, "body": body, "server": serverName()})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", hook, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "better-backup")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The address may carry a secret; leave it out of the message.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("the service answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	a := s.alertSettings()
	if a.Webhook == "" {
		fail(w, http.StatusBadRequest, "save a webhook address first")
		return
	}
	err := postWebhook(r.Context(), a.Webhook, "[better-backup] Test message from "+serverName(), "This is a test message from the better-backup server on "+serverName()+". Webhook alerts are working.")
	if err != nil {
		fail(w, http.StatusBadGateway, "the message was not delivered: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
