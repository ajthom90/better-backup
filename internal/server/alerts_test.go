package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// fakeSMTP accepts mail on a local port and hands each message, with
// its envelope, to the returned channel.
func fakeSMTP(t *testing.T) (host string, port int, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				say := func(s string) { io.WriteString(conn, s+"\r\n") }
				say("220 fake ESMTP")
				var envelope strings.Builder
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"):
						say("250 fake")
					case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
						envelope.WriteString(strings.TrimSpace(line) + "\n")
						say("250 ok")
					case cmd == "DATA":
						say("354 go on")
						var data strings.Builder
						for {
							l, err := r.ReadString('\n')
							if err != nil {
								return
							}
							if l == ".\r\n" {
								break
							}
							data.WriteString(l)
						}
						got <- envelope.String() + data.String()
						say("250 queued")
					case cmd == "QUIT":
						say("221 bye")
						return
					default:
						say("500 what")
					}
				}
			}()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port, got
}

func receive(t *testing.T, got chan string) string {
	t.Helper()
	select {
	case m := <-got:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no mail arrived")
		return ""
	}
}

func TestAlerts(t *testing.T) {
	host, port, got := fakeSMTP(t)
	s, err := New(Config{DataDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stop := make(chan struct{})
	defer close(stop)
	go s.mailer(stop)

	job := &api.Job{ID: "j1", Type: api.JobBackup}
	s.alertJob(job, api.StatusFailed, "disk on fire", nil) // alerts are off: nothing is sent

	a := api.AlertSettings{Enabled: true, Host: host, Port: port, Security: "none", From: "Backups <backup@example.com>", To: []string{"admin@example.com", " ops@example.com "}}
	if err := checkAlertSettings(&a); err != nil {
		t.Fatal(err)
	}
	s.setSetting(alertSettingsKey, mustJSON(t, a))

	s.alertJob(job, api.StatusFailed, "disk on fire\r\nBcc: evil@example.com", nil)
	m := receive(t, got)
	for _, want := range []string{"MAIL FROM:<backup@example.com>", "RCPT TO:<admin@example.com>", "RCPT TO:<ops@example.com>",
		"Subject: [better-backup] Backup failed", "Error: disk on fire"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "\nBcc:") && strings.Index(m, "\nBcc:") < strings.Index(m, "\r\n\r\n") {
		t.Errorf("job text reached the headers:\n%s", m)
	}

	// Warnings are only mailed when asked for.
	s.alertJob(job, api.StatusWarning, "", []string{"skipped a.txt"})
	s.alertJob(job, api.StatusSuccess, "", nil)
	a.OnWarning = true
	s.setSetting(alertSettingsKey, mustJSON(t, a))
	s.alertJob(job, api.StatusWarning, "", []string{"skipped b.txt"})
	if m := receive(t, got); !strings.Contains(m, "skipped b.txt") || strings.Contains(m, "skipped a.txt") {
		t.Errorf("unexpected warning mail:\n%s", m)
	}

	// A policy that is long overdue is reported once.
	now := time.Now()
	p := api.Policy{ID: "p1", Name: "Nightly", AgentID: "gone", Paths: []string{"/x"}, Enabled: true,
		Schedule: api.Schedule{Interval: "1h"}, Created: now.Add(-48 * time.Hour)}
	if _, err := s.db.Exec(`INSERT INTO policies (id, agent_id, created, spec) VALUES (?, ?, ?, ?)`, p.ID, p.AgentID, ms(p.Created), mustJSON(t, p)); err != nil {
		t.Fatal(err)
	}
	s.checkMissed(now)
	s.checkMissed(now)
	if m := receive(t, got); !strings.Contains(m, "Backup missed: Nightly") {
		t.Errorf("unexpected missed mail:\n%s", m)
	}
	select {
	case m := <-got:
		t.Fatalf("a second missed alert was sent:\n%s", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOverdue(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.Local)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	hourly := &api.Policy{Enabled: true, Schedule: api.Schedule{Interval: "1h"}, Created: ago(100 * time.Hour)}
	daily := &api.Policy{Enabled: true, Schedule: api.Schedule{DailyAt: "02:00"}, Created: ago(100 * time.Hour)}
	cases := []struct {
		name string
		p    *api.Policy
		last time.Time
		want bool
	}{
		{"hourly on time", hourly, ago(90 * time.Minute), false},
		{"hourly late", hourly, ago(121 * time.Minute), true},
		{"hourly never ran", hourly, time.Time{}, true},
		{"daily on time", daily, ago(30 * time.Hour), false},
		{"daily late", daily, ago(37 * time.Hour), true},
		{"new daily policy", &api.Policy{Enabled: true, Schedule: api.Schedule{DailyAt: "02:00"}, Created: ago(20 * time.Hour)}, time.Time{}, false},
		{"paused", &api.Policy{Schedule: api.Schedule{Interval: "1h"}, Created: ago(100 * time.Hour)}, time.Time{}, false},
		{"manual", &api.Policy{Enabled: true, Created: ago(100 * time.Hour)}, time.Time{}, false},
	}
	for _, c := range cases {
		if got := overdue(c.p, c.last, now); got != c.want {
			t.Errorf("%s: overdue = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCheckAlertSettings(t *testing.T) {
	a := api.AlertSettings{}
	if err := checkAlertSettings(&a); err != nil {
		t.Fatalf("switched-off alerts need no settings: %v", err)
	}
	a = api.AlertSettings{Enabled: true, Host: "mail.example.com", From: "b@example.com", To: []string{"a@example.com"}}
	if err := checkAlertSettings(&a); err != nil || a.Port != 587 || a.Security != "starttls" {
		t.Fatalf("defaults: %+v, %v", a, err)
	}
	for _, bad := range []api.AlertSettings{
		{Enabled: true, From: "b@example.com", To: []string{"a@example.com"}},
		{Enabled: true, Host: "h", To: []string{"a@example.com"}},
		{Enabled: true, Host: "h", From: "b@example.com"},
		{Enabled: true, Host: "h", From: "b@example.com", To: []string{"not an address"}},
		{Enabled: true, Host: "h", From: "b@example.com", To: []string{"a@example.com"}, Security: "ssl"},
	} {
		if err := checkAlertSettings(&bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
