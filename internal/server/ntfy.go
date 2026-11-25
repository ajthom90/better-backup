package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajthom90/better-backup/internal/api"
)

const ntfyPublic = "https://ntfy.sh"

// ntfyDefaults is how each level shows up unless the settings say
// otherwise. Priority 5 breaks through "do not disturb" on a phone.
var ntfyDefaults = map[string]api.NtfyLevel{
	api.AlertSecurity: {Priority: 5, Tags: "rotating_light"},
	api.AlertFailed:   {Priority: 4, Tags: "x"},
	api.AlertWarning:  {Priority: 3, Tags: "warning"},
	api.AlertInfo:     {Priority: 2, Tags: "bar_chart"},
}

var (
	ntfyTopic = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
	ntfyTag   = regexp.MustCompile(`^[^,\s][^,]*$`)
)

func ntfyOn(a api.AlertSettings) bool {
	return a.Ntfy != nil && a.Ntfy.Enabled && a.Ntfy.Topic != ""
}

func ntfyServer(n api.NtfySettings) string {
	if n.Server == "" {
		return ntfyPublic
	}
	return n.Server
}

// checkNtfy tidies the settings, fills in the defaults and rejects what
// cannot work.
func checkNtfy(n *api.NtfySettings) error {
	if n == nil {
		return nil
	}
	n.Server = strings.TrimRight(strings.TrimSpace(n.Server), "/")
	n.Topic, n.Token, n.Username = strings.TrimSpace(n.Topic), strings.TrimSpace(n.Token), strings.TrimSpace(n.Username)
	if n.Server != "" {
		u, err := url.Parse(n.Server)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.User != nil {
			return errors.New("the ntfy server must be an address that starts with https:// or http://, such as https://ntfy.sh")
		}
	}
	if n.Topic != "" && !ntfyTopic.MatchString(n.Topic) {
		return errors.New("an ntfy topic is up to 64 letters, digits, hyphens and underscores")
	}
	if n.Enabled && n.Topic == "" {
		return errors.New("ntfy needs a topic to publish to")
	}
	if n.Token != "" && n.Username != "" {
		return errors.New("sign in to ntfy with an access token or with a user name and password, not both")
	}
	if n.Username == "" {
		n.Password = ""
	} else if strings.Contains(n.Username, ":") {
		return errors.New("an ntfy user name cannot contain a colon")
	}
	levels := map[string]api.NtfyLevel{}
	for _, name := range api.AlertLevels {
		l, ok := n.Levels[name]
		if !ok {
			l = ntfyDefaults[name]
		}
		if l.Priority == 0 {
			l.Priority = 3
		}
		if l.Priority < 1 || l.Priority > 5 {
			return errors.New("an ntfy priority is a number from 1 to 5")
		}
		var tags []string
		for _, t := range strings.Split(l.Tags, ",") {
			if t = strings.TrimSpace(t); t == "" {
				continue
			}
			if !ntfyTag.MatchString(t) || len(t) > 60 {
				return fmt.Errorf("%q cannot be an ntfy tag", t)
			}
			tags = append(tags, t)
		}
		if len(tags) > 8 {
			return errors.New("give at most 8 ntfy tags for a level")
		}
		l.Tags = strings.Join(tags, ",")
		levels[name] = l
	}
	for name := range n.Levels {
		if _, ok := levels[name]; !ok {
			return errors.New(`the ntfy levels are "security", "failed", "warning" and "info"`)
		}
	}
	n.Levels = levels
	return nil
}

// cut shortens s to at most max bytes without splitting a character.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// postNtfy publishes one alert. It uses ntfy's JSON form, which carries
// titles in any language and works behind a reverse proxy that would
// strip unknown headers.
func postNtfy(ctx context.Context, n api.NtfySettings, level, subject, body string) error {
	l, ok := n.Levels[level]
	if !ok {
		if l, ok = ntfyDefaults[level]; !ok {
			l = ntfyDefaults[api.AlertWarning]
		}
	}
	msg := map[string]any{
		"topic": n.Topic,
		// A phone shows little of a title; the topic already says who
		// is talking.
		"title": cut(strings.TrimPrefix(oneLine(subject), "[better-backup] "), 250),
		// ntfy turns a message over 4096 bytes into an attachment, or
		// refuses it.
		"message":  cut(strings.TrimSpace(body), 3800),
		"priority": l.Priority,
	}
	if l.Tags != "" {
		msg["tags"] = strings.Split(l.Tags, ",")
	}
	b, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", ntfyServer(n)+"/", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "better-backup")
	switch {
	case n.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.Token)
	case n.Username != "":
		req.SetBasicAuth(n.Username, n.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
	var e struct {
		Error string `json:"error"`
	}
	said := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		said = e.Error
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("the ntfy server did not accept the sign-in (%s)", said)
	case http.StatusForbidden:
		if n.Token == "" && n.Username == "" {
			return fmt.Errorf("the ntfy server does not let just anyone publish to this topic; give an access token or a user name and password (%s)", said)
		}
		return fmt.Errorf("this ntfy account may not publish to the topic %q (%s)", n.Topic, said)
	}
	return fmt.Errorf("the ntfy server answered %s: %s", resp.Status, said)
}

// handleTestNtfy publishes a test notification now, looking the way a
// failed backup's would, and reports what the ntfy server said.
func (s *Server) handleTestNtfy(w http.ResponseWriter, r *http.Request) {
	a := s.alertSettings()
	if a.Ntfy == nil || a.Ntfy.Topic == "" {
		fail(w, http.StatusBadRequest, "save an ntfy topic first")
		return
	}
	level := r.URL.Query().Get("level")
	if _, ok := ntfyDefaults[level]; !ok {
		level = api.AlertFailed
	}
	err := postNtfy(r.Context(), *a.Ntfy, level, "[better-backup] Test message from "+serverName(),
		"This is a test message from the better-backup server on "+serverName()+". Notifications through ntfy are working. A failed backup arrives with this priority and these tags.")
	if err != nil {
		fail(w, http.StatusBadGateway, "the notification was not delivered: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
