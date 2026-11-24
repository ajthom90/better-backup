package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

const vmwareSettingsKey = "vmware_servers"

// vmwareServers returns the stored VMware servers, passwords included.
func (s *Server) vmwareServers() []api.VMwareServer {
	var out []api.VMwareServer
	if v := s.setting(vmwareSettingsKey); v != "" {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

func (s *Server) vmwareServer(id string) *api.VMwareServer {
	for _, v := range s.vmwareServers() {
		if v.ID == id {
			return &v
		}
	}
	return nil
}

// vmwareConnection is what an agent needs to reach a VMware server, or
// nil if there is no such server any more.
func (s *Server) vmwareConnection(id string) *hypervisor.Connection {
	v := s.vmwareServer(id)
	if v == nil {
		return nil
	}
	return &hypervisor.Connection{URL: v.URL, Username: v.Username, Password: v.Password, Thumbprint: v.Thumbprint, Insecure: v.Insecure}
}

func (s *Server) saveVMwareServers(list []api.VMwareServer) error {
	sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	b, _ := json.Marshal(list)
	return s.setSetting(vmwareSettingsKey, string(b))
}

// public is a server as the admin API shows it: without its password.
func publicVMware(v api.VMwareServer) api.VMwareServer {
	v.PasswordSet, v.Password = v.Password != "", ""
	return v
}

func (s *Server) handleVMwareServers(w http.ResponseWriter, r *http.Request) {
	out := []api.VMwareServer{}
	for _, v := range s.vmwareServers() {
		out = append(out, publicVMware(v))
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) handleSaveVMwareServer(w http.ResponseWriter, r *http.Request) {
	var v api.VMwareServer
	if !decode(w, r, &v) {
		return
	}
	s.vmwareMu.Lock()
	defer s.vmwareMu.Unlock()
	list := s.vmwareServers()
	at := -1
	if id := r.PathValue("id"); id != "" {
		for i := range list {
			if list[i].ID == id {
				at = i
			}
		}
		if at < 0 {
			fail(w, http.StatusNotFound, "no such VMware server")
			return
		}
		v.ID = id
		if v.Password == "" && v.PasswordSet {
			v.Password = list[at].Password
		}
	} else {
		v.ID = keys.RandomHex(6)
	}
	v.PasswordSet = false
	v.Name, v.URL, v.Username = strings.TrimSpace(v.Name), strings.TrimSpace(v.URL), strings.TrimSpace(v.Username)
	v.Thumbprint = strings.ToUpper(strings.TrimSpace(v.Thumbprint))
	bad := func(msg string) { fail(w, http.StatusBadRequest, msg) }
	switch {
	case v.URL == "":
		bad("a VMware server needs an address, such as vcenter.example.com")
		return
	case strings.ContainsAny(v.URL, " \t@"):
		bad("the address must not hold spaces or a user name; the account has its own fields")
		return
	case v.Username == "" || v.Password == "":
		bad("a VMware server needs a user name and a password")
		return
	}
	if hex := strings.NewReplacer(":", "", " ", "", "-", "").Replace(v.Thumbprint); hex != "" {
		if (len(hex) != 40 && len(hex) != 64) || strings.Trim(hex, "0123456789ABCDEF") != "" {
			bad("the thumbprint must be the SHA-256 or SHA-1 of the certificate, as hexadecimal digits")
			return
		}
	}
	if v.Name == "" {
		v.Name = v.URL
	}
	for i := range list {
		if i != at && strings.EqualFold(list[i].Name, v.Name) {
			bad("another VMware server is already called " + v.Name)
			return
		}
	}
	if at < 0 {
		list = append(list, v)
	} else {
		list[at] = v
	}
	if err := s.saveVMwareServers(list); err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, publicVMware(v))
}

func (s *Server) handleDeleteVMwareServer(w http.ResponseWriter, r *http.Request) {
	s.vmwareMu.Lock()
	defer s.vmwareMu.Unlock()
	id := r.PathValue("id")
	list := s.vmwareServers()
	keep := list[:0]
	for _, v := range list {
		if v.ID != id {
			keep = append(keep, v)
		}
	}
	if len(keep) == len(list) {
		fail(w, http.StatusNotFound, "no such VMware server")
		return
	}
	ps, err := s.policies(``)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, p := range ps {
		if p.Hypervisor == "vmware" && p.VMwareServer == id {
			fail(w, http.StatusConflict, "the policy "+p.Name+" backs up machines of this server; change or delete it first")
			return
		}
	}
	if err := s.saveVMwareServers(keep); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restoreVMware says which VMware server a restore of a snapshot goes
// to when none was asked for: the one the snapshot's policy names.
func (s *Server) restoreVMware(sn *tree.Snapshot) string {
	p, err := s.policy(sn.PolicyID)
	if err != nil || p == nil || p.Hypervisor != "vmware" || s.vmwareServer(p.VMwareServer) == nil {
		return ""
	}
	return p.VMwareServer
}
