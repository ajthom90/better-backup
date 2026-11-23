package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ajthom90/better-backup/internal/api"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS enroll_tokens (
	hash TEXT PRIMARY KEY, expires INTEGER NOT NULL, uses INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY, name TEXT NOT NULL, hostname TEXT NOT NULL, os TEXT NOT NULL,
	arch TEXT NOT NULL, version TEXT NOT NULL, cert_fp TEXT NOT NULL,
	enrolled INTEGER NOT NULL, last_seen INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS policies (
	id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, created INTEGER NOT NULL, spec TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY, type TEXT NOT NULL, status TEXT NOT NULL,
	agent_id TEXT NOT NULL DEFAULT '', policy_id TEXT NOT NULL DEFAULT '',
	snapshot_id TEXT NOT NULL DEFAULT '', spec TEXT NOT NULL DEFAULT '',
	progress TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '',
	warnings TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL, started INTEGER NOT NULL DEFAULT 0,
	finished INTEGER NOT NULL DEFAULT 0, heartbeat INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS jobs_agent ON jobs(agent_id, status);
CREATE INDEX IF NOT EXISTS jobs_created ON jobs(created);
`

func openDB(dir string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "server.db")) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has a single writer anyway, and this keeps
	// every statement consistent without retry logic.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

func (s *Server) setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Server) setSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

const jobCols = `id, type, status, agent_id, policy_id, snapshot_id, spec, progress, result, warnings, error, created, started, finished`

type scanner interface{ Scan(...any) error }

func scanJob(row scanner) (*api.Job, error) {
	var j api.Job
	var spec, progress, result, warnings string
	var created, started, finished int64
	if err := row.Scan(&j.ID, &j.Type, &j.Status, &j.AgentID, &j.PolicyID, &j.SnapshotID, &spec, &progress, &result, &warnings, &j.Error, &created, &started, &finished); err != nil {
		return nil, err
	}
	if spec != "" {
		switch j.Type {
		case api.JobBackup:
			j.Backup = new(api.BackupSpec)
			json.Unmarshal([]byte(spec), j.Backup)
		case api.JobRestore:
			j.Restore = new(api.RestoreSpec)
			json.Unmarshal([]byte(spec), j.Restore)
		}
	}
	if progress != "" {
		j.Progress = json.RawMessage(progress)
	}
	if result != "" {
		j.Result = json.RawMessage(result)
	}
	if warnings != "" {
		json.Unmarshal([]byte(warnings), &j.Warnings)
	}
	j.Created = fromMS(created)
	if started != 0 {
		t := fromMS(started)
		j.Started = &t
	}
	if finished != 0 {
		t := fromMS(finished)
		j.Finished = &t
	}
	return &j, nil
}

func (s *Server) job(id string) (*api.Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

func (s *Server) jobs(where string, args ...any) ([]*api.Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*api.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Server) policies(where string, args ...any) ([]*api.Policy, error) {
	rows, err := s.db.Query(`SELECT spec FROM policies `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*api.Policy{}
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		p := new(api.Policy)
		if err := json.Unmarshal([]byte(spec), p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) policy(id string) (*api.Policy, error) {
	ps, err := s.policies(`WHERE id = ?`, id)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return ps[0], nil
}

const agentCols = `id, name, hostname, os, arch, version, enrolled, last_seen, revoked`

func scanAgent(row scanner) (*api.Agent, error) {
	var a api.Agent
	var enrolled, seen int64
	if err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &enrolled, &seen, &a.Revoked); err != nil {
		return nil, err
	}
	a.EnrolledAt, a.LastSeen = fromMS(enrolled), fromMS(seen)
	a.Online = !a.Revoked && time.Since(a.LastSeen) < 90*time.Second
	return &a, nil
}

func (s *Server) agent(id string) (*api.Agent, error) {
	a, err := scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}
