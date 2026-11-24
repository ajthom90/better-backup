package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/searchindex"
	"github.com/ajthom90/better-backup/internal/tree"
)

// The index of file names. It is a file of its own in the data folder,
// search.db, that holds nothing the repository does not: it is built
// from the snapshots in the background, and built again if it is lost.
// Backups do not wait for it, and a search that comes before it is
// built reads the snapshots themselves, as searches did before it.

// searchCatchUp is how long a search waits for a snapshot that has
// just been taken to reach the index.
const searchCatchUp = 2 * time.Second

func (s *Server) openIndex() {
	ix, err := searchindex.Open(filepath.Join(s.cfg.DataDir, "search.db"), s.repo.ID())
	if err != nil {
		s.log.Error("the index of file names could not be opened; searches read the snapshots instead", "err", err)
		s.indexErr = err.Error()
		return
	}
	s.index = ix
}

// kickIndex makes the index look at the snapshots now.
func (s *Server) kickIndex() {
	select {
	case s.indexKick <- struct{}{}:
	default:
	}
}

// indexLoop keeps the index in step with the snapshots.
func (s *Server) indexLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		s.syncIndex(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.indexKick:
		}
	}
}

func (s *Server) syncIndex(ctx context.Context) {
	s.mu.Lock()
	reset := s.indexReset
	snaps := make([]*tree.Snapshot, 0, len(s.snaps))
	for _, sn := range s.snaps {
		snaps = append(snaps, sn)
	}
	s.mu.Unlock()
	failed := func(err error) {
		if ctx.Err() != nil {
			return
		}
		s.log.Error("updating the index of file names", "err", err)
		s.mu.Lock()
		s.indexErr = err.Error()
		s.mu.Unlock()
	}
	if reset {
		if err := s.index.Reset(); err != nil {
			failed(err)
			return
		}
		// Only now may a search look at it again; until this moment
		// searches read the snapshots.
		s.mu.Lock()
		s.indexReset = false
		s.mu.Unlock()
		s.log.Info("the index of file names was emptied and is being built again")
	}
	pending, err := s.index.Reconcile(snaps)
	if err != nil {
		failed(err)
		return
	}
	s.mu.Lock()
	s.indexPending, s.indexErr = len(pending), ""
	s.mu.Unlock()
	for _, sn := range pending {
		done, err := s.beginRead()
		if err != nil {
			return // space is being reclaimed: go on after it
		}
		// Stop between chunks when the server stops or the index is
		// to be built again.
		actx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.indexCancel = cancel
		again := s.indexReset
		s.mu.Unlock()
		start := time.Now()
		if !again {
			err = s.index.Add(actx, func(id keys.ID) ([]byte, error) {
				if err := actx.Err(); err != nil {
					return nil, err
				}
				return s.repo.Get(id)
			}, sn)
		}
		cancel()
		done()
		if again || actx.Err() != nil {
			s.kickIndex()
			return
		}
		if err != nil {
			if s.snapshot(sn.ID) == nil {
				continue // forgotten while it was being read
			}
			failed(err)
			return
		}
		s.log.Debug("snapshot added to the index of file names", "snapshot", sn.ID, "took", time.Since(start).Round(time.Millisecond))
		s.mu.Lock()
		s.indexPending--
		s.mu.Unlock()
	}
	if s.index.Meta("built") == "" {
		if err := s.index.SetMeta("built", time.Now().UTC().Format(time.RFC3339)); err != nil {
			failed(err)
			return
		}
		if len(snaps) > 0 {
			st := s.index.Status()
			s.log.Info("the index of file names is built", "snapshots", st.Snapshots, "names", st.Names, "bytes", st.Bytes)
		}
	}
}

func (s *Server) indexStatus() api.SearchIndexStatus {
	s.mu.Lock()
	out := api.SearchIndexStatus{State: "off", Snapshots: len(s.snaps), Waiting: s.indexPending, Error: s.indexErr}
	reset := s.indexReset
	s.mu.Unlock()
	if s.index == nil {
		return out
	}
	st := s.index.Status()
	out.Names, out.Bytes = st.Names, st.Bytes
	out.State = "ready"
	if reset || s.index.Meta("built") == "" {
		out.State = "building"
		if reset {
			out.Waiting, out.Names, out.Bytes = out.Snapshots, 0, 0
		}
	}
	return out
}

func (s *Server) handleSearchIndex(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.indexStatus())
}

// handleRebuildIndex empties the index and has it built again.
func (s *Server) handleRebuildIndex(w http.ResponseWriter, r *http.Request) {
	if s.index == nil {
		fail(w, http.StatusConflict, "the index of file names could not be opened when the server started; see the server's log, and restart the server once the cause is put right")
		return
	}
	s.mu.Lock()
	s.indexReset = true
	if s.indexCancel != nil {
		s.indexCancel()
	}
	s.mu.Unlock()
	s.kickIndex()
	reply(w, http.StatusOK, s.indexStatus())
}

// searchIndex answers a search from the index. It reports false if the
// index cannot answer yet, and the snapshots must be read instead.
func (s *Server) searchIndex(ctx context.Context, text, agent string) (*api.SearchResult, bool, error) {
	if s.index == nil {
		return nil, false, nil
	}
	behind := func() (int, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.indexErr != "" {
			return -s.indexPending, s.indexReset // stuck: do not wait for it
		}
		return s.indexPending, s.indexReset
	}
	if _, reset := behind(); reset || s.index.Meta("built") == "" {
		return nil, false, nil
	}
	// A backup that ended a moment ago is in the index a moment later.
	for wait := time.Now().Add(searchCatchUp); time.Now().Before(wait) && ctx.Err() == nil; time.Sleep(50 * time.Millisecond) {
		if n, _ := behind(); n <= 0 {
			break
		}
		s.kickIndex()
	}
	res, err := s.index.Search(ctx, searchindex.Query{Text: text, Match: matcher(text), Agent: agent, Limit: searchLimit, Timeout: searchTime})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		res, err = &searchindex.Result{Partial: true, More: true}, nil
	}
	if err != nil {
		// Emptied under the search, perhaps. The snapshots still answer.
		s.log.Warn("the index of file names could not answer a search; reading the snapshots instead", "err", err)
		return nil, false, nil
	}
	out := &api.SearchResult{Hits: []api.SearchHit{}, Snapshots: res.Snapshots, More: res.More, Partial: res.Partial, Indexed: true}
	if out.Behind, _ = behind(); out.Behind < 0 {
		out.Behind = -out.Behind
	}
	// The same path under two policies of an agent is one file.
	byPath := map[string]int{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range res.Hits {
		newest, oldest := s.snaps[h.Newest], s.snaps[h.Oldest]
		if newest == nil || oldest == nil {
			continue // forgotten this instant
		}
		sep := "/"
		if newest.OS == "windows" {
			sep = `\`
		}
		full := h.Path[0]
		if len(h.Path) > 1 {
			full = strings.TrimRight(full, sep) + sep + strings.Join(h.Path[1:], sep)
		}
		key := newest.AgentID + "\x00" + full
		if newest.OS == "windows" {
			key = strings.ToLower(key)
		}
		hit := api.SearchHit{SnapshotID: newest.ID, Time: newest.Time, Oldest: oldest.Time, Snapshots: h.Snapshots, AgentID: newest.AgentID, Hostname: newest.Hostname,
			Path: full, Name: lastPart(full), Type: h.Type, Size: h.Size, ModTime: h.ModTime}
		at, ok := byPath[key]
		if !ok {
			byPath[key] = len(out.Hits)
			out.Hits = append(out.Hits, hit)
			continue
		}
		was := &out.Hits[at]
		hit.Snapshots += was.Snapshots
		if was.Oldest.Before(hit.Oldest) {
			hit.Oldest = was.Oldest
		}
		if was.Time.After(hit.Time) {
			hit.SnapshotID, hit.Time, hit.Hostname, hit.Path, hit.Name, hit.Type, hit.Size, hit.ModTime = was.SnapshotID, was.Time, was.Hostname, was.Path, was.Name, was.Type, was.Size, was.ModTime
		}
		*was = hit
	}
	sort.SliceStable(out.Hits, func(i, j int) bool { return out.Hits[i].Time.After(out.Hits[j].Time) })
	return out, true, nil
}
