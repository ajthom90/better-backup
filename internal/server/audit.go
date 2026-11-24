package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// auditKeep is how long the activity log is kept.
const auditKeep = 2 * 365 * 24 * time.Hour

// auditNote lets a handler say what exactly it changed.
type auditNote struct{ detail string }

// note records the detail of the change a request makes, for the
// activity log.
func note(r *http.Request, format string, args ...any) {
	if a, _ := r.Context().Value(auditKey).(*auditNote); a != nil {
		a.detail = fmt.Sprintf(format, args...)
	}
}

// audit adds a line to the activity log.
func (s *Server) audit(user, remote, action, detail string) {
	if len(detail) > 500 {
		detail = detail[:500]
	}
	if _, err := s.db.Exec(`INSERT INTO audit (time, user, remote, action, detail) VALUES (?, ?, ?, ?, ?)`, ms(time.Now()), user, remote, action, detail); err != nil {
		s.log.Error("writing the activity log", "err", err)
	}
	// What is in the log changed something: save the server's state soon.
	s.stateDirty.Store(true)
	s.log.Info("activity", "user", user, "from", remote, "action", action, "detail", detail)
}

// subject names what a request is about before the handler runs, while
// the thing still exists.
func (s *Server) subject(r *http.Request) string {
	id := r.PathValue("id")
	if id == "" {
		return ""
	}
	path := r.URL.Path
	switch {
	case strings.Contains(path, "/policies/"):
		if p, _ := s.policy(id); p != nil {
			return p.Name
		}
	case strings.Contains(path, "/agents/"):
		if a, _ := s.agent(id); a != nil {
			return a.Name
		}
	case strings.Contains(path, "/snapshots/"):
		s.mu.Lock()
		sn := s.snaps[id]
		s.mu.Unlock()
		if sn != nil {
			return fmt.Sprintf("%s of %s, %s", id, sn.Hostname, sn.Time.Local().Format("2006-01-02 15:04"))
		}
	case strings.Contains(path, "/vmware-servers/"):
		for _, v := range s.vmwareServers() {
			if v.ID == id {
				return v.Name
			}
		}
	}
	return id
}

// statusWriter remembers the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	q, args := `SELECT id, time, user, remote, action, detail FROM audit`, []any{}
	if before > 0 {
		q += ` WHERE id < ?`
		args = append(args, before)
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	out := []api.AuditEntry{}
	for rows.Next() {
		var e api.AuditEntry
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.User, &e.Remote, &e.Action, &e.Detail); err != nil {
			s.internal(w, err)
			return
		}
		e.Time = fromMS(t)
		out = append(out, e)
	}
	reply(w, http.StatusOK, out)
}
