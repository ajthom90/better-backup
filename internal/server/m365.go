package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/m365"
	"github.com/ajthom90/better-backup/internal/tree"
)

const m365SettingsKey = "m365_tenants"

// m365Tenants returns the stored tenants, client secrets included.
func (s *Server) m365Tenants() []api.M365Tenant {
	var out []api.M365Tenant
	if v := s.setting(m365SettingsKey); v != "" {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

func (s *Server) m365Tenant(id string) *api.M365Tenant {
	for _, t := range s.m365Tenants() {
		if t.ID == id {
			return &t
		}
	}
	return nil
}

// m365Connection is what an agent needs to sign in to a tenant, or nil
// if there is no such tenant any more.
func (s *Server) m365Connection(id string) *api.M365Connection {
	t := s.m365Tenant(id)
	if t == nil {
		return nil
	}
	return &api.M365Connection{TenantID: t.TenantID, ClientID: t.ClientID, ClientSecret: t.ClientSecret, LoginURL: t.LoginURL, GraphURL: t.GraphURL}
}

func m365Client(t *api.M365Tenant) *m365.Client {
	return m365.New(m365.Connection{TenantID: t.TenantID, ClientID: t.ClientID, ClientSecret: t.ClientSecret, LoginURL: t.LoginURL, GraphURL: t.GraphURL})
}

// publicM365 is a tenant as the admin API shows it: without its secret.
func publicM365(t api.M365Tenant) api.M365Tenant {
	t.SecretSet, t.ClientSecret = t.ClientSecret != "", ""
	return t
}

func (s *Server) handleM365Tenants(w http.ResponseWriter, r *http.Request) {
	out := []api.M365Tenant{}
	for _, t := range s.m365Tenants() {
		out = append(out, publicM365(t))
	}
	reply(w, http.StatusOK, out)
}

// cleanM365 tidies a tenant that was sent in, takes the stored secret
// where it was left out, and says what is wrong with it, if anything.
func (s *Server) cleanM365(t *api.M365Tenant, list []api.M365Tenant) string {
	for i := range list {
		if list[i].ID == t.ID && t.ID != "" && t.ClientSecret == "" && t.SecretSet {
			t.ClientSecret = list[i].ClientSecret
		}
	}
	t.SecretSet = false
	t.Name, t.TenantID, t.ClientID, t.ClientSecret = strings.TrimSpace(t.Name), strings.TrimSpace(t.TenantID), strings.TrimSpace(t.ClientID), strings.TrimSpace(t.ClientSecret)
	t.LoginURL, t.GraphURL = strings.TrimRight(strings.TrimSpace(t.LoginURL), "/"), strings.TrimRight(strings.TrimSpace(t.GraphURL), "/")
	switch {
	case t.TenantID == "":
		return "a tenant needs its directory (tenant) id, or one of its domain names such as example.onmicrosoft.com"
	case strings.ContainsAny(t.TenantID, " /\\?#@"):
		return "the directory (tenant) id is an id such as 11111111-2222-3333-4444-555555555555, or a domain name"
	case t.ClientID == "":
		return "a tenant needs the application (client) id of the app registration"
	case t.ClientSecret == "":
		return "a tenant needs the client secret of the app registration: its Value, not its Secret ID"
	case (t.LoginURL == "") != (t.GraphURL == ""):
		return "for another Microsoft cloud give both addresses, the sign-in service and Graph"
	}
	for _, a := range []string{t.LoginURL, t.GraphURL} {
		if a == "" {
			continue
		}
		if u, err := url.Parse(a); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" {
			return "the addresses for another Microsoft cloud must look like https://graph.microsoft.us"
		}
	}
	if t.Name == "" {
		t.Name = t.TenantID
	}
	return ""
}

func (s *Server) handleSaveM365Tenant(w http.ResponseWriter, r *http.Request) {
	var t api.M365Tenant
	if !decode(w, r, &t) {
		return
	}
	s.vmwareMu.Lock()
	defer s.vmwareMu.Unlock()
	list := s.m365Tenants()
	at := -1
	t.ID = r.PathValue("id")
	if t.ID != "" {
		for i := range list {
			if list[i].ID == t.ID {
				at = i
			}
		}
		if at < 0 {
			fail(w, http.StatusNotFound, "no such Microsoft 365 tenant")
			return
		}
	}
	if msg := s.cleanM365(&t, list); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	for i := range list {
		if i != at && strings.EqualFold(list[i].Name, t.Name) {
			fail(w, http.StatusBadRequest, "another Microsoft 365 tenant is already called "+t.Name)
			return
		}
	}
	if at < 0 {
		t.ID = keys.RandomHex(6)
		list = append(list, t)
	} else {
		list[at] = t
	}
	sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	b, _ := json.Marshal(list)
	if err := s.setSetting(m365SettingsKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "%s", t.Name)
	reply(w, http.StatusOK, publicM365(t))
}

func (s *Server) handleDeleteM365Tenant(w http.ResponseWriter, r *http.Request) {
	s.vmwareMu.Lock()
	defer s.vmwareMu.Unlock()
	id := r.PathValue("id")
	var keep []api.M365Tenant
	name := ""
	for _, t := range s.m365Tenants() {
		if t.ID != id {
			keep = append(keep, t)
		} else {
			name = t.Name
		}
	}
	if name == "" {
		fail(w, http.StatusNotFound, "no such Microsoft 365 tenant")
		return
	}
	ps, err := s.policies(``)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, p := range ps {
		if p.M365 != nil && p.M365.Tenant == id {
			fail(w, http.StatusConflict, "the policy "+p.Name+" backs up this tenant; change or delete it first")
			return
		}
	}
	b, _ := json.Marshal(keep)
	if err := s.setSetting(m365SettingsKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "%s", name)
	w.WriteHeader(http.StatusNoContent)
}

// m365Users signs in to a tenant from the server and lists its users.
func m365Users(ctx context.Context, t *api.M365Tenant) ([]api.M365User, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	us, err := m365Client(t).Users(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	out := make([]api.M365User, len(us))
	for i, u := range us {
		out[i] = api.M365User{UPN: u.UPN, Name: u.Name, Licensed: u.Licensed, Guest: u.Guest}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].UPN) < strings.ToLower(out[j].UPN) })
	return out, nil
}

func m365Refused(w http.ResponseWriter, err error) {
	if err == context.DeadlineExceeded {
		fail(w, http.StatusBadGateway, "Microsoft 365 did not answer in time; this server may not reach the internet. A backup still works if the agent that runs it does.")
		return
	}
	fail(w, http.StatusBadGateway, err.Error())
}

func (s *Server) handleM365Users(w http.ResponseWriter, r *http.Request) {
	t := s.m365Tenant(r.PathValue("id"))
	if t == nil {
		fail(w, http.StatusNotFound, "no such Microsoft 365 tenant")
		return
	}
	us, err := m365Users(r.Context(), t)
	if err != nil {
		m365Refused(w, err)
		return
	}
	reply(w, http.StatusOK, us)
}

// handleTestM365Tenant tries a tenant as it was typed, before it is
// saved: it signs in and counts the users.
func (s *Server) handleTestM365Tenant(w http.ResponseWriter, r *http.Request) {
	var t api.M365Tenant
	if !decode(w, r, &t) {
		return
	}
	if msg := s.cleanM365(&t, s.m365Tenants()); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	us, err := m365Users(r.Context(), &t)
	if err != nil {
		m365Refused(w, err)
		return
	}
	n := 0
	for _, u := range us {
		if u.Licensed && !u.Guest {
			n++
		}
	}
	reply(w, http.StatusOK, map[string]int{"users": len(us), "licensed": n})
}

// checkM365 tidies the Microsoft 365 part of a policy and says what is
// wrong with it, if anything.
func (s *Server) checkM365(p *api.Policy) string {
	m := p.M365
	if m == nil {
		return ""
	}
	if s.m365Tenant(m.Tenant) == nil {
		return "choose the Microsoft 365 tenant; tenants are added under Microsoft 365"
	}
	if !m.Mail && !m.OneDrive {
		return "choose what to back up from Microsoft 365: mail, OneDrive or both"
	}
	var users []string
	seen := map[string]bool{}
	for _, u := range m.Users {
		u = strings.TrimSpace(u)
		if u == "" || seen[strings.ToLower(u)] {
			continue
		}
		if u != "*" && (!strings.Contains(u, "@") || strings.ContainsAny(u, " /\\")) {
			return "Microsoft 365 users are named by their sign-in name, such as alice@example.com; * stands for everyone with a licence"
		}
		seen[strings.ToLower(u)] = true
		users = append(users, u)
	}
	if len(users) == 0 {
		return "name the Microsoft 365 users to back up, or * for everyone with a licence"
	}
	m.Users = users
	return ""
}

// coversM365 reports whether a restore of include (everything, if it is
// empty) from sn takes in what came from Microsoft 365.
func coversM365(sn *tree.Snapshot, include []string) bool {
	has := false
	for _, e := range sn.Sources {
		has = has || e.Name == tree.M365Name
	}
	if !has {
		return false
	}
	if len(include) == 0 {
		return true
	}
	for _, inc := range include {
		inc = strings.ReplaceAll(inc, `\`, "/")
		if inc == tree.M365Name || strings.HasPrefix(inc, tree.M365Name+"/") {
			return true
		}
	}
	return false
}
