package m365

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/ajthom90/better-backup/internal/backup"
)

// Spec says what to back up from a tenant.
type Spec struct {
	// Users are sign-in names (alice@example.com). "*" stands for
	// every licensed account that is not a guest.
	Users    []string
	Mail     bool
	OneDrive bool
}

// Source returns the tree a backup stores: a folder per user, holding
// "Mail" (a folder per mail folder, a .eml file per message) and
// "OneDrive" (the files as they are). It signs in and finds the users
// before it returns; everything below them is read as the backup walks.
// warn is told about users that are skipped.
func (c *Client) Source(ctx context.Context, spec Spec, warn func(string)) (*backup.VNode, error) {
	if !spec.Mail && !spec.OneDrive {
		return nil, fmt.Errorf("Microsoft 365: neither mail nor OneDrive is selected")
	}
	type picked struct {
		User
		named bool
	}
	var users []picked
	have := map[string]bool{}
	add := func(u User, named bool) {
		if k := strings.ToLower(u.UPN); k != "" && !have[k] {
			have[k] = true
			users = append(users, picked{u, named})
		}
	}
	all := false
	for _, name := range spec.Users {
		name = strings.TrimSpace(name)
		switch {
		case name == "":
		case name == "*":
			all = true
		default:
			u, err := c.User(ctx, name)
			if NotFound(err) {
				warn("Microsoft 365: there is no user " + name + " in this tenant")
				continue
			}
			if err != nil {
				return nil, err
			}
			add(*u, true)
		}
	}
	if all {
		us, err := c.Users(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range us {
			if u.Licensed && !u.Guest {
				add(u, false)
			}
		}
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("Microsoft 365: none of the chosen users were found")
	}
	root := &backup.VNode{Dir: true, List: func(ctx context.Context) ([]*backup.VNode, error) {
		var out []*backup.VNode
		for _, u := range users {
			out = append(out, &backup.VNode{Name: safeName(strings.ToLower(u.UPN)), Dir: true, List: func(ctx context.Context) ([]*backup.VNode, error) {
				return c.userNodes(ctx, u.User, u.named, spec, warn)
			}})
		}
		return out, nil
	}}
	return root, nil
}

// userNodes looks whether the user has a mailbox and a OneDrive at all.
// A user without one simply has no such folder; only a user who was
// named in the policy gets a warning about it.
func (c *Client) userNodes(ctx context.Context, u User, named bool, spec Spec, warn func(string)) ([]*backup.VNode, error) {
	var out []*backup.VNode
	base := "/users/" + url.PathEscape(u.ID)
	if spec.Mail {
		top, err := c.mailFolders(ctx, base+"/mailFolders")
		switch {
		case NotFound(err):
			if named {
				warn("Microsoft 365: " + u.UPN + " has no mailbox")
			}
		default:
			// A failure is reported by the listing of the folder, so
			// that the last backup's mail is kept.
			out = append(out, &backup.VNode{Name: "Mail", Dir: true, List: func(ctx context.Context) ([]*backup.VNode, error) {
				if err != nil {
					return nil, err
				}
				return c.folderNodes(base, top), nil
			}})
		}
	}
	if spec.OneDrive {
		var d struct {
			ID string `json:"id"`
		}
		err := hint(c.getJSON(ctx, base+"/drive?$select=id", &d), "Files.Read.All")
		switch {
		case NotFound(err):
			if named {
				warn("Microsoft 365: " + u.UPN + " has no OneDrive")
			}
		default:
			out = append(out, &backup.VNode{Name: "OneDrive", Dir: true, List: func(ctx context.Context) ([]*backup.VNode, error) {
				if err != nil {
					return nil, err
				}
				return c.driveNodes(ctx, base, "/drive/root/children")
			}})
		}
	}
	return out, nil
}

type mailFolder struct {
	ID       string `json:"id"`
	Name     string `json:"displayName"`
	Children int    `json:"childFolderCount"`
}

func (c *Client) mailFolders(ctx context.Context, path string) ([]mailFolder, error) {
	fs, err := list[mailFolder](ctx, c, path+"?$select=id,displayName,childFolderCount&$top=100")
	return fs, hint(err, "Mail.Read")
}

type message struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	Received string `json:"receivedDateTime"`
	Sent     string `json:"sentDateTime"`
	Created  string `json:"createdDateTime"`
	Change   string `json:"changeKey"`
	Draft    bool   `json:"isDraft"`
}

func (c *Client) folderNodes(base string, folders []mailFolder) []*backup.VNode {
	var out []*backup.VNode
	names := map[string]int{}
	for _, f := range folders {
		name := safeName(f.Name)
		// Two folders may differ only in letters a file name cannot hold.
		if names[strings.ToLower(name)]++; names[strings.ToLower(name)] > 1 {
			name += " [" + short(f.ID) + "]"
		}
		out = append(out, &backup.VNode{Name: name, Dir: true, List: func(ctx context.Context) ([]*backup.VNode, error) {
			var kids []*backup.VNode
			if f.Children > 0 {
				sub, err := c.mailFolders(ctx, base+"/mailFolders/"+url.PathEscape(f.ID)+"/childFolders")
				if err != nil {
					return nil, err
				}
				kids = c.folderNodes(base, sub)
			}
			msgs, err := list[message](ctx, c, base+"/mailFolders/"+url.PathEscape(f.ID)+
				"/messages?$select=id,subject,receivedDateTime,sentDateTime,createdDateTime,changeKey,isDraft&$top=500")
			if err != nil {
				return nil, hint(err, "Mail.Read")
			}
			for _, m := range msgs {
				kids = append(kids, c.messageNode(base, m))
			}
			return kids, nil
		}})
	}
	return out
}

// messageNode is a message as a file: "2026-11-24 0930 Subject
// [1a2b3c4d].eml". The letters at the end come from the message's id
// and keep two messages with one subject apart.
func (c *Client) messageNode(base string, m message) *backup.VNode {
	var when time.Time
	for _, s := range []string{m.Received, m.Sent, m.Created} {
		if t, err := time.Parse(time.RFC3339, s); err == nil && t.Year() > 1970 {
			when = t.UTC()
			break
		}
	}
	subject := safeName(m.Subject)
	if r := []rune(subject); len(r) > 80 {
		subject = strings.TrimRight(string(r[:80]), " .")
	}
	if m.Subject == "" {
		subject = "(no subject)"
	}
	name := subject + " [" + short(m.ID) + "].eml"
	if !when.IsZero() {
		name = when.Format("2006-01-02 1504 ") + name
	}
	// A message that was sent or received does not change, apart from
	// marks such as read and flagged, which the file does not hold. A
	// draft does.
	key := m.ID
	if m.Draft {
		key += " " + m.Change
	}
	return &backup.VNode{Name: name, ModTime: when, Key: key, Open: func(ctx context.Context) (io.ReadCloser, error) {
		resp, err := c.get(ctx, base+"/messages/"+url.PathEscape(m.ID)+"/$value")
		if err != nil {
			return nil, hint(err, "Mail.Read")
		}
		return resp.Body, nil
	}}
}

type driveItem struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	CTag     string    `json:"cTag"`
	ETag     string    `json:"eTag"`
	Modified string    `json:"lastModifiedDateTime"`
	File     *struct{} `json:"file"`
	Folder   *struct{} `json:"folder"`
	Package  *struct{} `json:"package"`
	FS       struct {
		Modified string `json:"lastModifiedDateTime"`
	} `json:"fileSystemInfo"`
}

func (c *Client) driveNodes(ctx context.Context, base, path string) ([]*backup.VNode, error) {
	items, err := list[driveItem](ctx, c, base+path+"?$select=id,name,cTag,eTag,lastModifiedDateTime,file,folder,package,fileSystemInfo&$top=200")
	if err != nil {
		return nil, hint(err, "Files.Read.All")
	}
	var out []*backup.VNode
	for _, it := range items {
		n := &backup.VNode{Name: safeName(it.Name)}
		for _, s := range []string{it.FS.Modified, it.Modified} {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				n.ModTime = t.UTC()
				break
			}
		}
		at := "/drive/items/" + url.PathEscape(it.ID)
		switch {
		case it.Folder != nil || it.Package != nil: // a package is a OneNote notebook: a folder of its sections
			n.Dir = true
			n.List = func(ctx context.Context) ([]*backup.VNode, error) { return c.driveNodes(ctx, base, at+"/children") }
		case it.File != nil:
			if n.Key = it.CTag; n.Key == "" {
				n.Key = it.ETag
			}
			n.Open = func(ctx context.Context) (io.ReadCloser, error) {
				resp, err := c.get(ctx, base+at+"/content")
				if err != nil {
					return nil, hint(err, "Files.Read.All")
				}
				return resp.Body, nil
			}
		default:
			continue // a shortcut to someone else's folder
		}
		out = append(out, n)
	}
	return out, nil
}

func short(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:4])
}

// safeName turns a name into one every file system takes.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case strings.ContainsRune(`<>:"/\|?*`, r), unicode.IsControl(r):
			return '_'
		}
		return r
	}, s)
	s = strings.TrimRight(strings.TrimSpace(s), " .")
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}
