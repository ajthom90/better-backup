package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
)

// Questions to agents. The web UI asks an agent what is on its machine
// (folders, volumes, disks, virtual machines) while a policy is written.
// A question is not a job: it is not stored, the agent answers it at
// once on a connection of its own, also while a backup runs, and it is
// forgotten when the page stops waiting.

type agentQuery struct {
	q      api.AgentQuery
	agent  string
	answer chan *api.BrowseResult
}

type queryHub struct {
	mu      sync.Mutex
	waiting map[string][]*agentQuery // agent id -> not yet fetched
	asked   map[string]*agentQuery   // query id -> not yet answered
	wake    map[string]chan struct{} // agent id -> its long poll
	seen    map[string]time.Time     // agent id -> when it last asked for questions
}

func (h *queryHub) init() {
	if h.waiting == nil {
		h.waiting, h.asked = map[string][]*agentQuery{}, map[string]*agentQuery{}
		h.wake, h.seen = map[string]chan struct{}{}, map[string]time.Time{}
	}
}

// channel returns the channel that wakes an agent's long poll.
func (h *queryHub) channel(agent string) chan struct{} {
	h.init()
	ch := h.wake[agent]
	if ch == nil {
		ch = make(chan struct{}, 1)
		h.wake[agent] = ch
	}
	return ch
}

// ask queues a question for an agent.
func (h *queryHub) ask(agent string, query api.AgentQuery) *agentQuery {
	query.ID = keys.RandomHex(12)
	q := &agentQuery{q: query, agent: agent, answer: make(chan *api.BrowseResult, 1)}
	h.mu.Lock()
	ch := h.channel(agent)
	h.waiting[agent] = append(h.waiting[agent], q)
	h.asked[q.q.ID] = q
	h.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
	return q
}

// drop forgets a question nobody waits for any more.
func (h *queryHub) drop(q *agentQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.asked, q.q.ID)
	list := h.waiting[q.agent]
	for i, x := range list {
		if x == q {
			h.waiting[q.agent] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
}

// handleNextQuery is an agent's long poll for questions.
func (s *Server) handleNextQuery(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	wait := 25 * time.Second
	if v, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && v >= 0 && v <= 55 {
		wait = time.Duration(v) * time.Second
	}
	h := &s.queries
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		h.mu.Lock()
		ch := h.channel(a.ID)
		h.seen[a.ID] = time.Now()
		var q *agentQuery
		if list := h.waiting[a.ID]; len(list) > 0 {
			q, h.waiting[a.ID] = list[0], list[1:]
		}
		h.mu.Unlock()
		if q != nil {
			reply(w, http.StatusOK, q.q)
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.stop:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// handleQueryAnswer takes an agent's answer to a question.
func (s *Server) handleQueryAnswer(w http.ResponseWriter, r *http.Request, a *api.Agent) {
	var res api.BrowseResult
	if !decode(w, r, &res) {
		return
	}
	h := &s.queries
	h.mu.Lock()
	q := h.asked[r.PathValue("id")]
	if q != nil && q.agent != a.ID {
		q = nil
	}
	h.mu.Unlock()
	if q != nil {
		select {
		case q.answer <- &res:
		default:
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// browseWait is how long the page waits for an agent's answer. A list of
// virtual machines can take a while to gather.
var browseWait = 25 * time.Second

// browseWaitUnseen is how long it waits for an agent that has not asked
// for questions since the server started. An agent that can answer asks
// all the time, so this one is away or older than the feature.
var browseWaitUnseen = 8 * time.Second

// handleBrowse asks an agent what is on its machine and waits for the
// answer.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if a == nil || a.Revoked {
		fail(w, http.StatusNotFound, "no such agent")
		return
	}
	var req api.BrowseRequest
	if !decode(w, r, &req) {
		return
	}
	switch req.Kind {
	case api.BrowseFolders, api.BrowseVolumes, api.BrowseDisks:
	case api.BrowseLog:
		// The log can name files and people; it is for administrators.
		if who(r).role != api.RoleAdmin {
			fail(w, http.StatusForbidden, "Only an administrator can read an agent's log")
			return
		}
	case api.BrowseVMs:
		req.Connection = nil
		if req.Hypervisor == "vmware" {
			if req.Connection = s.vmwareConnection(req.VMwareServer); req.Connection == nil {
				fail(w, http.StatusBadRequest, "Choose the VMware server first")
				return
			}
		}
	default:
		fail(w, http.StatusBadRequest, "unknown kind of list")
		return
	}
	if res := s.askAgent(w, r, a, api.AgentQuery{Browse: &req}); res != nil {
		reply(w, http.StatusOK, res)
	}
}

// askAgent puts a question to an agent and waits for the answer. When
// there is none it has answered the request itself, and returns nil.
func (s *Server) askAgent(w http.ResponseWriter, r *http.Request, a *api.Agent, query api.AgentQuery) *api.BrowseResult {
	if !a.Online {
		fail(w, http.StatusConflict, a.Name+" is not connected right now, so it cannot be asked. Try again when it is back.")
		return nil
	}
	h := &s.queries
	h.mu.Lock()
	h.init()
	_, asked := h.seen[a.ID]
	h.mu.Unlock()
	wait := browseWait
	if !asked {
		wait = browseWaitUnseen
	}
	q := h.ask(a.ID, query)
	defer h.drop(q)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case res := <-q.answer:
		if res.Entries == nil {
			res.Entries = []api.BrowseEntry{}
		}
		return res
	case <-timer.C:
		if !asked {
			fail(w, http.StatusConflict, a.Name+" has not answered. It may be switched off, or its agent may be older than this server: install the newest agent on it, or type the name instead.")
			return nil
		}
		fail(w, http.StatusGatewayTimeout, a.Name+" did not answer in time. Try again, or type the name instead.")
	case <-s.stop:
		fail(w, http.StatusServiceUnavailable, "the server is stopping")
	case <-r.Context().Done():
	}
	return nil
}

// handleDSRM restarts a domain controller into Directory Services
// Restore Mode, or back out of it, so that nobody has to type bcdedit
// on the machine before Active Directory is restored.
func (s *Server) handleDSRM(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	if a == nil || a.Revoked {
		fail(w, http.StatusNotFound, "no such agent")
		return
	}
	var req struct {
		Leave    bool   `json:"leave"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) || !s.confirmPassword(w, r, req.Password) {
		return
	}
	action := api.ActionEnterDSRM
	if req.Leave {
		action = api.ActionLeaveDSRM
	}
	res := s.askAgent(w, r, a, api.AgentQuery{Action: action})
	if res == nil {
		return
	}
	if res.Error != "" {
		fail(w, http.StatusConflict, res.Error)
		return
	}
	note(r, "%s: %s", a.Name, res.Text)
	reply(w, http.StatusOK, res)
}
