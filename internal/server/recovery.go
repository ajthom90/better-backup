package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/disklayout"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
)

// Bare-metal recovery. A machine started from the recovery disc has no
// identity: it announces itself, gets a session and shows the session's
// code on its screen. Nothing is handed to it until the administrator
// approves the machine showing that code. Approval mints a one-time
// enrollment token, the machine enrolls with it like any agent, and from
// then on a restore reaches it through the ordinary job machinery.

const (
	// recoveryIdle is how long a machine may stay silent before the
	// server forgets it. A waiting machine polls every few seconds.
	recoveryIdle = 2 * time.Minute
	// maxRecoverySessions bounds what unauthenticated callers can make
	// the server remember.
	maxRecoverySessions = 64

	// RecoveryPlaceholder marks the place in the recovery disc image
	// where the server writes its address when the image is downloaded.
	// It sits on the kernel command line in the image's boot menu, and
	// is RecoveryConfigSize bytes long with its padding.
	RecoveryPlaceholder = "@@BB-RECOVERY-CONFIG@@"
	RecoveryConfigSize  = 384
)

type recoverySession struct {
	id, code   string
	secretHash string
	remote     string
	created    time.Time
	seen       time.Time
	approved   bool
	token      string // enrollment token waiting to be collected
	tokenHash  string
	agentID    string
	version    string
	status     string
	hw         *api.RecoveryHardware
	command    string
	wake       chan struct{}
}

type isoLink struct {
	config  api.RecoveryConfig
	expires time.Time
}

// recoveryCode makes a code that is easy to read off a screen and to
// compare: no letters or digits that look alike.
func recoveryCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:3]) + "-" + string(b[3:])
}

// expireRecovery forgets machines that went away. Called with s.mu held.
func (s *Server) expireRecoveryLocked() (gone []string) {
	for id, rs := range s.recovery {
		if time.Since(rs.seen) > recoveryIdle {
			delete(s.recovery, id)
			if rs.agentID != "" {
				gone = append(gone, rs.agentID)
			}
		}
	}
	for t, l := range s.isoLinks {
		if time.Now().After(l.expires) {
			delete(s.isoLinks, t)
		}
	}
	return gone
}

// retireRecoveryAgents blocks the identities of machines that left the
// recovery disc and fails what was still queued for them.
func (s *Server) retireRecoveryAgents(ids []string) {
	for _, id := range ids {
		s.db.Exec(`UPDATE agents SET revoked = 1 WHERE id = ? AND recovery = 1`, id)
		if js, err := s.jobs(`WHERE agent_id = ? AND status = ?`, id, api.StatusQueued); err == nil {
			for _, j := range js {
				s.finish(j, api.StatusFailed, "the machine left the recovery disc before this restore started", nil, nil)
			}
		}
	}
}

func (s *Server) expireRecovery() {
	s.mu.Lock()
	gone := s.expireRecoveryLocked()
	s.mu.Unlock()
	s.retireRecoveryAgents(gone)
}

func (s *Server) handleRecoveryHello(w http.ResponseWriter, r *http.Request) {
	s.expireRecovery()
	out := api.RecoverySession{ID: keys.RandomHex(8), Secret: keys.RandomHex(24)}
	rs := &recoverySession{id: out.ID, secretHash: hashToken(out.Secret), remote: r.RemoteAddr, created: time.Now(), seen: time.Now(), wake: make(chan struct{}, 1)}
	s.mu.Lock()
	if len(s.recovery) >= maxRecoverySessions {
		s.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "too many machines are waiting for recovery; forget some in the web interface")
		return
	}
	// Codes are compared by eye, so two machines must never share one.
	for {
		rs.code = recoveryCode()
		clash := false
		for _, o := range s.recovery {
			clash = clash || o.code == rs.code
		}
		if !clash {
			break
		}
	}
	out.Code = rs.code
	s.recovery[rs.id] = rs
	s.mu.Unlock()
	s.log.Info("a machine started from the recovery disc is waiting for approval", "code", rs.code, "remote", r.RemoteAddr)
	reply(w, http.StatusCreated, out)
}

func (s *Server) handleRecoveryPoll(w http.ResponseWriter, r *http.Request) {
	var req api.RecoveryPoll
	if !decode(w, r, &req) {
		return
	}
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	rs := s.recovery[r.PathValue("id")]
	if rs == nil || subtle.ConstantTimeCompare([]byte(hashToken(secret)), []byte(rs.secretHash)) != 1 {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "the server does not know this recovery session; start over")
		return
	}
	rs.seen, rs.remote = time.Now(), r.RemoteAddr
	if req.Version != "" {
		rs.version = req.Version
	}
	rs.status = req.Status
	if req.Hardware != nil {
		rs.hw = req.Hardware
	}
	s.mu.Unlock()

	wait := 20 * time.Second
	if v, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && v >= 0 && v <= 55 {
		wait = time.Duration(v) * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		out := api.RecoveryReply{Approved: rs.approved, EnrollToken: rs.token, Command: rs.command}
		rs.token, rs.command = "", ""
		rs.seen = time.Now()
		s.mu.Unlock()
		if out.EnrollToken != "" || out.Command != "" {
			reply(w, http.StatusOK, out)
			return
		}
		select {
		case <-rs.wake:
		case <-timer.C:
			reply(w, http.StatusOK, out)
			return
		case <-s.stop:
			reply(w, http.StatusOK, out)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// recoveryEnrolled ties a new agent to the recovery session whose token
// it enrolled with.
func (s *Server) recoveryEnrolled(tokenHash, agentID string) {
	s.mu.Lock()
	var found *recoverySession
	for _, rs := range s.recovery {
		if rs.tokenHash != "" && rs.tokenHash == tokenHash {
			rs.agentID, rs.tokenHash, rs.token = agentID, "", ""
			found = rs
		}
	}
	s.mu.Unlock()
	if found != nil {
		s.db.Exec(`UPDATE agents SET recovery = 1, name = ? WHERE id = ?`, "Recovery disc "+found.code, agentID)
	}
}

func (s *Server) recoverySession(w http.ResponseWriter, r *http.Request) *recoverySession {
	s.mu.Lock()
	rs := s.recovery[r.PathValue("id")]
	s.mu.Unlock()
	if rs == nil {
		fail(w, http.StatusNotFound, "that machine is no longer waiting; it may have been restarted")
	}
	return rs
}

func nudge(rs *recoverySession) {
	select {
	case rs.wake <- struct{}{}:
	default:
	}
}

func (s *Server) handleRecoveryApprove(w http.ResponseWriter, r *http.Request) {
	rs := s.recoverySession(w, r)
	if rs == nil {
		return
	}
	s.mu.Lock()
	already := rs.approved
	s.mu.Unlock()
	if already {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := keys.RandomHex(16)
	if _, err := s.db.Exec(`INSERT INTO enroll_tokens (hash, expires, uses) VALUES (?, ?, 1)`, hashToken(token), ms(time.Now().Add(15*time.Minute))); err != nil {
		s.internal(w, err)
		return
	}
	s.mu.Lock()
	rs.approved, rs.token, rs.tokenHash = true, token, hashToken(token)
	s.mu.Unlock()
	nudge(rs)
	s.log.Info("recovery machine approved", "code", rs.code, "remote", rs.remote)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRecoveryCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if !decode(w, r, &req) {
		return
	}
	switch req.Command {
	case "reboot", "poweroff", "rescan":
	default:
		fail(w, http.StatusBadRequest, "command must be reboot, poweroff or rescan")
		return
	}
	rs := s.recoverySession(w, r)
	if rs == nil {
		return
	}
	s.mu.Lock()
	ok := rs.approved
	busy := rs.agentID != "" && s.running[rs.agentID] != ""
	if ok && !(busy && req.Command != "rescan") {
		rs.command = req.Command
	}
	s.mu.Unlock()
	switch {
	case !ok:
		fail(w, http.StatusConflict, "approve the machine first")
	case busy && req.Command != "rescan":
		fail(w, http.StatusConflict, "a restore is running on this machine; wait for it to finish")
	default:
		nudge(rs)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleRecoveryForget(w http.ResponseWriter, r *http.Request) {
	rs := s.recoverySession(w, r)
	if rs == nil {
		return
	}
	s.mu.Lock()
	delete(s.recovery, rs.id)
	s.mu.Unlock()
	if rs.agentID != "" {
		s.retireRecoveryAgents([]string{rs.agentID})
	}
	nudge(rs)
	w.WriteHeader(http.StatusNoContent)
}

// isoPath finds the recovery disc image.
func (s *Server) isoPath() string {
	candidates := []string{s.cfg.RecoveryISO, filepath.Join(s.cfg.DataDir, "recovery.iso"),
		"/usr/share/better-backup/recovery.iso"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "recovery.iso"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return c
		}
	}
	return ""
}

func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	s.expireRecovery()
	var out api.RecoveryStatus
	if p := s.isoPath(); p != "" {
		if fi, err := os.Stat(p); err == nil {
			out.ISO.Available, out.ISO.Size, out.ISO.Modified = true, fi.Size(), fi.ModTime()
		}
	}
	out.Machines = []api.RecoveryMachine{}
	s.mu.Lock()
	for _, rs := range s.recovery {
		out.Machines = append(out.Machines, api.RecoveryMachine{ID: rs.id, Code: rs.code, Remote: rs.remote, Created: rs.created, LastSeen: rs.seen,
			Approved: rs.approved, AgentID: rs.agentID, Version: rs.version, Status: rs.status, Hardware: rs.hw})
	}
	s.mu.Unlock()
	sort.Slice(out.Machines, func(i, j int) bool { return out.Machines[i].Created.Before(out.Machines[j].Created) })
	for i := range out.Machines {
		m := &out.Machines[i]
		if m.AgentID == "" {
			continue
		}
		if js, err := s.jobs(`WHERE agent_id = ? ORDER BY created DESC LIMIT 1`, m.AgentID); err == nil && len(js) > 0 {
			if js[0].Restore != nil {
				js[0].Restore.Sources = nil
			}
			m.Job = js[0]
		}
	}
	reply(w, http.StatusOK, out)
}

// handleRecoveryLink makes a short-lived link to the recovery disc image
// with this server's address written into it. A link, because a browser
// cannot send the admin token along with an ordinary download.
func (s *Server) handleRecoveryLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Server string `json:"server"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Server = strings.TrimRight(strings.TrimSpace(req.Server), "/")
	u, err := url.Parse(req.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || strings.ContainsAny(req.Server, " \"'`$\\\n") {
		fail(w, http.StatusBadRequest, "the server address must look like https://backup.example.com:8443: it is how the machine being recovered reaches this server")
		return
	}
	if s.isoPath() == "" {
		fail(w, http.StatusNotFound, "this server has no recovery disc image; upload one first")
		return
	}
	cfg := api.RecoveryConfig{Server: req.Server, Fingerprint: s.fp}
	if len(recoveryCmdline(cfg)) > RecoveryConfigSize {
		fail(w, http.StatusBadRequest, "the server address is too long")
		return
	}
	ticket := keys.RandomHex(16)
	s.mu.Lock()
	s.expireRecoveryLocked()
	s.isoLinks[ticket] = isoLink{config: cfg, expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	reply(w, http.StatusCreated, map[string]string{"url": "/dl/recovery/" + ticket})
}

// recoveryCmdline is what the recovery disc finds on its kernel command
// line once the image is personalized.
func recoveryCmdline(c api.RecoveryConfig) string {
	return "bb.server=" + c.Server + " bb.fingerprint=" + c.Fingerprint
}

func (s *Server) handleRecoveryDownload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	link, ok := s.isoLinks[r.PathValue("ticket")]
	s.mu.Unlock()
	if !ok || time.Now().After(link.expires) {
		http.Error(w, "This download link has expired. Go back to the better-backup page and ask for the recovery disc again.", http.StatusNotFound)
		return
	}
	path := s.isoPath()
	if path == "" {
		http.Error(w, "The server has no recovery disc image.", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-iso9660-image")
	w.Header().Set("Content-Disposition", `attachment; filename="better-backup-recovery.iso"`)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if err := PersonalizeISO(w, f, link.config); err != nil {
		s.log.Warn("sending the recovery disc image", "err", err)
	}
}

// PersonalizeISO copies a recovery disc image, writing the server's
// address over every placeholder in it. The image keeps its size.
func PersonalizeISO(w io.Writer, r io.Reader, c api.RecoveryConfig) error {
	cmdline := recoveryCmdline(c)
	if len(cmdline) > RecoveryConfigSize {
		return errors.New("the recovery configuration does not fit the image")
	}
	fill := []byte(cmdline + strings.Repeat(" ", RecoveryConfigSize-len(cmdline)))
	marker := []byte(RecoveryPlaceholder)
	// Read in pieces, holding back the end of each: a placeholder may
	// straddle two of them.
	buf := make([]byte, 0, 2<<20)
	tmp := make([]byte, 1<<20)
	for eof := false; !eof || len(buf) > 0; {
		if !eof {
			n, err := r.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err == io.EOF {
				eof = true
			} else if err != nil {
				return err
			}
		}
		emit := len(buf)
		if !eof {
			emit = max(0, len(buf)-len(marker)+1)
		}
		for from := 0; ; {
			i := bytes.Index(buf[from:], marker)
			if i < 0 {
				break
			}
			i += from
			if i+RecoveryConfigSize > len(buf) {
				if !eof {
					emit = i // wait for the rest of it
				}
				break
			}
			copy(buf[i:], fill)
			from = i + RecoveryConfigSize
		}
		if _, err := w.Write(buf[:emit]); err != nil {
			return err
		}
		buf = append(buf[:0], buf[emit:]...)
	}
	return nil
}

// handleRecoveryUpload stores a recovery disc image in the data
// directory, for servers that were installed without one.
func (s *Server) handleRecoveryUpload(w http.ResponseWriter, r *http.Request) {
	dest := filepath.Join(s.cfg.DataDir, "recovery.iso")
	tmp, err := os.CreateTemp(s.cfg.DataDir, ".recovery-*.iso")
	if err != nil {
		s.internal(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, 2<<30))
	found := false
	if err == nil {
		found, err = hasPlaceholder(tmp)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		fail(w, http.StatusBadRequest, "the upload failed: "+err.Error())
		return
	case n < 1<<20 || !found:
		fail(w, http.StatusBadRequest, "that file is not a better-backup recovery disc image")
		return
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		s.internal(w, err)
		return
	}
	s.log.Info("stored a new recovery disc image", "file", dest, "bytes", n)
	w.WriteHeader(http.StatusNoContent)
}

func hasPlaceholder(f *os.File) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	marker := []byte(RecoveryPlaceholder)
	buf := make([]byte, 1<<20)
	keep := 0
	for {
		n, err := f.Read(buf[keep:])
		if bytes.Contains(buf[:keep+n], marker) {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		keep = copy(buf, buf[max(0, keep+n-len(marker)):keep+n])
	}
}

// machineEntry returns the whole-machine backup of a snapshot, if any.
func machineEntry(sn *tree.Snapshot) *tree.Entry {
	for i := range sn.Sources {
		if sn.Sources[i].Type == tree.TypeMachine {
			return &sn.Sources[i]
		}
	}
	return nil
}

// handleSnapshotMachine describes the whole-machine backup in a
// snapshot, so the administrator can match its disks to new ones.
func (s *Server) handleSnapshotMachine(w http.ResponseWriter, r *http.Request) {
	sn := s.snapshot(r.PathValue("id"))
	if sn == nil {
		fail(w, http.StatusNotFound, "no such snapshot")
		return
	}
	e := machineEntry(sn)
	if e == nil {
		fail(w, http.StatusNotFound, "this snapshot holds no whole-machine backup")
		return
	}
	m, images, err := restore.ReadMachine(s.repo.Get, e)
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, describeMachine(m, images))
}

func describeMachine(m *disklayout.Machine, images map[string]*tree.Entry) api.MachineBackup {
	out := api.MachineBackup{Hostname: m.Hostname, OS: m.OS, Firmware: m.Firmware}
	for i := range m.Disks {
		d := &m.Disks[i]
		md := api.MachineDisk{Name: d.Name, Device: d.Device, Model: d.Model, Size: d.Size, SectorSize: d.SectorSize, Style: d.Style, Needs: d.Needs()}
		for k := range d.Partitions {
			p := &d.Partitions[k]
			if p.Kind == disklayout.KindExtended {
				continue
			}
			label := p.Label
			if label == "" {
				label = p.Name
			}
			md.Partitions = append(md.Partitions, api.MachinePartition{Number: p.Number, Size: p.Size, Kind: p.Kind, FS: p.FS, Label: label, Mount: p.Mount, Saved: images[p.Image] != nil})
		}
		out.Disks = append(out.Disks, md)
	}
	return out
}
