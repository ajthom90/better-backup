package agentui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ajthom90/better-backup/internal/agentctl"
)

// Local serves the page to a browser on this machine: on a loopback
// port the system picks, behind a key that is part of the address. It
// calls ready with that address, then serves until ctx ends, or, when
// idle is set, until no page has asked anything for that long (an open
// page asks every few seconds).
func Local(ctx context.Context, ctl *agentctl.Client, idle time.Duration, ready func(addr string)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	b := make([]byte, 16)
	rand.Read(b)
	key := hex.EncodeToString(b)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	page := Handler(Options{Control: ctl, Key: key, Host: ln.Addr().String()})
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(time.Now().UnixNano())
		page.ServeHTTP(w, r)
	})}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				srv.Close()
				return
			case <-t.C:
				if idle > 0 && time.Since(time.Unix(0, last.Load())) > idle {
					srv.Close()
					return
				}
			}
		}
	}()
	ready("http://" + ln.Addr().String() + "/?key=" + key)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
