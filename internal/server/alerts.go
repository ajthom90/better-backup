package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
)

const alertSettingsKey = "alerts"

type mailMsg struct{ subject, body string }

// alertSettings returns the stored settings, including the password.
func (s *Server) alertSettings() api.AlertSettings {
	var a api.AlertSettings
	if v := s.setting(alertSettingsKey); v != "" {
		json.Unmarshal([]byte(v), &a)
	}
	return a
}

// checkAlertSettings fills in defaults and rejects settings that cannot
// work.
func checkAlertSettings(a *api.AlertSettings) error {
	a.Host = strings.TrimSpace(a.Host)
	a.From = strings.TrimSpace(a.From)
	if a.Security == "" {
		a.Security = "starttls"
	}
	if a.Security != "starttls" && a.Security != "tls" && a.Security != "none" {
		return errors.New(`security must be "starttls", "tls" or "none"`)
	}
	if a.Port == 0 {
		a.Port = map[string]int{"starttls": 587, "tls": 465, "none": 25}[a.Security]
	}
	if a.Port < 1 || a.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	var to []string
	for _, t := range a.To {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if _, err := mail.ParseAddress(t); err != nil {
			return fmt.Errorf("%q is not an email address", t)
		}
		to = append(to, t)
	}
	a.To = to
	if a.From != "" {
		if _, err := mail.ParseAddress(a.From); err != nil {
			return fmt.Errorf("%q is not an email address", a.From)
		}
	}
	if !a.Enabled {
		return nil
	}
	switch {
	case a.Host == "":
		return errors.New("alerts need the mail server's host name")
	case a.From == "":
		return errors.New("alerts need a from address")
	case len(a.To) == 0:
		return errors.New("alerts need at least one address to send to")
	}
	return nil
}

func (s *Server) handleAlertSettings(w http.ResponseWriter, r *http.Request) {
	a := s.alertSettings()
	a.PasswordSet, a.Password = a.Password != "", ""
	reply(w, http.StatusOK, a)
}

func (s *Server) handleSaveAlertSettings(w http.ResponseWriter, r *http.Request) {
	var a api.AlertSettings
	if !decode(w, r, &a) {
		return
	}
	if a.Password == "" && a.PasswordSet {
		a.Password = s.alertSettings().Password
	}
	a.PasswordSet = false
	if err := checkAlertSettings(&a); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(a)
	if err := s.setSetting(alertSettingsKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	a.PasswordSet, a.Password = a.Password != "", ""
	reply(w, http.StatusOK, a)
}

// handleTestAlert sends a test message now and reports what the mail
// server said.
func (s *Server) handleTestAlert(w http.ResponseWriter, r *http.Request) {
	a := s.alertSettings()
	if a.Host == "" || a.From == "" || len(a.To) == 0 {
		fail(w, http.StatusBadRequest, "save the mail server, from address and recipients first")
		return
	}
	err := sendMail(a, "[better-backup] Test message from "+serverName(),
		"This is a test message from the better-backup server on "+serverName()+".\n\nEmail alerts are working.\n")
	if err != nil {
		fail(w, http.StatusBadGateway, "the message was not sent: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serverName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "the backup server"
	}
	return h
}

// alert queues a message if alerts are on. It never blocks.
func (s *Server) alert(subject, body string) {
	select {
	case s.mail <- mailMsg{subject, body}:
	default:
		s.log.Error("too many alerts waiting; dropped one", "subject", subject)
	}
}

// mailer sends queued alerts one at a time until stop is closed.
func (s *Server) mailer(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case m := <-s.mail:
			a := s.alertSettings()
			if !a.Enabled {
				continue
			}
			if err := sendMail(a, m.subject, m.body); err != nil {
				s.log.Error("sending alert email", "subject", m.subject, "err", err)
			} else {
				s.log.Info("sent alert email", "subject", m.subject)
			}
		}
	}
}

// alertJob sends mail about a job that failed or, if asked for, finished
// with warnings.
func (s *Server) alertJob(j *api.Job, status, errMsg string, warnings []string) {
	if status != api.StatusFailed && status != api.StatusWarning {
		return
	}
	a := s.alertSettings()
	if !a.Enabled || (status == api.StatusWarning && !a.OnWarning) {
		return
	}
	what := map[string]string{api.JobBackup: "Backup", api.JobRestore: "Restore", api.JobGC: "Garbage collection", api.JobVerify: "Repository verification", api.JobOffsite: "Offsite copy"}[j.Type]
	if what == "" {
		what = "Job"
	}
	outcome := "failed"
	if status == api.StatusWarning {
		outcome = "finished with warnings"
	}
	subject := what + " " + outcome
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s on the better-backup server %s.\n\n", what, outcome, serverName())
	if p, _ := s.policy(j.PolicyID); p != nil {
		subject += ": " + p.Name
		fmt.Fprintf(&b, "Policy:  %s\n", p.Name)
	}
	if ag, _ := s.agent(j.AgentID); ag != nil {
		subject += " on " + ag.Name
		fmt.Fprintf(&b, "Machine: %s\n", ag.Name)
	}
	fmt.Fprintf(&b, "Job:     %s\nTime:    %s\n", j.ID, time.Now().Format("2006-01-02 15:04 MST"))
	if errMsg != "" {
		fmt.Fprintf(&b, "\nError: %s\n", errMsg)
	}
	if len(warnings) > 0 {
		const show = 20
		fmt.Fprintf(&b, "\nWarnings (%d):\n", len(warnings))
		for i, w := range warnings {
			if i == show {
				fmt.Fprintf(&b, "  ... and %d more. See them all with: bb job %s\n", len(warnings)-show, j.ID)
				break
			}
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}
	s.alert("[better-backup] "+subject, b.String())
}

// overdue reports whether a scheduled policy has gone too long without a
// good backup: one and a half schedule periods, and at least an hour
// past the time a backup was due. lastGood is when its most recent
// successful backup finished.
func overdue(p *api.Policy, lastGood, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	var period time.Duration
	switch {
	case p.Schedule.Interval != "":
		d, err := time.ParseDuration(p.Schedule.Interval)
		if err != nil {
			return false
		}
		period = d
	case p.Schedule.DailyAt != "":
		period = 24 * time.Hour
	default:
		return false
	}
	if lastGood.IsZero() {
		lastGood = p.Created
	}
	return now.Sub(lastGood) > period+max(period/2, time.Hour)
}

// checkMissed sends one alert when a policy becomes overdue, and forgets
// about it once the policy has a good backup again.
func (s *Server) checkMissed(now time.Time) {
	if !s.alertSettings().Enabled {
		return
	}
	ps, err := s.policies("")
	if err != nil {
		return
	}
	for _, p := range ps {
		key := "alert_missed:" + p.ID
		var last int64
		var running int
		s.db.QueryRow(`SELECT COALESCE(MAX(finished), 0) FROM jobs WHERE policy_id = ? AND type = ? AND status IN (?, ?)`,
			p.ID, api.JobBackup, api.StatusSuccess, api.StatusWarning).Scan(&last)
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE policy_id = ? AND type = ? AND status = ?`, p.ID, api.JobBackup, api.StatusRunning).Scan(&running)
		// A backup that is still running, such as a large first backup,
		// is late but not missed.
		if !overdue(p, fromMS(last), now) {
			if s.setting(key) != "" {
				s.setSetting(key, "")
			}
			continue
		}
		if running > 0 || s.setting(key) != "" {
			continue
		}
		s.setSetting(key, "1")
		name, state := p.AgentID, ""
		if ag, _ := s.agent(p.AgentID); ag != nil {
			name = ag.Name
			if !ag.Online {
				state = fmt.Sprintf("\nThe agent on %s is offline. It was last seen %s.\n", ag.Name, ag.LastSeen.Local().Format("2006-01-02 15:04 MST"))
			}
		}
		lastText := "never"
		if last != 0 {
			lastText = fromMS(last).Local().Format("2006-01-02 15:04 MST")
		}
		s.alert("[better-backup] Backup missed: "+p.Name+" on "+name,
			fmt.Sprintf("The policy %q on %s has not had a successful backup when one was due.\n\nLast successful backup: %s\n%s\nThis alert is sent once. Another is sent if backups resume and are missed again.\n",
				p.Name, name, lastText, state))
	}
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

// sendMail delivers one plain-text message.
func sendMail(a api.AlertSettings, subject, body string) error {
	from, err := mail.ParseAddress(a.From)
	if err != nil {
		return fmt.Errorf("from address: %w", err)
	}
	addr := net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsCfg := &tls.Config{ServerName: a.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if a.Security == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Minute))
	c, err := smtp.NewClient(conn, a.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if a.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS; choose a different security setting")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if a.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", a.Username, a.Password, a.Host)); err != nil {
			return fmt.Errorf("signing in to the mail server: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	var tos []string
	for _, t := range a.To {
		ad, err := mail.ParseAddress(t)
		if err != nil {
			return fmt.Errorf("recipient %q: %w", t, err)
		}
		if err := c.Rcpt(ad.Address); err != nil {
			return err
		}
		tos = append(tos, ad.String())
	}
	var msg bytes.Buffer
	domain := "localhost"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\n", from.String(), strings.Join(tos, ", "),
		mime.QEncoding.Encode("utf-8", oneLine(subject)), time.Now().Format(time.RFC1123Z), keys.RandomHex(16), domain)
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\nAuto-Submitted: auto-generated\r\n\r\n")
	qp := quotedprintable.NewWriter(&msg)
	qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	qp.Close()
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
