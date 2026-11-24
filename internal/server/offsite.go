package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/offsite"
)

const offsiteSettingsKey = "offsite"

// offsiteSettings returns the stored settings, including the secret key.
func (s *Server) offsiteSettings() api.OffsiteSettings {
	var o api.OffsiteSettings
	if v := s.setting(offsiteSettingsKey); v != "" {
		json.Unmarshal([]byte(v), &o)
	}
	return o
}

func checkOffsiteSettings(o *api.OffsiteSettings) error {
	o.Folder, o.Endpoint, o.Bucket = strings.TrimSpace(o.Folder), strings.TrimSpace(o.Endpoint), strings.TrimSpace(o.Bucket)
	o.Region, o.Prefix, o.AccessKey = strings.TrimSpace(o.Region), strings.TrimSpace(o.Prefix), strings.TrimSpace(o.AccessKey)
	o.DailyAt = strings.TrimSpace(o.DailyAt)
	if o.DailyAt != "" {
		if _, _, err := parseDailyAt(o.DailyAt); err != nil {
			return errors.New("the daily time must look like 03:00")
		}
	}
	if o.Kind == "" && !o.Enabled {
		return nil
	}
	if o.Kind != "folder" && o.Kind != "s3" {
		return errors.New(`kind must be "folder" or "s3"`)
	}
	if !o.Enabled {
		return nil
	}
	_, err := offsiteTarget(*o)
	return err
}

func offsiteTarget(o api.OffsiteSettings) (offsite.Target, error) {
	switch o.Kind {
	case "folder":
		return offsite.NewDir(o.Folder)
	case "s3":
		return offsite.NewS3(offsite.S3Config{Endpoint: o.Endpoint, Region: o.Region, Bucket: o.Bucket, Prefix: o.Prefix,
			AccessKey: o.AccessKey, SecretKey: o.SecretKey, VirtualHost: o.VirtualHost})
	}
	return nil, errors.New("the offsite copy is not set up")
}

func (s *Server) offsiteStatus() api.OffsiteStatus {
	st := api.OffsiteStatus{OffsiteSettings: s.offsiteSettings()}
	st.SecretKeySet, st.SecretKey = st.SecretKey != "", ""
	if js, err := s.jobs(`WHERE type = ? ORDER BY created DESC LIMIT 1`, api.JobOffsite); err == nil && len(js) > 0 {
		st.LastJob = js[0]
	}
	if js, err := s.jobs(`WHERE type = ? AND status = ? ORDER BY created DESC LIMIT 1`, api.JobOffsite, api.StatusSuccess); err == nil && len(js) > 0 {
		st.LastSuccess = js[0].Finished
	}
	return st
}

func (s *Server) handleOffsite(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.offsiteStatus())
}

func (s *Server) handleSaveOffsite(w http.ResponseWriter, r *http.Request) {
	var o api.OffsiteSettings
	if !decode(w, r, &o) {
		return
	}
	if o.SecretKey == "" && o.SecretKeySet {
		o.SecretKey = s.offsiteSettings().SecretKey
	}
	o.SecretKeySet = false
	if err := checkOffsiteSettings(&o); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	o.Saved = time.Now()
	b, _ := json.Marshal(o)
	if err := s.setSetting(offsiteSettingsKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusOK, s.offsiteStatus())
}

// handleTestOffsite checks that the saved place can be listed, written
// to and deleted from, and reports what it holds.
func (s *Server) handleTestOffsite(w http.ResponseWriter, r *http.Request) {
	t, err := offsiteTarget(s.offsiteSettings())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	// Listing first: it fails at once when the place cannot be reached.
	objs, err := t.List(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, "listing "+t.String()+" failed: "+err.Error())
		return
	}
	const probe = "write-test.tmp-probe"
	if err := t.Put(ctx, probe, bytes.NewReader([]byte("better-backup")), 13); err != nil {
		fail(w, http.StatusBadGateway, "writing to "+t.String()+" failed: "+err.Error())
		return
	}
	if err := t.Delete(ctx, probe); err != nil {
		fail(w, http.StatusBadGateway, "deleting from "+t.String()+" failed: "+err.Error())
		return
	}
	var res api.OffsiteTest
	for _, o := range objs {
		res.Objects++
		res.Bytes += o.Size
	}
	reply(w, http.StatusOK, res)
}

func (s *Server) handleRunOffsite(w http.ResponseWriter, r *http.Request) {
	if _, err := offsiteTarget(s.offsiteSettings()); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	j, err := s.enqueueOffsite()
	if err != nil {
		s.internal(w, err)
		return
	}
	reply(w, http.StatusAccepted, j)
}

// enqueueOffsite queues a copy job, or returns the one already waiting
// or running.
func (s *Server) enqueueOffsite() (*api.Job, error) {
	js, err := s.jobs(`WHERE type = ? AND status IN (?, ?) ORDER BY created LIMIT 1`, api.JobOffsite, api.StatusQueued, api.StatusRunning)
	if err != nil {
		return nil, err
	}
	if len(js) > 0 {
		return js[0], nil
	}
	j := &api.Job{Type: api.JobOffsite}
	return j, s.enqueue(j)
}

// scheduleOffsite queues the daily copy when its time has come.
func (s *Server) scheduleOffsite(now time.Time) {
	o := s.offsiteSettings()
	if !o.Enabled || o.DailyAt == "" {
		return
	}
	h, m, err := parseDailyAt(o.DailyAt)
	if err != nil {
		return
	}
	slot := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if now.Before(slot) {
		slot = slot.AddDate(0, 0, -1)
	}
	var created int64
	s.db.QueryRow(`SELECT COALESCE(MAX(created), 0) FROM jobs WHERE type = ?`, api.JobOffsite).Scan(&created)
	last := fromMS(created)
	if last.IsZero() || last.Before(o.Saved) {
		last = o.Saved // newly set up: wait for the first slot
	}
	if !last.Before(slot) {
		return
	}
	if _, err := s.enqueueOffsite(); err != nil {
		s.log.Error("scheduling the offsite copy", "err", err)
	}
}

// runOffsite starts a queued copy job. Copying runs beside backups and
// restores, which only add files, but never beside garbage collection.
func (s *Server) runOffsite(ctx context.Context) {
	js, err := s.jobs(`WHERE type = ? AND status = ? ORDER BY created LIMIT 1`, api.JobOffsite, api.StatusQueued)
	if err != nil || len(js) == 0 {
		return
	}
	s.mu.Lock()
	if s.maint || s.offsiteBusy {
		s.mu.Unlock()
		return // retried when that finishes
	}
	s.offsiteBusy = true
	s.mu.Unlock()
	j := js[0]
	s.db.Exec(`UPDATE jobs SET status = ?, started = ? WHERE id = ?`, api.StatusRunning, ms(time.Now()), j.ID)
	s.log.Info("offsite copy started", "job", j.ID)
	go func() {
		var res offsite.Result
		t, err := offsiteTarget(s.offsiteSettings())
		if err == nil {
			// Finish the pack being written, so the copy includes
			// everything backed up so far.
			if err = s.repo.Flush(); err == nil {
				// The copy also carries the server's own configuration.
				if serr := s.saveState(); serr != nil {
					s.log.Error("saving the server's configuration into the repository", "err", serr)
				}
				last := time.Now()
				res, err = offsite.Sync(ctx, s.cfg.RepoDir, t, func(r offsite.Result) {
					if time.Since(last) < 2*time.Second {
						return
					}
					last = time.Now()
					b, _ := json.Marshal(r)
					s.db.Exec(`UPDATE jobs SET progress = ?, heartbeat = ? WHERE id = ?`, string(b), ms(last), j.ID)
				})
			}
		}
		s.mu.Lock()
		s.offsiteBusy = false
		s.mu.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				err = errors.New("the server stopped before the copy finished; it continues with the next run")
			}
			s.finish(j, api.StatusFailed, err.Error(), res, nil)
		} else {
			s.finish(j, api.StatusSuccess, "", res, nil)
		}
	}()
}
