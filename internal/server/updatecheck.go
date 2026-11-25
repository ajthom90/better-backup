package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	betterbackup "github.com/ajthom90/better-backup"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/version"
)

// Telling the administrator that a newer server is out.
//
// Twice a day the server reads the list of releases the project
// publishes on GitHub: one plain request, the same a browser makes. It
// sends nothing about this server: no identifier, no version, no
// counts. The answer is compared here. If a release on the chosen
// channel is newer than the running version, the web UI says so and
// shows its release notes, and an alert goes out once per version. The
// check can be turned off in the web UI. Nothing is downloaded or
// installed by itself.
//
// The channels are "stable" (releases) and "edge" (every build from the
// main branch, published as the release tagged "edge"). docs/versioning.md
// has the whole policy.

// DefaultUpdateURL lists the releases of better-backup.
const DefaultUpdateURL = "https://api.github.com/repos/ajthom90/better-backup/releases?per_page=30"

const (
	updateSettingsKey = "update_check"
	updateFoundKey    = "update_found"
	updateEvery       = 12 * time.Hour
	updateRetry       = time.Hour
	// maxNotes bounds the release notes kept of one release.
	maxNotes = 20000
)

// updateSettings are stored as they are; the zero value is the default:
// check, alert, and the channel this build came from.
type updateSettings struct {
	Off     bool   `json:"off,omitempty"`
	Channel string `json:"channel,omitempty"`
	NoAlert bool   `json:"no_alert,omitempty"`
}

// updateFound is what the last check learned.
type updateFound struct {
	Checked  time.Time           `json:"checked"`
	Channel  string              `json:"channel"`
	Error    string              `json:"error,omitempty"`
	Releases []api.ServerRelease `json:"releases,omitempty"` // newest first
	// Alerted is the version the last alert was about.
	Alerted string `json:"alerted,omitempty"`
}

// buildChannel is the channel the running program came from.
func buildChannel() string {
	if v, ok := version.Parse(Version); ok && v.Stable() {
		return api.ChannelStable
	} else if !ok && !strings.HasPrefix(Version, "edge-") {
		// Built from source: releases are what there is to hear about.
		return api.ChannelStable
	}
	return api.ChannelEdge
}

func (s *Server) updateSettings() updateSettings {
	var u updateSettings
	if v := s.setting(updateSettingsKey); v != "" {
		json.Unmarshal([]byte(v), &u)
	}
	if u.Channel != api.ChannelStable && u.Channel != api.ChannelEdge {
		u.Channel = buildChannel()
	}
	return u
}

func (s *Server) updateFound() updateFound {
	var f updateFound
	if v := s.setting(updateFoundKey); v != "" {
		json.Unmarshal([]byte(v), &f)
	}
	return f
}

// isNewer says whether a published version is newer than the running
// one. The images built from main before versions were numbered called
// themselves "edge-COMMIT": everything numbered is newer than those. A
// build from source is never told to update.
func isNewer(published string) bool {
	if strings.HasPrefix(Version, "edge-") {
		return version.Valid(published)
	}
	return version.Newer(published, Version)
}

// ghRelease is a release as the GitHub API lists it.
type ghRelease struct {
	Tag        string     `json:"tag_name"`
	Name       string     `json:"name"`
	Body       string     `json:"body"`
	URL        string     `json:"html_url"`
	Draft      bool       `json:"draft"`
	Prerelease bool       `json:"prerelease"`
	Published  *time.Time `json:"published_at"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func webURL(s string) string {
	if u, err := url.Parse(s); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		return s
	}
	return ""
}

// serverAsset says whether a release file is the server's installer or
// package for the system this server runs on.
func serverAsset(name string) bool {
	if !strings.HasPrefix(name, "better-backup-server_") {
		return false
	}
	ext := name[strings.LastIndex(name, ".")+1:]
	switch runtime.GOOS {
	case "windows":
		return ext == "msi" && strings.Contains(name, "_windows_"+runtime.GOARCH+".")
	case "darwin":
		return ext == "pkg" && strings.Contains(name, "_darwin_")
	case "linux":
		return (ext == "deb" || ext == "rpm") && strings.Contains(name, "_linux_"+runtime.GOARCH+".")
	}
	return false
}

// releasesOn picks the releases of a channel out of GitHub's list,
// newest first. The stable channel is the releases that are not marked
// as pre-releases. The edge channel is everything: the build from main
// (the release tagged "edge", named after its version), release
// candidates and releases, so that an edge server is also told of a
// release that is newer than the last build from main.
func releasesOn(channel string, list []ghRelease) []api.ServerRelease {
	var out []api.ServerRelease
	for _, r := range list {
		if r.Draft {
			continue
		}
		tag := r.Tag
		if tag == "edge" {
			tag = r.Name
		}
		v, ok := version.Parse(tag)
		if !ok || (channel != api.ChannelEdge && (r.Prerelease || !v.Stable())) {
			continue
		}
		notes := strings.TrimSpace(strings.ReplaceAll(r.Body, "\r\n", "\n"))
		if len(notes) > maxNotes {
			notes = notes[:maxNotes] + "\n\n(The notes go on; the release page has all of them.)"
		}
		rel := api.ServerRelease{Version: v.String(), Notes: notes, URL: webURL(r.URL), Prerelease: !v.Stable()}
		if r.Published != nil {
			rel.Published = *r.Published
		}
		for _, a := range r.Assets {
			if serverAsset(a.Name) && webURL(a.URL) != "" {
				rel.Downloads = append(rel.Downloads, api.ServerDownload{Name: a.Name, URL: a.URL, Size: a.Size})
			}
		}
		out = append(out, rel)
	}
	sort.SliceStable(out, func(i, j int) bool { return version.Newer(out[i].Version, out[j].Version) })
	return out
}

// fetchReleases asks GitHub for the list. The request says nothing
// about this server.
func fetchReleases(ctx context.Context, from string) ([]ghRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "better-backup") // GitHub refuses requests without one
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub answered %s: it takes only so many questions an hour from one address. The server asks again later", resp.Status)
		}
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var list []ghRelease
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("the list of releases could not be read: %w", err)
	}
	return list, nil
}

// checkUpdate asks once, stores what it learned and sends the alert for
// a version not alerted about before.
func (s *Server) checkUpdate(ctx context.Context) updateFound {
	set := s.updateSettings()
	before := s.updateFound()
	found := updateFound{Checked: time.Now().UTC(), Channel: set.Channel, Alerted: before.Alerted}
	list, err := fetchReleases(ctx, s.cfg.UpdateURL)
	if err != nil {
		// Keep what was known: a release does not go away because
		// GitHub could not be reached today.
		found.Error = err.Error()
		if before.Channel == set.Channel {
			found.Releases = before.Releases
		}
		s.log.Warn("could not check for a newer version", "err", err)
	} else {
		found.Releases = releasesOn(set.Channel, list)
		if len(found.Releases) > 20 {
			found.Releases = found.Releases[:20]
		}
	}
	if newer := newerReleases(found.Releases); len(newer) > 0 && newer[0].Version != found.Alerted {
		found.Alerted = newer[0].Version
		s.log.Info("a newer version of better-backup is out", "version", newer[0].Version, "running", Version)
		if !set.NoAlert {
			s.alert("Version "+newer[0].Version+" of better-backup is out", s.updateMail(newer))
		}
	}
	b, _ := json.Marshal(found)
	s.setSetting(updateFoundKey, string(b))
	return found
}

// newerReleases are the releases newer than the running version.
func newerReleases(all []api.ServerRelease) []api.ServerRelease {
	out := []api.ServerRelease{}
	for _, r := range all {
		if isNewer(r.Version) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) updateMail(newer []api.ServerRelease) string {
	var b strings.Builder
	r := newer[0]
	fmt.Fprintf(&b, "The backup server %s runs version %s of better-backup. Version %s is out.\n\n", serverName(), Version, r.Version)
	b.WriteString("How to update: " + updateHow(s.updateSettings().Channel) + "\n\n")
	for i, r := range newer {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "What is new in %s\n\n%s\n\n", r.Version, r.Notes)
	}
	if r.URL != "" {
		b.WriteString(r.URL + "\n\n")
	}
	b.WriteString("The web UI shows the same under Updates, where these messages can also be turned off.\n")
	return b.String()
}

// updateHow says how this server is updated, by how it was installed.
func updateHow(channel string) string {
	tag := "latest"
	if channel == api.ChannelEdge {
		tag = "edge"
	}
	end := " Settings, agents and backups stay as they are, and agents that update by themselves follow the server."
	switch {
	case inContainer():
		return "This server runs from a container image. Pull the newer image and start the container again: with Docker Compose, run \"docker compose pull\" and then \"docker compose up -d\"; on TrueNAS, open Apps, pick the app and choose Update, or edit and save it so that it pulls the image again. The image tag \"" + tag + "\" always points at the newest version of this channel." + end
	case runtime.GOOS == "windows":
		return "Download the server installer for Windows (.msi) and run it on this server. It installs over the version that is there." + end
	case runtime.GOOS == "darwin":
		return "Download the server installer for macOS (.pkg) and open it on this server. It installs over the version that is there." + end
	}
	return "Download the server package and install it over the one that is there: \"sudo apt install ./FILE.deb\" on Debian and Ubuntu, \"sudo dnf install ./FILE.rpm\" on Red Hat, Fedora and SUSE. A server set up from the archive is updated by replacing its programs and starting it again." + end
}

// scheduleUpdateCheck asks when the last answer is old enough. It never
// blocks: the question is asked in the background.
func (s *Server) scheduleUpdateCheck(now time.Time) {
	if s.cfg.UpdateURL == "" || now.UnixNano() < s.updateNext.Load() {
		return
	}
	set := s.updateSettings()
	if set.Off {
		s.updateNext.Store(now.Add(updateEvery).UnixNano())
		return
	}
	f := s.updateFound()
	wait := updateEvery
	if f.Error != "" {
		wait = updateRetry
	}
	if f.Channel == set.Channel && now.Sub(f.Checked) < wait {
		s.updateNext.Store(f.Checked.Add(wait).UnixNano())
		return
	}
	if !s.updateBusy.CompareAndSwap(false, true) {
		return
	}
	s.updateNext.Store(now.Add(updateRetry).UnixNano())
	go func() {
		defer s.updateBusy.Store(false)
		s.checkUpdate(s.ctx)
	}()
}

func (s *Server) serverUpdate() api.ServerUpdate {
	set, f := s.updateSettings(), s.updateFound()
	out := api.ServerUpdate{Current: Version, Channel: set.Channel, Check: !set.Off, Alert: !set.NoAlert, CanCheck: s.cfg.UpdateURL != "",
		Releases: []api.ServerRelease{}, How: updateHow(set.Channel), CurrentNotes: betterbackup.ReleaseNotes(Version), Container: inContainer()}
	if v, ok := version.Parse(Version); ok {
		out.Numbered, out.CurrentStable = true, v.Stable()
	}
	// What was found on another channel says nothing about this one.
	if f.Channel != set.Channel || set.Off {
		return out
	}
	if !f.Checked.IsZero() {
		out.Checked = &f.Checked
	}
	out.Error = f.Error
	out.Releases = newerReleases(f.Releases)
	if len(out.Releases) > 0 {
		out.Latest = out.Releases[0].Version
	}
	return out
}

func (s *Server) handleServerUpdate(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.serverUpdate())
}

func (s *Server) handleSaveServerUpdate(w http.ResponseWriter, r *http.Request) {
	var req api.ServerUpdate
	if !decode(w, r, &req) {
		return
	}
	if req.Channel != api.ChannelStable && req.Channel != api.ChannelEdge {
		fail(w, http.StatusBadRequest, "The channel is \"stable\" or \"edge\".")
		return
	}
	set := updateSettings{Off: !req.Check, Channel: req.Channel, NoAlert: !req.Alert}
	b, _ := json.Marshal(set)
	if err := s.setSetting(updateSettingsKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	note(r, "check: %v, channel: %s, alert: %v", req.Check, req.Channel, req.Alert)
	// A channel without an answer yet is asked about now, so that the
	// page that saved shows it.
	s.updateNext.Store(0)
	if !set.Off && s.cfg.UpdateURL != "" && s.updateFound().Channel != set.Channel && s.updateBusy.CompareAndSwap(false, true) {
		s.checkUpdate(r.Context())
		s.updateBusy.Store(false)
	}
	reply(w, http.StatusOK, s.serverUpdate())
}

// handleCheckServerUpdate asks now and answers with what was found.
func (s *Server) handleCheckServerUpdate(w http.ResponseWriter, r *http.Request) {
	if s.cfg.UpdateURL == "" {
		fail(w, http.StatusConflict, "This server was started without a place to ask for newer versions.")
		return
	}
	if s.updateSettings().Off {
		fail(w, http.StatusConflict, "Checking for newer versions is turned off. Turn it on first.")
		return
	}
	if !s.updateBusy.CompareAndSwap(false, true) {
		fail(w, http.StatusConflict, "The server is asking right now. Try again in a moment.")
		return
	}
	defer s.updateBusy.Store(false)
	s.checkUpdate(r.Context())
	reply(w, http.StatusOK, s.serverUpdate())
}
