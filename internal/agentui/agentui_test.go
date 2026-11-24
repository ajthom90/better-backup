package agentui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/better-backup/internal/agentctl"
)

type fake struct{ enrolled bool }

func (f *fake) Status(context.Context) *agentctl.Status {
	return &agentctl.Status{Version: "test", Enrolled: f.enrolled, Hostname: "box"}
}
func (f *fake) Enroll(_ context.Context, req agentctl.EnrollRequest) error {
	if req.Code != "bb1.good" {
		return errors.New("bad code")
	}
	f.enrolled = true
	return nil
}
func (f *fake) Unenroll(context.Context) error { f.enrolled = false; return nil }
func (f *fake) Backup(context.Context) (*agentctl.BackupResult, error) {
	return &agentctl.BackupResult{Policies: 1, Started: 1}, nil
}

// TestLocal serves the page as "bb-agent setup" does, over a real
// control channel, and checks who is let in.
func TestLocal(t *testing.T) {
	dir, err := os.MkdirTemp("", "bbui") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sock := agentctl.SocketPath(dir)
	ctl := agentctl.Dial(sock)
	served := make(chan error, 1)
	go func() { served <- agentctl.Serve(ctx, sock, &fake{}) }()
	for i := 0; ; i++ {
		if _, err := ctl.Status(ctx); err == nil {
			break
		} else if i > 100 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	addrc := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- Local(ctx, ctl, 0, func(a string) { addrc <- a }) }()
	addr := <-addrc
	base := addr[:strings.Index(addr, "/?")]
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	do := func(c *http.Client, method, url, body string, hdr ...string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			if hdr[i] == "Host" {
				req.Host = hdr[i+1]
			} else {
				req.Header.Set(hdr[i], hdr[i+1])
			}
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Without the key: nothing.
	if code, _ := do(http.DefaultClient, "GET", base+"/", ""); code != http.StatusForbidden {
		t.Fatalf("page without key: %d", code)
	}
	if code, _ := do(http.DefaultClient, "GET", base+"/api/state", ""); code != http.StatusForbidden {
		t.Fatalf("state without key: %d", code)
	}
	if code, _ := do(http.DefaultClient, "GET", base+"/?key=wrong", ""); code != http.StatusForbidden {
		t.Fatalf("page with a wrong key: %d", code)
	}
	// With it: the page, then a cookie.
	if code, body := do(hc, "GET", addr, ""); code != 200 || !strings.Contains(body, "<title>Better Backup</title>") {
		t.Fatalf("page: %d", code)
	}
	if code, body := do(hc, "GET", base+"/api/state", ""); code != 200 || !strings.Contains(body, `"running":true`) || !strings.Contains(body, `"enrolled":false`) {
		t.Fatalf("state: %d %s", code, body)
	}
	// A page elsewhere that knows the port still cannot act: it cannot
	// set the header, and a name it resolves here is not our host.
	if code, _ := do(hc, "POST", base+"/api/enroll", `{"code":"bb1.good"}`); code != http.StatusForbidden {
		t.Fatalf("post without the header: %d", code)
	}
	if code, _ := do(hc, "GET", base+"/api/state", "", "Host", "evil.example"); code != http.StatusForbidden {
		t.Fatalf("another host name: %d", code)
	}
	if code, body := do(hc, "POST", base+"/api/enroll", `{"code":"nope"}`, "X-Better-Backup", "1"); code != 400 || !strings.Contains(body, "bad code") {
		t.Fatalf("bad code: %d %s", code, body)
	}
	if code, body := do(hc, "POST", base+"/api/enroll", `{"code":"bb1.good"}`, "X-Better-Backup", "1"); code != 200 || !strings.Contains(body, `"enrolled":true`) {
		t.Fatalf("enroll: %d %s", code, body)
	}
	if code, body := do(hc, "POST", base+"/api/backup", "", "X-Better-Backup", "1"); code != 200 || !strings.Contains(body, `"started":1`) {
		t.Fatalf("backup: %d %s", code, body)
	}
	if code, _ := do(hc, "POST", base+"/api/open", `{"what":"https://evil.example"}`, "X-Better-Backup", "1"); code != 400 {
		t.Fatalf("open of something unnamed: %d", code)
	}
	if code, body := do(hc, "POST", base+"/api/unenroll", "", "X-Better-Backup", "1"); code != 200 || !strings.Contains(body, `"enrolled":false`) {
		t.Fatalf("unenroll: %d %s", code, body)
	}

	// The agent goes away: the page is told, not failed.
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go Local(ctx2, ctl, 0, func(a string) { addrc <- a })
	addr = <-addrc
	jar2, _ := cookiejar.New(nil)
	hc2 := &http.Client{Jar: jar2}
	do(hc2, "GET", addr, "")
	if code, body := do(hc2, "GET", addr[:strings.Index(addr, "/?")]+"/api/state", ""); code != 200 || !strings.Contains(body, `"running":false`) {
		t.Fatalf("state without an agent: %d %s", code, body)
	}
}
