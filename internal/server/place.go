package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/logring"
	"github.com/ajthom90/better-backup/internal/repo"
)

// place is where the repository is kept and what the server listens
// on, when that was chosen in the web UI. It is a file of its own,
// place.json in the data folder, because it is needed before the
// repository and the database are opened. It wins over the command
// line, the environment and what the installer was told.
type place struct {
	RepoDir string `json:"repo_dir,omitempty"`
	Listen  string `json:"listen,omitempty"`
}

func placeFile(dataDir string) string { return filepath.Join(dataDir, "place.json") }

func readPlace(dataDir string) *place {
	b, err := os.ReadFile(placeFile(dataDir))
	if err != nil {
		return nil
	}
	var p place
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// inContainer says whether the server runs in a container (Docker, a
// TrueNAS app, Podman). There the folder and the port are set where the
// container is defined: a path inside would be lost with the container,
// and another port inside is not the one that is published.
var inContainer = func() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

func listenPort(listen string) int {
	_, port, _ := net.SplitHostPort(listen)
	n, _ := strconv.Atoi(port)
	return n
}

// handleSavePlace moves the server to another repository folder or
// port, and starts it again there.
func (s *Server) handleSavePlace(w http.ResponseWriter, r *http.Request) {
	var req api.PlaceRequest
	if !decode(w, r, &req) {
		return
	}
	if inContainer() {
		fail(w, http.StatusConflict, "This server runs in a container. Its folder and port are set where the container is defined: the volume mounted at /data/repo and the published port.")
		return
	}
	if !s.confirmPassword(w, r, req.Password) {
		return
	}
	cur := s.serverSettings()
	old := place{}
	if pl := readPlace(s.cfg.DataDir); pl != nil {
		old = *pl
	}
	next := old

	if req.Port != 0 && req.Port != cur.Port {
		if req.Port < 1 || req.Port > 65535 {
			fail(w, http.StatusBadRequest, "A port is a number from 1 to 65535")
			return
		}
		host, _, _ := net.SplitHostPort(s.cfg.Listen)
		next.Listen = net.JoinHostPort(host, strconv.Itoa(req.Port))
		ln, err := net.Listen("tcp", next.Listen)
		if err != nil {
			fail(w, http.StatusConflict, fmt.Sprintf("Port %d cannot be used: %v", req.Port, err))
			return
		}
		ln.Close()
	}

	var copyFrom string
	if req.RepoDir != "" && filepath.Clean(req.RepoDir) != cur.RepoDir {
		dir := filepath.Clean(req.RepoDir)
		if !filepath.IsAbs(dir) {
			fail(w, http.StatusBadRequest, "Give the whole path of the folder, such as "+filepath.Join(filepath.VolumeName(cur.RepoDir)+string(filepath.Separator), "Backups"))
			return
		}
		s.mu.Lock()
		busy := len(s.running) > 0 || s.maint || s.offsiteBusy || s.gcRunning
		have := make([]string, 0, len(s.snaps)+len(s.trashed))
		for id := range s.snaps {
			have = append(have, id)
		}
		for id := range s.trashed {
			have = append(have, id)
		}
		s.mu.Unlock()
		if busy {
			fail(w, http.StatusConflict, "A job is running. Wait until it is done, then change the folder.")
			return
		}
		if repo.Exists(dir) {
			// A copy of this repository, made by the administrator.
			pass, err := s.passphrase()
			if err != nil {
				s.internal(w, err)
				return
			}
			other, err := repo.OpenReadOnly(dir, pass)
			if err != nil {
				fail(w, http.StatusBadRequest, "That folder holds a repository this server's key does not open: "+err.Error())
				return
			}
			id := other.ID()
			snaps, err := other.LoadSnapshots()
			other.Close()
			if id != s.repo.ID() {
				fail(w, http.StatusBadRequest, "That folder holds another server's backups, not a copy of this one's.")
				return
			}
			if err != nil {
				fail(w, http.StatusBadRequest, "The copy in that folder cannot be read: "+err.Error())
				return
			}
			there := map[string]bool{}
			for _, sn := range snaps {
				there[sn.ID] = true
			}
			missing := 0
			for _, id := range s.snapshotIDs() {
				if !there[id] {
					missing++
				}
			}
			if missing > 0 {
				fail(w, http.StatusConflict, fmt.Sprintf("The copy in that folder is older than what the server has now: %d snapshots are missing from it. Copy the folder again while no backup runs.", missing))
				return
			}
		} else {
			entries, err := os.ReadDir(dir)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				fail(w, http.StatusBadRequest, "That folder cannot be read: "+err.Error())
				return
			}
			if len(entries) > 0 {
				fail(w, http.StatusBadRequest, "That folder holds other files. Choose an empty folder, or one that does not exist yet.")
				return
			}
			if len(have) > 0 {
				fail(w, http.StatusConflict, "The server already holds backups, and moving them is a copy of the whole folder, which is yours to make: while no backup runs, copy "+cur.RepoDir+" to the new place with all it holds, then give the new folder here. The server checks the copy is complete before it switches, and leaves the old folder alone.")
				return
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				fail(w, http.StatusBadRequest, "That folder cannot be made: "+err.Error())
				return
			}
			probe := filepath.Join(dir, ".bb-write-test")
			if err := os.WriteFile(probe, nil, 0o600); err != nil {
				fail(w, http.StatusBadRequest, "The server cannot write to that folder: "+err.Error())
				return
			}
			os.Remove(probe)
			copyFrom = cur.RepoDir
		}
		next.RepoDir = dir
	}

	if next == old {
		reply(w, http.StatusOK, map[string]any{"restarting": false})
		return
	}
	note(r, "repository %s, listening on %s", orStr(next.RepoDir, cur.RepoDir), orStr(next.Listen, cur.Listen))
	dataDir, log := s.cfg.DataDir, s.log
	s.requestRestart(func() error {
		if copyFrom != "" {
			// An empty repository: a few small files.
			if err := copyTree(copyFrom, next.RepoDir); err != nil {
				return fmt.Errorf("copying the empty repository to %s: %w", next.RepoDir, err)
			}
		}
		b, _ := json.MarshalIndent(next, "", "  ")
		if err := os.WriteFile(placeFile(dataDir), b, 0o600); err != nil {
			return err
		}
		if next.Listen != "" {
			if err := openFirewall(listenPort(next.Listen)); err != nil {
				log.Warn("could not open the new port in the firewall; open it by hand", "err", err)
			}
		}
		return nil
	})
	reply(w, http.StatusOK, map[string]any{"restarting": true, "port": listenPort(orStr(next.Listen, cur.Listen))})
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// snapshotIDs lists the snapshots the server has, deleted ones that can
// still be brought back included.
func (s *Server) snapshotIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.snaps))
	for id := range s.snaps {
		ids = append(ids, id)
	}
	return ids
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// handleServerLog hands an administrator the server's newest log lines.
func (s *Server) handleServerLog(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]string{"text": logring.Text()})
}
