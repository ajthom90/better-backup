package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
)

// sessionIdle is how long a sign-in lasts without being used.
const sessionIdle = 12 * time.Hour

const minPassword = 10

// principal is whoever makes an admin API request.
type principal struct {
	id, name, role string
	// token: the admin token, not a person.
	token bool
	// session is the hash of the bearer token of a signed-in user.
	session string
}

func roleRank(role string) int {
	switch role {
	case api.RoleAdmin:
		return 3
	case api.RoleOperator:
		return 2
	case api.RoleViewer:
		return 1
	}
	return 0
}

// Password hashes are Argon2id with the parameters stored beside the
// hash, so they can be raised later without locking anyone out.
func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	const t, m, p = 2, 19 * 1024, 1
	sum := argon2.IDKey([]byte(pw), salt, t, m, p, 32)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s", t, m, p, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum))
}

func checkPassword(hash, pw string) bool {
	f := strings.Split(hash, "$")
	if len(f) != 6 || f[0] != "argon2id" {
		return false
	}
	t, _ := strconv.ParseUint(f[1], 10, 32)
	m, _ := strconv.ParseUint(f[2], 10, 32)
	p, _ := strconv.ParseUint(f[3], 10, 8)
	salt, err1 := base64.RawStdEncoding.DecodeString(f[4])
	want, err2 := base64.RawStdEncoding.DecodeString(f[5])
	if err1 != nil || err2 != nil || t == 0 || m == 0 || p == 0 || m > 1<<20 || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked when a name is unknown, so that a wrong name and
// a wrong password take the same time.
var dummyHash = sync.OnceValue(func() string { return hashPassword(keys.RandomHex(16)) })

func checkNewPassword(pw string) error {
	if len(pw) < minPassword {
		return fmt.Errorf("the password must have at least %d characters", minPassword)
	}
	if len(pw) > 1024 {
		return errors.New("the password is too long")
	}
	return nil
}

func checkUserName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsAny(name, "\r\n\t") {
		return "", errors.New("the name must have between 1 and 64 characters")
	}
	return name, nil
}

const userCols = `id, name, role, disabled, created, last_login`

func scanUser(row scanner) (*api.User, string, error) {
	var u api.User
	var hash string
	var created, last int64
	if err := row.Scan(&u.ID, &u.Name, &u.Role, &u.Disabled, &created, &last, &hash); err != nil {
		return nil, "", err
	}
	u.Created = fromMS(created)
	if last != 0 {
		t := fromMS(last)
		u.LastLogin = &t
	}
	return &u, hash, nil
}

func (s *Server) userWhere(where string, args ...any) (*api.User, string, error) {
	u, hash, err := scanUser(s.db.QueryRow(`SELECT `+userCols+`, hash FROM users `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	return u, hash, err
}

func (s *Server) userCount() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// activeAdmins counts the administrators who can sign in, leaving out
// one user.
func (s *Server) activeAdmins(except string) int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0 AND id != ?`, api.RoleAdmin, except).Scan(&n)
	return n
}

func (s *Server) createUser(name, password, role string) (*api.User, error) {
	name, err := checkUserName(name)
	if err != nil {
		return nil, err
	}
	if roleRank(role) == 0 {
		return nil, errors.New(`the role must be "admin", "operator" or "viewer"`)
	}
	if err := checkNewPassword(password); err != nil {
		return nil, err
	}
	if u, _, _ := s.userWhere(`WHERE name = ? COLLATE NOCASE`, name); u != nil {
		return nil, fmt.Errorf("a user named %s already exists", u.Name)
	}
	u := &api.User{ID: keys.RandomHex(8), Name: name, Role: role, Created: time.Now()}
	_, err = s.db.Exec(`INSERT INTO users (id, name, hash, role, created) VALUES (?, ?, ?, ?, ?)`, u.ID, u.Name, hashPassword(password), u.Role, ms(u.Created))
	return u, err
}

func (s *Server) newSession(u *api.User) (*api.Session, error) {
	tok := keys.RandomHex(32)
	now := time.Now()
	exp := now.Add(sessionIdle)
	s.db.Exec(`DELETE FROM sessions WHERE expires < ?`, ms(now))
	if _, err := s.db.Exec(`INSERT INTO sessions (hash, user_id, created, expires) VALUES (?, ?, ?, ?)`, hashToken(tok), u.ID, ms(now), ms(exp)); err != nil {
		return nil, err
	}
	s.db.Exec(`UPDATE users SET last_login = ? WHERE id = ?`, ms(now), u.ID)
	return &api.Session{Token: tok, Expires: exp, User: *u}, nil
}

// authAdmin identifies the caller of an admin API request from its
// bearer token: the admin token, or a signed-in user's session.
func (s *Server) authAdmin(r *http.Request) *principal {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil
	}
	h := hashToken(tok)
	if subtle.ConstantTimeCompare([]byte(h), []byte(s.setting("admin_token_hash"))) == 1 {
		return &principal{name: "admin token", role: api.RoleAdmin, token: true}
	}
	var p principal
	var expires int64
	var disabled bool
	err := s.db.QueryRow(`SELECT u.id, u.name, u.role, u.disabled, s.expires FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.hash = ?`, h).
		Scan(&p.id, &p.name, &p.role, &disabled, &expires)
	now := time.Now()
	if err != nil || disabled || fromMS(expires).Before(now) {
		return nil
	}
	// Using a session keeps it alive. Writing that at most once a minute
	// keeps the page's polling from turning into a write per request.
	if fromMS(expires).Sub(now) < sessionIdle-time.Minute {
		s.db.Exec(`UPDATE sessions SET expires = ? WHERE hash = ?`, ms(now.Add(sessionIdle)), h)
	}
	p.session = h
	return &p
}

type ctxKey int

const (
	principalKey ctxKey = iota
	auditKey
)

func who(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey).(*principal)
	if p == nil {
		return &principal{name: "nobody"}
	}
	return p
}

func withPrincipal(r *http.Request, p *principal, a *auditNote) *http.Request {
	ctx := context.WithValue(r.Context(), principalKey, p)
	return r.WithContext(context.WithValue(ctx, auditKey, a))
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// loginGuard slows down password guessing: after five failures in a row
// for a name, or twenty from an address, further tries must wait, longer
// each time, up to a quarter of an hour. An address gets more room
// because several people may share one.
type loginGuard struct {
	mu sync.Mutex
	m  map[string]*loginFails
}

type loginFails struct {
	n     int
	until time.Time
	last  time.Time
}

func (g *loginGuard) wait(keys ...string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	var d time.Duration
	for _, k := range keys {
		if f := g.m[k]; f != nil {
			d = max(d, time.Until(f.until))
		}
	}
	return d
}

func (g *loginGuard) failed(keys ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*loginFails{}
	}
	now := time.Now()
	for k, f := range g.m {
		if now.Sub(f.last) > time.Hour {
			delete(g.m, k)
		}
	}
	for _, k := range keys {
		f := g.m[k]
		if f == nil {
			f = &loginFails{}
			g.m[k] = f
		}
		f.n++
		f.last = now
		limit := 5
		if strings.HasPrefix(k, "addr:") {
			limit = 20
		}
		if f.n >= limit {
			f.until = now.Add(min(time.Duration(1<<min(f.n-limit, 10))*30*time.Second, 15*time.Minute))
		}
	}
}

func (g *loginGuard) succeeded(keys ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		delete(g.m, k)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req api.LoginRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	byName, byAddr := "name:"+strings.ToLower(name), "addr:"+remoteHost(r)
	if d := s.logins.wait(byName, byAddr); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		fail(w, http.StatusTooManyRequests, fmt.Sprintf("Too many failed sign-ins. Try again in %s.", d.Round(time.Second)+time.Second))
		return
	}
	u, hash, err := s.userWhere(`WHERE name = ? COLLATE NOCASE`, name)
	if err != nil {
		s.internal(w, err)
		return
	}
	if u == nil {
		hash = dummyHash()
	}
	if !checkPassword(hash, req.Password) || u == nil || u.Disabled {
		s.logins.failed(byName, byAddr)
		s.audit(name, remoteHost(r), "Failed to sign in", "")
		fail(w, http.StatusUnauthorized, "Wrong name or password")
		return
	}
	s.logins.succeeded(byName)
	sess, err := s.newSession(u)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.audit(u.Name, remoteHost(r), "Signed in", "")
	reply(w, http.StatusOK, sess)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if p := who(r); p.session != "" {
		s.db.Exec(`DELETE FROM sessions WHERE hash = ?`, p.session)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if p.token {
		reply(w, http.StatusOK, api.User{Name: p.name, Role: p.role, Token: true})
		return
	}
	u, _, err := s.userWhere(`WHERE id = ?`, p.id)
	if err != nil || u == nil {
		fail(w, http.StatusUnauthorized, "signed out")
		return
	}
	reply(w, http.StatusOK, u)
}

// handleMyPassword lets anyone change their own password, given the old.
func (s *Server) handleMyPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := who(r)
	if p.token {
		fail(w, http.StatusBadRequest, "the admin token has no password; sign in as a user")
		return
	}
	_, hash, err := s.userWhere(`WHERE id = ?`, p.id)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !checkPassword(hash, req.Old) {
		fail(w, http.StatusForbidden, "The current password is wrong")
		return
	}
	if err := checkNewPassword(req.New); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.db.Exec(`UPDATE users SET hash = ? WHERE id = ?`, hashPassword(req.New), p.id); err != nil {
		s.internal(w, err)
		return
	}
	// Every other sign-in of this user ends.
	s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND hash != ?`, p.id, p.session)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT ` + userCols + `, hash FROM users ORDER BY name COLLATE NOCASE`)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	out := []*api.User{}
	for rows.Next() {
		u, _, err := scanUser(rows)
		if err != nil {
			s.internal(w, err)
			return
		}
		out = append(out, u)
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req api.User
	if !decode(w, r, &req) {
		return
	}
	u, err := s.createUser(req.Name, req.Password, req.Role)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	note(r, "%s (%s)", u.Name, u.Role)
	reply(w, http.StatusCreated, u)
}

// handleUpdateUser changes a user's name, role, password or whether
// they may sign in. The last administrator cannot be locked out.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req api.User
	if !decode(w, r, &req) {
		return
	}
	u, _, err := s.userWhere(`WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if u == nil {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	name, err := checkUserName(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if roleRank(req.Role) == 0 {
		fail(w, http.StatusBadRequest, `the role must be "admin", "operator" or "viewer"`)
		return
	}
	if other, _, _ := s.userWhere(`WHERE name = ? COLLATE NOCASE AND id != ?`, name, u.ID); other != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("a user named %s already exists", other.Name))
		return
	}
	if (req.Role != api.RoleAdmin || req.Disabled) && s.activeAdmins(u.ID) == 0 {
		fail(w, http.StatusBadRequest, "This is the only administrator who can sign in. Make someone else an administrator first.")
		return
	}
	if req.Password != "" {
		if err := checkNewPassword(req.Password); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := s.db.Exec(`UPDATE users SET hash = ? WHERE id = ?`, hashPassword(req.Password), u.ID); err != nil {
			s.internal(w, err)
			return
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET name = ?, role = ?, disabled = ? WHERE id = ?`, name, req.Role, req.Disabled, u.ID); err != nil {
		s.internal(w, err)
		return
	}
	var changes []string
	if name != u.Name {
		changes = append(changes, "renamed from "+u.Name)
	}
	if req.Role != u.Role {
		changes = append(changes, "role "+u.Role+" to "+req.Role)
	}
	if req.Disabled != u.Disabled {
		changes = append(changes, map[bool]string{true: "sign-in turned off", false: "sign-in turned on"}[req.Disabled])
	}
	if req.Password != "" {
		changes = append(changes, "new password")
	}
	// A changed password, role or lock ends the user's sign-ins, except
	// the one making the change.
	if req.Password != "" || req.Disabled || req.Role != u.Role {
		s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND hash != ?`, u.ID, who(r).session)
	}
	note(r, "%s: %s", name, strings.Join(changes, ", "))
	u, _, _ = s.userWhere(`WHERE id = ?`, u.ID)
	reply(w, http.StatusOK, u)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, _, err := s.userWhere(`WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if u == nil {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	if s.activeAdmins(u.ID) == 0 {
		fail(w, http.StatusBadRequest, "This is the only administrator who can sign in. Make someone else an administrator first.")
		return
	}
	s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, u.ID)
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "%s", u.Name)
	w.WriteHeader(http.StatusNoContent)
}
