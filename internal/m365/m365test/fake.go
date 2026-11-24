// Package m365test is a stand-in for Microsoft's sign-in service and
// Microsoft Graph, holding a small tenant in memory. It answers what
// internal/m365 asks, in the shapes Microsoft uses, including pages,
// throttling and redirects to file content.
package m365test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Message struct {
	ID       string
	Subject  string
	Received time.Time
	Draft    bool
	Change   string
	MIME     string
}

type Folder struct {
	ID       string
	Name     string
	Folders  []*Folder
	Messages []*Message
}

// Item is a file or, with Folder set, a folder of a OneDrive.
type Item struct {
	ID       string
	Name     string
	Folder   bool
	Children []*Item
	Data     string
	CTag     string
	Modified time.Time
}

type User struct {
	ID       string
	UPN      string
	Name     string
	Licensed bool
	Guest    bool
	// Mail is nil for a user without a mailbox, Drive for one without
	// a OneDrive.
	Mail  []*Folder
	Drive *Item
}

// Tenant is the fake service. Change its fields only while holding Lock.
type Tenant struct {
	sync.Mutex
	TenantID, ClientID, Secret string
	Users                      []*User
	// PageSize is how many things one answer lists.
	PageSize int
	// Throttle makes the next that many Graph requests fail with 429.
	Throttle int
	// Deny lists permissions the app lacks: "mail", "files", "users".
	Deny map[string]bool
	// Broken lists ids of messages and files whose content fails.
	Broken map[string]bool
	// Downloads counts content requests by message or item id.
	Downloads map[string]int
	Requests  int

	URL string
	srv *httptest.Server
}

// New starts a tenant with a few users.
func New() *Tenant {
	t := &Tenant{TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Secret: "s3cret~value",
		PageSize: 2, Deny: map[string]bool{}, Broken: map[string]bool{}, Downloads: map[string]int{}}
	day := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	t.Users = []*User{
		{ID: "u-alice", UPN: "Alice@example.com", Name: "Alice Adams", Licensed: true,
			Mail: []*Folder{
				{ID: "f-inbox", Name: "Inbox", Messages: []*Message{
					{ID: "m1", Subject: "Quarterly numbers", Received: day, MIME: mime("Quarterly numbers", "The numbers are in.")},
					{ID: "m2", Subject: `Re: plans / "next" year?`, Received: day.Add(time.Hour), MIME: mime("Re: plans", "Let us talk on Monday.")},
					{ID: "m3", Subject: "", Received: day.Add(2 * time.Hour), MIME: mime("", "No subject here.")},
				}, Folders: []*Folder{
					{ID: "f-proj", Name: "Projects", Messages: []*Message{
						{ID: "m4", Subject: "Kickoff", Received: day.Add(3 * time.Hour), MIME: mime("Kickoff", "We start on the first.")},
					}},
				}},
				{ID: "f-drafts", Name: "Drafts", Messages: []*Message{
					{ID: "m5", Subject: "Unfinished", Received: day, Draft: true, Change: "c1", MIME: mime("Unfinished", "First words")},
				}},
				{ID: "f-sent", Name: "Sent Items"},
			},
			Drive: &Item{ID: "d-root", Folder: true, Children: []*Item{
				{ID: "i1", Name: "Budget.xlsx", Data: strings.Repeat("budget ", 500), CTag: "c:{i1},1", Modified: day},
				{ID: "i2", Name: "Notes.txt", Data: "remember the milk", CTag: "c:{i2},1", Modified: day},
				{ID: "i3", Name: "Reports", Folder: true, Children: []*Item{
					{ID: "i4", Name: "2026.docx", Data: strings.Repeat("report ", 2000), CTag: "c:{i4},1", Modified: day},
					{ID: "i5", Name: "Old", Folder: true},
				}},
			}},
		},
		{ID: "u-bob", UPN: "bob@example.com", Name: "Bob Brown", Licensed: true,
			Mail: []*Folder{{ID: "f-binbox", Name: "Inbox", Messages: []*Message{
				{ID: "m9", Subject: "Hello Bob", Received: day, MIME: mime("Hello Bob", "Welcome.")},
			}}}},
		{ID: "u-room", UPN: "room1@example.com", Name: "Meeting room 1"},
		{ID: "u-guest", UPN: "carol_partner.com#EXT#@example.com", Name: "Carol (guest)", Licensed: true, Guest: true},
	}
	t.srv = httptest.NewServer(http.HandlerFunc(t.serve))
	t.URL = t.srv.URL
	return t
}

func (t *Tenant) Close() { t.srv.Close() }

func mime(subject, body string) string {
	return "From: sender@example.com\r\nTo: someone@example.com\r\nSubject: " + subject + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\n" + body + "\r\n"
}

func (t *Tenant) fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// page answers with one page of all, and a link to the next.
func (t *Tenant) page(w http.ResponseWriter, r *http.Request, all []any) {
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	skip = min(skip, len(all))
	end := min(skip+t.PageSize, len(all))
	out := map[string]any{"value": all[skip:end]}
	if all[skip:end] == nil {
		out["value"] = []any{}
	}
	if end < len(all) {
		q := r.URL.Query()
		q.Set("$skiptoken", strconv.Itoa(end))
		out["@odata.nextLink"] = t.URL + r.URL.Path + "?" + q.Encode()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (t *Tenant) serve(w http.ResponseWriter, r *http.Request) {
	t.Lock()
	defer t.Unlock()
	path := r.URL.Path
	if path == "/"+t.TenantID+"/oauth2/v2.0/token" && r.Method == "POST" {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Form.Get("client_id") != t.ClientID:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized_client", "error_description": "AADSTS700016: Application with identifier '" + r.Form.Get("client_id") + "' was not found in the directory.\r\nTrace ID: 0000"})
		case r.Form.Get("client_secret") != t.Secret:
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: 0000"})
		case r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != t.URL+"/.default":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "error_description": "AADSTS900144: bad grant or scope"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "token-for-" + t.ClientID})
		}
		return
	}
	if strings.HasSuffix(path, "/oauth2/v2.0/token") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "error_description": "AADSTS90002: Tenant not found.\r\nTrace ID: 0000"})
		return
	}
	// File content is served from another address, without the token.
	if id, ok := strings.CutPrefix(path, "/download/"); ok {
		if it := t.item(id); it != nil {
			w.Write([]byte(it.Data))
			return
		}
		http.NotFound(w, r)
		return
	}
	rest, ok := strings.CutPrefix(path, "/v1.0")
	if !ok {
		http.NotFound(w, r)
		return
	}
	t.Requests++
	if r.Header.Get("Authorization") != "Bearer token-for-"+t.ClientID {
		t.fail(w, http.StatusUnauthorized, "InvalidAuthenticationToken", "Access token is empty.")
		return
	}
	if t.Throttle > 0 {
		t.Throttle--
		w.Header().Set("Retry-After", "0")
		t.fail(w, http.StatusTooManyRequests, "TooManyRequests", "Please retry later.")
		return
	}
	seg := strings.Split(strings.Trim(rest, "/"), "/")
	for i := range seg {
		seg[i], _ = url.PathUnescape(seg[i])
	}
	if seg[0] != "users" {
		t.fail(w, http.StatusBadRequest, "BadRequest", "Resource not found for the segment '"+seg[0]+"'.")
		return
	}
	if t.Deny["users"] {
		t.fail(w, http.StatusForbidden, "Authorization_RequestDenied", "Insufficient privileges to complete the operation.")
		return
	}
	juser := func(u *User) map[string]any {
		lic := []any{}
		if u.Licensed {
			lic = append(lic, map[string]string{"skuId": "sku"})
		}
		typ := "Member"
		if u.Guest {
			typ = "Guest"
		}
		return map[string]any{"id": u.ID, "userPrincipalName": u.UPN, "displayName": u.Name, "userType": typ, "assignedLicenses": lic}
	}
	if len(seg) == 1 {
		var all []any
		for _, u := range t.Users {
			all = append(all, juser(u))
		}
		t.page(w, r, all)
		return
	}
	var u *User
	for _, x := range t.Users {
		if x.ID == seg[1] || strings.EqualFold(x.UPN, seg[1]) {
			u = x
		}
	}
	if u == nil {
		t.fail(w, http.StatusNotFound, "Request_ResourceNotFound", "Resource '"+seg[1]+"' does not exist.")
		return
	}
	seg = seg[2:]
	switch {
	case len(seg) == 0:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(juser(u))
	case seg[0] == "mailFolders" || seg[0] == "messages":
		if t.Deny["mail"] {
			t.fail(w, http.StatusForbidden, "ErrorAccessDenied", "Access is denied. Check credentials and try again.")
			return
		}
		if u.Mail == nil {
			t.fail(w, http.StatusNotFound, "MailboxNotEnabledForRESTAPI", "The mailbox is either inactive, soft-deleted, or is hosted on-premise.")
			return
		}
		t.mail(w, r, u, seg)
	case seg[0] == "drive":
		if t.Deny["files"] {
			t.fail(w, http.StatusForbidden, "accessDenied", "Either scp or roles claim need to be present in the token.")
			return
		}
		if u.Drive == nil {
			t.fail(w, http.StatusNotFound, "ResourceNotFound", "User's mysite not found.")
			return
		}
		t.drive(w, r, u, seg[1:])
	default:
		t.fail(w, http.StatusBadRequest, "BadRequest", "Unsupported segment "+seg[0])
	}
}

func findFolder(fs []*Folder, id string) *Folder {
	for _, f := range fs {
		if f.ID == id {
			return f
		}
		if g := findFolder(f.Folders, id); g != nil {
			return g
		}
	}
	return nil
}

func eachMessage(fs []*Folder, fn func(*Message)) {
	for _, f := range fs {
		for _, m := range f.Messages {
			fn(m)
		}
		eachMessage(f.Folders, fn)
	}
}

func (t *Tenant) mail(w http.ResponseWriter, r *http.Request, u *User, seg []string) {
	jfolders := func(fs []*Folder) []any {
		var all []any
		for _, f := range fs {
			all = append(all, map[string]any{"id": f.ID, "displayName": f.Name, "childFolderCount": len(f.Folders)})
		}
		return all
	}
	if seg[0] == "messages" && len(seg) == 3 && seg[2] == "$value" {
		var m *Message
		eachMessage(u.Mail, func(x *Message) {
			if x.ID == seg[1] {
				m = x
			}
		})
		if m == nil {
			t.fail(w, http.StatusNotFound, "ErrorItemNotFound", "The specified object was not found in the store.")
			return
		}
		t.Downloads[m.ID]++
		if t.Broken[m.ID] {
			t.fail(w, http.StatusInternalServerError, "ErrorInternalServerError", "An internal server error occurred.")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(m.MIME))
		return
	}
	if seg[0] != "mailFolders" {
		t.fail(w, http.StatusBadRequest, "BadRequest", "Unsupported mail request")
		return
	}
	if len(seg) == 1 {
		t.page(w, r, jfolders(u.Mail))
		return
	}
	f := findFolder(u.Mail, seg[1])
	if f == nil || len(seg) != 3 {
		t.fail(w, http.StatusNotFound, "ErrorItemNotFound", "The specified folder could not be found in the store.")
		return
	}
	switch seg[2] {
	case "childFolders":
		t.page(w, r, jfolders(f.Folders))
	case "messages":
		var all []any
		for _, m := range f.Messages {
			all = append(all, map[string]any{"id": m.ID, "subject": m.Subject, "receivedDateTime": m.Received.Format(time.RFC3339),
				"sentDateTime": m.Received.Format(time.RFC3339), "createdDateTime": m.Received.Format(time.RFC3339), "changeKey": m.Change, "isDraft": m.Draft})
		}
		t.page(w, r, all)
	default:
		t.fail(w, http.StatusBadRequest, "BadRequest", "Unsupported mail request")
	}
}

func find(it *Item, id string) *Item {
	if it == nil || it.ID == id {
		return it
	}
	for _, c := range it.Children {
		if f := find(c, id); f != nil {
			return f
		}
	}
	return nil
}

func (t *Tenant) item(id string) *Item {
	for _, u := range t.Users {
		if it := find(u.Drive, id); it != nil {
			return it
		}
	}
	return nil
}

func (t *Tenant) drive(w http.ResponseWriter, r *http.Request, u *User, seg []string) {
	children := func(it *Item) {
		var all []any
		for _, c := range it.Children {
			j := map[string]any{"id": c.ID, "name": c.Name, "cTag": c.CTag, "eTag": c.CTag, "lastModifiedDateTime": c.Modified.Format(time.RFC3339),
				"fileSystemInfo": map[string]string{"lastModifiedDateTime": c.Modified.Format(time.RFC3339)}}
			if c.Folder {
				j["folder"] = map[string]int{"childCount": len(c.Children)}
			} else {
				j["file"] = map[string]string{"mimeType": "application/octet-stream"}
				j["size"] = len(c.Data)
			}
			all = append(all, j)
		}
		t.page(w, r, all)
	}
	switch {
	case len(seg) == 0:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": "drive-" + u.ID})
	case len(seg) == 2 && seg[0] == "root" && seg[1] == "children":
		children(u.Drive)
	case len(seg) == 3 && seg[0] == "items":
		it := find(u.Drive, seg[1])
		if it == nil {
			t.fail(w, http.StatusNotFound, "itemNotFound", "The resource could not be found.")
			return
		}
		switch seg[2] {
		case "children":
			children(it)
		case "content":
			t.Downloads[it.ID]++
			if t.Broken[it.ID] {
				t.fail(w, http.StatusInternalServerError, "generalException", "An unexpected error occurred.")
				return
			}
			http.Redirect(w, r, t.URL+"/download/"+it.ID, http.StatusFound)
		default:
			t.fail(w, http.StatusBadRequest, "BadRequest", "Unsupported drive request")
		}
	default:
		t.fail(w, http.StatusBadRequest, "BadRequest", fmt.Sprint("Unsupported drive request ", seg))
	}
}
