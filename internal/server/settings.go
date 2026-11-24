package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
)

// tlsNames returns the extra names set in the web UI.
func (s *Server) tlsNames() []string {
	var names []string
	if v := s.setting("tls_names"); v != "" {
		json.Unmarshal([]byte(v), &names)
	}
	return names
}

// issueCert makes the certificate the listener hands out, for the names
// from the command line and the ones set in the web UI.
func (s *Server) issueCert() error {
	cert, err := s.ca.serverCert(append(append([]string{}, s.cfg.TLSNames...), s.tlsNames()...))
	if err != nil {
		return err
	}
	s.cert.Store(&cert)
	return nil
}

func settingTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil
	}
	return &t
}

func (s *Server) serverSettings() api.ServerSettings {
	out := api.ServerSettings{DataDir: s.cfg.DataDir, RepoDir: s.cfg.RepoDir, Listen: s.cfg.Listen, FixedTLSNames: s.cfg.TLSNames,
		TLSNames: s.tlsNames(), Fingerprint: s.fp, KeySource: "file",
		KeySaved: settingTime(s.setting("key_saved")), StateSaved: settingTime(s.setting("state_saved"))}
	if s.cfg.Passphrase != "" {
		out.KeySource = "environment"
	}
	if out.TLSNames == nil {
		out.TLSNames = []string{}
	}
	out.Hostname, _ = os.Hostname()
	if abs, err := filepath.Abs(out.DataDir); err == nil {
		out.DataDir = abs
	}
	if abs, err := filepath.Abs(out.RepoDir); err == nil {
		out.RepoDir = abs
	}
	return out
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.serverSettings())
}

// handleSaveSettings stores the names agents and browsers reach the
// server by, and puts them in its certificate at once.
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req api.ServerSettings
	if !decode(w, r, &req) {
		return
	}
	names := []string{}
	for _, n := range req.TLSNames {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if net.ParseIP(n) == nil && (len(n) > 253 || strings.ContainsAny(n, " /:@\t") || strings.HasPrefix(n, ".")) {
			fail(w, http.StatusBadRequest, n+" is not a host name or an IP address. Leave out https:// and the port.")
			return
		}
		names = append(names, n)
	}
	if len(names) > 50 {
		fail(w, http.StatusBadRequest, "at most 50 names")
		return
	}
	b, _ := json.Marshal(names)
	if err := s.setSetting("tls_names", string(b)); err != nil {
		s.internal(w, err)
		return
	}
	if err := s.issueCert(); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "names in the certificate: %s", strings.Join(names, ", "))
	reply(w, http.StatusOK, s.serverSettings())
}

// confirmPassword makes a signed-in user type their password again
// before a step that hands out a secret.
func (s *Server) confirmPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	p := who(r)
	if p.token {
		return true
	}
	_, hash, _ := s.userWhere(`WHERE id = ?`, p.id)
	if !checkPassword(hash, password) {
		fail(w, http.StatusForbidden, "The password is wrong")
		return false
	}
	return true
}

// handleRepoKey hands the repository key to an administrator, so a copy
// can be kept away from the server.
func (s *Server) handleRepoKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) || !s.confirmPassword(w, r, req.Password) {
		return
	}
	pass, err := s.passphrase()
	if err != nil {
		s.internal(w, err)
		return
	}
	s.setSetting("key_saved", time.Now().UTC().Format(time.RFC3339))
	reply(w, http.StatusOK, map[string]string{"key": string(pass), "repo_id": s.repo.ID()})
}

// handleNewAdminToken replaces the admin token the command line uses.
func (s *Server) handleNewAdminToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) || !s.confirmPassword(w, r, req.Password) {
		return
	}
	tok := keys.RandomHex(24)
	af := AdminFile{URL: s.localURL(), Token: tok, Fingerprint: s.fp}
	b, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "admin.json"), b, 0o600); err != nil {
		s.internal(w, errors.New("writing admin.json: "+err.Error()))
		return
	}
	if err := s.setSetting("admin_token_hash", hashToken(tok)); err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"token": tok})
}
