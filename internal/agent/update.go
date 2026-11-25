package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/api"
)

// The agent keeps itself up to date. It asks the server now and then,
// and each time it connects, whether the server holds a newer installer
// for this machine. It fetches the installer over the connection it
// already trusts, checks it against the SHA-256 the server gave (and,
// where this program is signed, against its signer), waits until no job
// runs, and hands over to the updater.
//
// The updater is a copy of this program that runs on its own, because
// the installer stops the agent. It runs the installer and waits for the
// new version to say it reached the server. If the installer fails, or
// the new version does not come back, the updater puts this program back
// where it was, starts it, and leaves word of what went wrong.

// Timing, as variables so that tests need not wait.
var (
	updateFirstCheck = 20 * time.Second // after the agent starts
	updateEvery      = 30 * time.Minute // between checks
	updateSettle     = 5 * time.Second  // between looks at an update in progress
	// updateGiveUp is how long an update may be "installing" before the
	// agent that finds it calls it failed: the updater is gone.
	updateGiveUp = 20 * time.Minute
	// updateHealthWait is how long the updater waits for the new version
	// to reach the server before it puts the old one back.
	updateHealthWait = 5 * time.Minute
	// updateInstallLimit is how long the installer may take.
	updateInstallLimit = 15 * time.Minute
)

// Seams for tests: what kind of install this is, and how the updater is
// started.
var (
	installKindOf = installKind
	startUpdater  = launchUpdater
	checkSigner   = verifySigner
)

const (
	updateFolder = "update"
	planFile     = "update.json"

	planInstalling = "installing"
	planOK         = "ok"
	planFailed     = "failed"
)

// updatePlan is the note the agent and its updater share about one
// update, in update/update.json of the agent's state directory.
type updatePlan struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Install   string    `json:"install"`
	Installer string    `json:"installer"` // the installer file
	Exe       string    `json:"exe"`       // the installed program
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started"`
	// Reported: the server has been told how it ended.
	Reported bool `json:"reported,omitempty"`
}

func planPath(dir string) string { return filepath.Join(dir, updateFolder, planFile) }

func loadPlan(dir string) *updatePlan {
	b, err := os.ReadFile(planPath(dir))
	if err != nil {
		return nil
	}
	var p updatePlan
	if json.Unmarshal(b, &p) != nil || p.To == "" {
		return nil
	}
	return &p
}

func savePlan(dir string, p *updatePlan) error {
	b, _ := json.MarshalIndent(p, "", "  ")
	tmp := planPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, planPath(dir))
}

func (a *Agent) nudgeUpdate() {
	select {
	case a.updateNow <- struct{}{}:
	default:
	}
}

// keepUpdated runs until ctx is cancelled.
func (a *Agent) keepUpdated(ctx context.Context) {
	kind := installKindOf()
	delay := updateFirstCheck
	// The version an update was for is waiting to hear that it works.
	if p := loadPlan(a.dir); p != nil && p.State == planInstalling {
		delay = 0
	}
	for {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-a.updateNow:
			t.Stop()
		case <-t.C:
		}
		delay = a.updateOnce(ctx, kind)
	}
}

// updateOnce settles an earlier update and looks for a new one. It
// returns how long to wait before looking again.
func (a *Agent) updateOnce(ctx context.Context, kind string) time.Duration {
	// Spread the agents out a little.
	usual := updateEvery + time.Duration(rand.Int64N(int64(updateEvery/6)+1))
	if soon, settled := a.settleUpdate(ctx); !settled {
		return soon
	}
	q := url.Values{"version": {Version}, "install": {kind}, "os": {runtime.GOOS}, "arch": {runtime.GOARCH}}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	data, code, err := a.c.pollOnce(rctx, "/agent/v1/update?"+q.Encode())
	cancel()
	if err != nil {
		if code == http.StatusNotFound {
			return 6 * time.Hour // an older server, which has no updates
		}
		return time.Minute // the run loop says when the server is back
	}
	if code != http.StatusOK {
		return usual
	}
	var offer api.AgentUpdate
	if json.Unmarshal(data, &offer) != nil || offer.Version == "" || offer.Version == Version {
		return usual
	}
	if err := a.installUpdate(ctx, kind, &offer); err != nil {
		if ctx.Err() != nil {
			return usual
		}
		var busy errBusy
		if errors.As(err, &busy) {
			return time.Minute
		}
		a.log.Error("could not update the agent", "to", offer.Version, "err", err)
		a.reportUpdate(ctx, api.AgentUpdateReport{State: api.UpdateFailed, From: Version, Target: offer.Version, Error: err.Error()})
	}
	return usual
}

type errBusy struct{ error }

// settleUpdate deals with the update the plan file tells of. settled is
// false while that update is still under way.
func (a *Agent) settleUpdate(ctx context.Context) (soon time.Duration, settled bool) {
	p := loadPlan(a.dir)
	if p == nil {
		return 0, true
	}
	switch {
	case p.State == planOK:
		// Long done, and the updater has seen it: nothing of it is
		// needed any more.
		os.RemoveAll(filepath.Join(a.dir, updateFolder))
	case p.State == planInstalling && p.To == Version:
		// This is the new version, and the server can hear it: say so,
		// to the server and to the updater that is waiting.
		if err := a.reportUpdate(ctx, api.AgentUpdateReport{State: api.UpdateDone, From: p.From, Target: p.To}); err != nil {
			return updateSettle, false
		}
		p.State, p.Reported = planOK, true
		savePlan(a.dir, p)
		a.log.Info("the agent was updated", "from", p.From, "to", p.To)
	case p.State == planInstalling:
		// The earlier version is running while its updater is at work
		// or has vanished.
		if time.Since(p.Started) < updateGiveUp {
			return updateSettle, false
		}
		p.State, p.Error = planFailed, "the updater did not finish; the agent runs the version it had"
		savePlan(a.dir, p)
		fallthrough
	case p.State == planFailed && !p.Reported:
		a.log.Error("the update of the agent failed", "to", p.To, "err", p.Error)
		if err := a.reportUpdate(ctx, api.AgentUpdateReport{State: api.UpdateFailed, From: p.From, Target: p.To, Error: p.Error}); err != nil {
			return time.Minute, false
		}
		p.Reported = true
		savePlan(a.dir, p)
	}
	return 0, true
}

func (a *Agent) reportUpdate(ctx context.Context, rep api.AgentUpdateReport) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return a.c.json(ctx, "POST", "/agent/v1/update", rep, nil)
}

var installerFile = regexp.MustCompile(`^better-backup-agent_[0-9A-Za-z.+~-]+_[a-z]+_[a-z0-9]+\.(msi|pkg|deb|rpm)$`)

// installUpdate fetches and checks the installer, waits for the agent to
// be idle and hands over to the updater. When it returns nil the update
// ended, one way or the other, and the plan file says how.
func (a *Agent) installUpdate(ctx context.Context, kind string, offer *api.AgentUpdate) error {
	if !installerFile.MatchString(offer.Name) || !strings.HasSuffix(offer.Name, "."+kind) {
		return fmt.Errorf("the server offered %q, which is not an installer for this machine", offer.Name)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	folder := filepath.Join(a.dir, updateFolder)
	os.RemoveAll(folder)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	a.log.Info("a newer agent is available; fetching it", "version", offer.Version, "installer", offer.Name)
	installer := filepath.Join(folder, offer.Name)
	if err := a.fetchInstaller(ctx, offer, installer); err != nil {
		return err
	}
	if err := checkSigner(ctx, exe, installer); err != nil {
		return err
	}
	// The updater is this program, kept aside: it must outlive the
	// installed one, and it is what goes back if the new one fails.
	updater := filepath.Join(folder, "bb-agent-updater")
	if runtime.GOOS == "windows" {
		updater += ".exe"
	}
	if err := copyFile(exe, updater, 0o700); err != nil {
		return fmt.Errorf("keeping a copy of this version: %w", err)
	}

	// Not in the middle of a job: wait for the one that runs, and keep
	// the next from starting, here and on the server.
	a.work.Lock()
	defer a.work.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	rep := api.AgentUpdateReport{State: api.UpdateInstalling, From: Version, Target: offer.Version}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, code, err := a.c.do(rctx, "POST", "/agent/v1/update", mustJSON(rep), "application/json")
	cancel()
	if err != nil {
		if code == http.StatusConflict {
			return errBusy{err} // a job was handed out meanwhile
		}
		return errBusy{fmt.Errorf("telling the server: %w", err)}
	}
	plan := &updatePlan{From: Version, To: offer.Version, Install: kind, Installer: installer, Exe: exe, State: planInstalling, Started: time.Now()}
	if err := savePlan(a.dir, plan); err != nil {
		return err
	}
	a.log.Info("installing the newer agent; the agent restarts", "from", Version, "to", offer.Version)
	if err := startUpdater(updater, []string{"--dir", a.dir, "apply-update"}); err != nil {
		os.Remove(planPath(a.dir))
		return fmt.Errorf("starting the updater: %w", err)
	}
	// Normally the installer stops this process within seconds. If it
	// fails before that, the updater says so in the plan.
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(updateSettle):
		}
		if _, settled := a.settleUpdate(ctx); settled {
			return nil
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// fetchInstaller downloads the installer and checks its size and hash.
func (a *Agent) fetchInstaller(ctx context.Context, offer *api.AgentUpdate, to string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", a.c.base+"/installers/"+url.PathEscape(offer.Name), nil)
	if err != nil {
		return err
	}
	resp, err := a.c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetching the installer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching the installer: %w", httpError(resp))
	}
	f, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	// One byte more than promised is enough to know it is too long.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, offer.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("fetching the installer: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != offer.Size || !strings.EqualFold(got, offer.SHA256) {
		os.Remove(to)
		return fmt.Errorf("the installer that arrived is not the one the server described (%d bytes, SHA-256 %s; expected %d bytes, %s); it was not installed", n, got, offer.Size, offer.SHA256)
	}
	return nil
}

func copyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Updater is what "bb-agent apply-update" runs: the earlier version of
// the agent, started from a copy, installing the newer one.
type Updater struct {
	Dir string
	Log *slog.Logger
	// Install runs the installer; Restart starts the installed agent
	// again, and Stop stops it. Nil means the real things.
	Install func(ctx context.Context, kind, installer, folder string) error
	Stop    func() error
	Restart func() error
}

// Run installs the update the plan describes and sees it through.
func (u *Updater) Run(ctx context.Context) error {
	p := loadPlan(u.Dir)
	if p == nil || p.State != planInstalling {
		return errors.New("there is no update to install")
	}
	if u.Log == nil {
		u.Log = slog.Default()
	}
	if u.Install == nil {
		u.Install = runInstaller
	}
	if u.Stop == nil {
		u.Stop = stopService
	}
	if u.Restart == nil {
		u.Restart = restartService
	}
	folder := filepath.Join(u.Dir, updateFolder)
	u.Log.Info("installing", "installer", p.Installer, "from", p.From, "to", p.To)
	ictx, cancel := context.WithTimeout(ctx, updateInstallLimit)
	err := u.Install(ictx, p.Install, p.Installer, folder)
	cancel()
	if err == nil {
		u.Log.Info("the installer finished; waiting for the new version to reach the server")
		deadline := time.Now().Add(updateHealthWait)
		for time.Now().Before(deadline) {
			if cur := loadPlan(u.Dir); cur != nil && cur.State == planOK {
				u.Log.Info("the new version is running", "version", p.To)
				return nil
			}
			time.Sleep(time.Second)
		}
		err = fmt.Errorf("version %s was installed but did not reach the server within %s", p.To, updateHealthWait)
	} else {
		err = fmt.Errorf("the installer failed: %w", err)
	}
	u.Log.Error("the update failed; going back to the earlier version", "err", err)
	if rerr := u.rollBack(p); rerr != nil {
		err = fmt.Errorf("%w. Putting version %s back failed too: %v. Install the agent on this machine by hand", err, p.From, rerr)
	} else {
		err = fmt.Errorf("%w. Version %s was put back and is running", err, p.From)
	}
	p.State, p.Error = planFailed, err.Error()
	if serr := savePlan(u.Dir, p); serr != nil {
		u.Log.Error("could not record the failure", "err", serr)
	}
	return err
}

// rollBack makes the installed program the earlier version again, which
// is this very program, and starts it.
func (u *Updater) rollBack(p *updatePlan) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if sum := fileSHA256(self); sum == "" || sum != fileSHA256(p.Exe) {
		// Windows cannot replace a program that runs.
		if runtime.GOOS == "windows" {
			if err := u.Stop(); err != nil {
				u.Log.Warn("stopping the agent", "err", err)
			}
		}
		tmp := p.Exe + ".rollback"
		if err := copyFile(self, tmp, 0o755); err != nil {
			return err
		}
		var rerr error
		for try := 0; try < 10; try++ {
			if rerr = os.Rename(tmp, p.Exe); rerr == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if rerr != nil {
			os.Remove(tmp)
			return rerr
		}
		u.Log.Info("put the earlier program back", "path", p.Exe)
	}
	return u.Restart()
}
