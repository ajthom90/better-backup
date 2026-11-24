// Package agent implements the better-backup agent: it enrolls with a
// server, waits for backup and restore jobs, and runs them.
package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ajthom90/better-backup/internal/agentctl"
	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	_ "github.com/ajthom90/better-backup/internal/hypervisor/all"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/m365"
	"github.com/ajthom90/better-backup/internal/restore"
	"github.com/ajthom90/better-backup/internal/tree"
	"github.com/ajthom90/better-backup/internal/truenas"
	"github.com/ajthom90/better-backup/internal/vss"
	"github.com/ajthom90/better-backup/internal/zfs"
)

// Version is set at build time.
var Version = "dev"

// Config is the agent's saved identity, written by Enroll.
type Config struct {
	Server  string `json:"server"`
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	IDKey   []byte `json:"id_key"`
}

func loadConfig(dir string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, "agent.json"))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("this agent is not enrolled (no agent.json in %s); run: bb-agent enroll", dir)
	}
	if err != nil {
		return nil, err
	}
	c := new(Config)
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if len(c.IDKey) != 32 {
		return nil, errors.New("agent.json is damaged; enroll this agent again")
	}
	return c, nil
}

// Enrolled reports whether dir holds an agent identity.
func Enrolled(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "agent.json"))
	return err == nil
}

// Enroll registers this machine with a server. The fingerprint pins the
// server's certificate authority; the token authorizes the enrollment.
func Enroll(dir, serverURL, token, fingerprint, name string) (*Config, error) {
	serverURL = strings.TrimRight(serverURL, "/")
	if u, err := url.Parse(serverURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, permanent{fmt.Errorf("server must be an https URL such as https://backup.example.com:8443, got %q", serverURL)}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	caPEM, err := api.FetchCA(serverURL, fingerprint)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: host}}, key)
	if err != nil {
		return nil, err
	}
	req := api.EnrollRequest{Token: token, Name: name, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: Version,
		CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))}
	tlsCfg, err := api.PinnedTLS(caPEM, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	body, _ := json.Marshal(req)
	resp, err := hc.Post(serverURL+"/agent/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, permanent{httpError(resp)} // the token was refused: trying again cannot help
	}
	if resp.StatusCode != 200 {
		return nil, httpError(resp)
	}
	var er api.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Server: serverURL, AgentID: er.AgentID, Name: name, IDKey: er.IDKey}
	cfgJSON, _ := json.MarshalIndent(cfg, "", "  ")
	files := []struct {
		name string
		data []byte
	}{
		{"ca.pem", caPEM},
		{"agent.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})},
		{"agent.crt", []byte(er.Cert)},
		{"agent.json", cfgJSON}, // last: its presence marks the agent as enrolled
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func httpError(resp *http.Response) error {
	var e api.Error
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("server: %s", e.Error)
	}
	return fmt.Errorf("server returned %s", resp.Status)
}

// client talks to the server with the agent's certificate.
type client struct {
	base string
	http *http.Client
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

func newClient(dir string, cfg *Config) (*client, error) {
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	if err != nil {
		return nil, err
	}
	tlsCfg, err := api.PinnedTLS(caPEM, &cert)
	if err != nil {
		return nil, err
	}
	c := &client{base: cfg.Server, http: &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}}}
	c.enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	c.dec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(64<<20))
	return c, nil
}

// permanent marks errors that retrying cannot fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// do sends a request, retrying network failures and server errors.
func (c *client) do(ctx context.Context, method, path string, body []byte, ctype string) ([]byte, int, error) {
	var last error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			last = err
			continue
		}
		data, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case rerr != nil:
			last = rerr
		case resp.StatusCode >= 500:
			resp.Body = io.NopCloser(bytes.NewReader(data))
			last = httpError(resp)
		case resp.StatusCode >= 400:
			resp.Body = io.NopCloser(bytes.NewReader(data))
			return nil, resp.StatusCode, permanent{httpError(resp)}
		default:
			return data, resp.StatusCode, nil
		}
	}
	return nil, 0, last
}

func (c *client) json(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	data, _, err := c.do(ctx, method, path, body, "application/json")
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *client) Missing(ctx context.Context, ids []keys.ID) ([]keys.ID, error) {
	var out struct {
		Missing []keys.ID `json:"missing"`
	}
	err := c.json(ctx, "POST", "/agent/v1/chunks/missing", map[string][]keys.ID{"ids": ids}, &out)
	return out.Missing, err
}

func (c *client) Put(ctx context.Context, id keys.ID, data []byte) error {
	_, _, err := c.do(ctx, "PUT", "/agent/v1/chunks/"+id.String(), c.enc.EncodeAll(data, nil), "application/zstd")
	return err
}

func (c *client) Get(ctx context.Context, id keys.ID) ([]byte, error) {
	data, _, err := c.do(ctx, "GET", "/agent/v1/chunks/"+id.String(), nil, "")
	if err != nil {
		return nil, err
	}
	return c.dec.DecodeAll(data, nil)
}

// Agent runs jobs for one enrolled machine.
type Agent struct {
	dir string
	cfg *Config
	c   *client
	log *slog.Logger

	mu    sync.Mutex
	state state
	// Notify, if set, hears about jobs: "started", then "progress" with
	// the job's latest figures, then "finished" with its error or nil.
	Notify func(event string, j *api.Job, detail any)
}

// state is what the agent last saw of the server and of its own work.
type state struct {
	connected   bool
	revoked     bool
	lastContact time.Time
	err         string
	job         *agentctl.Job
	progress    any
}

func (a *Agent) setState(f func(*state)) {
	a.mu.Lock()
	f(&a.state)
	a.mu.Unlock()
}

// New loads the agent identity saved in dir.
func New(dir string, log *slog.Logger) (*Agent, error) {
	cfg, err := loadConfig(dir)
	if err != nil {
		return nil, err
	}
	c, err := newClient(dir, cfg)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if missing := vss.EnablePrivileges(); len(missing) > 0 {
		log.Warn("could not enable some backup privileges; files this account cannot open will be skipped", "missing", missing)
	}
	return &Agent{dir: dir, cfg: cfg, c: c, log: log}, nil
}

// Run polls for jobs until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("better-backup agent started", "server", a.cfg.Server, "agent", a.cfg.AgentID, "version", Version)
	backoff := time.Second
	// Until the server has answered once, ask without waiting for work:
	// whoever is watching wants to know now whether the server is there.
	wait := "0"
	for ctx.Err() == nil {
		data, code, err := a.c.pollOnce(ctx, "/agent/v1/jobs/next?wait="+wait+"&version="+url.QueryEscape(Version))
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var p permanent
			revoked := errors.As(err, &p) && code == http.StatusUnauthorized
			a.setState(func(s *state) { s.connected, s.revoked, s.err = false, revoked, err.Error() })
			wait = "0"
			if revoked {
				a.log.Error("the server rejected this agent; it may have been revoked", "err", err)
				backoff = time.Minute
			} else {
				a.log.Warn("cannot reach the server; will retry", "err", err, "in", backoff)
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff, wait = time.Second, "25"
		a.setState(func(s *state) { s.connected, s.revoked, s.err, s.lastContact = true, false, "", time.Now() })
		if code == http.StatusNoContent {
			continue
		}
		var j api.Job
		if err := json.Unmarshal(data, &j); err != nil {
			a.log.Error("could not decode job", "err", err)
			continue
		}
		a.runJob(ctx, &j)
	}
	return nil
}

func newRequest(ctx context.Context, method, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, nil)
}

// pollOnce is a single long poll, without the retry loop of do: the
// caller has its own backoff.
func (c *client) pollOnce(ctx context.Context, path string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, permanent{httpError(resp)}
	}
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func (a *Agent) runJob(ctx context.Context, j *api.Job) {
	log := a.log.With("job", j.ID, "type", j.Type)
	log.Info("job started")
	var (
		mu       sync.Mutex
		progress any
		warnings []string
	)
	warn := func(msg string) {
		log.Warn(msg)
		mu.Lock()
		if len(warnings) < api.MaxWarnings {
			warnings = append(warnings, msg)
		}
		mu.Unlock()
	}
	notify := func(event string, detail any) {
		if a.Notify != nil {
			a.Notify(event, j, detail)
		}
	}
	setProgress := func(v any) {
		mu.Lock()
		progress = v
		mu.Unlock()
		a.setState(func(s *state) { s.progress = v })
		notify("progress", v)
	}
	a.setState(func(s *state) {
		s.job, s.progress = &agentctl.Job{ID: j.ID, Type: j.Type, Started: time.Now()}, nil
	})
	defer a.setState(func(s *state) { s.job, s.progress, s.lastContact = nil, nil, time.Now() })
	notify("started", nil)

	// Heartbeat: report progress every few seconds so the server knows
	// the job is alive. If the server says the job is gone, stop work.
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-jctx.Done():
				return
			case <-t.C:
				mu.Lock()
				p := progress
				mu.Unlock()
				err := a.c.json(jctx, "POST", "/agent/v1/jobs/"+j.ID+"/progress", p, nil)
				var perm permanent
				if errors.As(err, &perm) {
					log.Error("the server no longer tracks this job; stopping", "err", err)
					cancel()
					return
				}
			}
		}
	}()

	var req api.CompleteRequest
	var err error
	switch {
	case j.Type == api.JobBackup && j.Backup != nil:
		var stats tree.Stats
		release := func() {}
		opts := backup.Options{
			Paths: j.Backup.Paths, Excludes: j.Backup.Excludes, Parent: j.Backup.Parent, IDKey: [32]byte(a.cfg.IDKey),
			Progress: func(s tree.Stats) { setProgress(s) }, Warn: warn}
		// On Windows, read from a VSS snapshot so open and locked files
		// are backed up in a consistent state. If the snapshot cannot be
		// made, back up the live files and say so.
		// Volume images are always read from a snapshot: an image of a
		// volume that is changing would not be a usable file system.
		var snapPaths []string
		var set *vss.Set
		if runtime.GOOS == "windows" {
			if !j.Backup.NoSnapshot {
				snapPaths = append(snapPaths, j.Backup.Paths...)
			}
			for _, v := range j.Backup.Volumes {
				snapPaths = append(snapPaths, strings.TrimRight(v, `\`)+`\`)
			}
		}
		if j.Backup.SystemState {
			// The system's VSS writers say which files make up its
			// state; they are open and locked, so they need the snapshot.
			sel, names, ad, serr := systemState()
			if serr != nil {
				err = fmt.Errorf("reading the system state file lists: %w", serr)
				break
			}
			log.Info("system state", "writers", names, "drives", sel.Roots())
			if ad {
				log.Info("this machine is a domain controller: the Active Directory database and SYSVOL are included")
			}
			opts.SystemState = &backup.SystemState{Roots: sel.Roots(), Match: sel.Match}
			snapPaths = append(snapPaths, sel.Roots()...)
		}
		// A whole machine: look at its disks now, so the volumes that
		// must be read from a snapshot join the snapshot set.
		var plan *backup.MachinePlan
		if j.Backup.BareMetal {
			var perr error
			if plan, perr = backup.PlanMachine(j.Backup.MachineDisks); perr != nil {
				warn("the whole machine was not backed up: " + perr.Error())
			} else {
				for _, w := range plan.Warnings {
					warn(w)
				}
				snapPaths = append(snapPaths, plan.SnapPaths...)
				defer plan.Close()
			}
		}
		if len(snapPaths) > 0 {
			var skipped []string
			var serr error
			set, skipped, serr = vss.Create(snapPaths, 10*time.Minute)
			for _, s := range skipped {
				warn("no snapshot for one volume, backing up its live files: " + s)
			}
			if serr != nil {
				set = nil
				if opts.SystemState != nil {
					warn("the system state was not backed up: it needs a VSS snapshot, and that failed")
					opts.SystemState = nil
				}
				warn("could not create a VSS snapshot, backing up live files (files in use may be skipped): " + serr.Error())
			} else {
				log.Info("VSS snapshot created", "volumes", set.Volumes())
				opts.MapPath = set.Path
				release = func() {
					if cerr := set.Close(); cerr != nil {
						log.Warn("releasing the VSS snapshot", "err", cerr)
					}
				}
			}
		}
		covered := map[string]bool{}
		if plan != nil {
			m, merr := plan.Open(set.Path)
			if merr != nil {
				warn("the whole machine was not backed up: " + merr.Error())
			} else {
				opts.Machine = m
				for _, d := range m.Layout.Disks {
					for _, pt := range d.Partitions {
						if pt.Image != "" && pt.Mount != "" {
							covered[strings.ToUpper(pt.Mount)] = true
						}
					}
				}
			}
		}
		// Elsewhere, ZFS does the same job: every dataset that holds a
		// path, and every ZFS volume, is read from a snapshot taken at
		// one instant.
		var frozen *zfs.Session
		if runtime.GOOS != "windows" {
			var zwarn []string
			var zerr error
			frozen, zwarn, zerr = zfs.Begin(jctx, zfs.Options{Agent: a.cfg.AgentID, Tag: "better-backup-" + j.ID,
				Paths: j.Backup.Paths, Volumes: j.Backup.Volumes, Live: j.Backup.NoSnapshot})
			for _, w := range zwarn {
				warn(w)
			}
			if zerr != nil {
				warn("could not look for ZFS datasets, backing up live files: " + zerr.Error())
			}
			opts.Excludes = append(append([]string{}, opts.Excludes...), frozen.Excludes()...)
			if snaps := frozen.Snapshots(); len(snaps) > 0 {
				log.Info("ZFS snapshots created", "snapshots", snaps)
				opts.MapPath = frozen.Path
			}
			release = func() {
				if cerr := frozen.Close(); cerr != nil {
					warn("removing the backup's ZFS snapshots failed; remove them by hand: " + cerr.Error())
				}
			}
		}
		for _, v := range j.Backup.Volumes {
			if covered[strings.ToUpper(strings.TrimRight(v, `\`))] {
				continue // already in the whole-machine backup
			}
			name, readPath, zvol := v, v, false
			if runtime.GOOS == "windows" {
				name = strings.ToUpper(strings.TrimRight(v, `\`))
				readPath = strings.TrimRight(set.Path(name+`\`), `\`)
				if set == nil || strings.EqualFold(readPath, name) {
					warn("volume " + name + " was not backed up: it has no VSS snapshot")
					continue
				}
			} else {
				var ok bool
				if name, readPath, zvol, ok = frozen.Volume(v); !ok {
					continue // Begin said why
				}
			}
			src, oerr := backup.OpenImage(name, readPath)
			if oerr != nil {
				warn("volume " + name + " was not backed up: " + oerr.Error())
				continue
			}
			// The unwritten parts of a ZFS volume read as zeros and
			// take no room in the backup.
			src.SkipZero = zvol
			opts.Images = append(opts.Images, src)
		}
		if j.Backup.TrueNASConfig {
			stage := filepath.Join(a.dir, "truenas-config")
			version, terr := truenas.SaveConfig(stage)
			// The copy holds the system's secret key: it must not
			// outlive the backup.
			defer os.RemoveAll(stage)
			if terr != nil {
				warn("the TrueNAS configuration was not backed up: " + terr.Error())
			} else {
				log.Info("TrueNAS configuration saved", "version", version)
				opts.Folders = append(opts.Folders, backup.Folder{Name: tree.TrueNASConfigName, Path: stage})
			}
		}
		vms := j.Backup.VMs
		var drv hypervisor.Driver
		if len(vms) > 0 {
			var derr error
			if drv, derr = hypervisor.For(j.Backup.Hypervisor); derr != nil {
				warn("the virtual machines were not backed up: " + derr.Error())
				vms = nil
			}
		}
		for _, v := range vms {
			if v == "*" {
				var lerr error
				if vms, lerr = drv.List(jctx, j.Backup.Connection); lerr != nil {
					warn("could not list the virtual machines on this host: " + lerr.Error())
				}
				break
			}
		}
		for _, name := range vms {
			opts.VMs = append(opts.VMs, backup.VMSource{Name: name, Open: func(vm *backup.VMSource, prev []byte) error {
				fz, err := drv.Freeze(jctx, vm.Name, hypervisor.Options{Tracking: j.Backup.VMTracking != "", Previous: prev,
					Tag: j.ID, Scope: j.PolicyID, StateDir: a.dir, Connection: j.Backup.Connection})
				if err != nil {
					return err
				}
				for _, w := range fz.Warnings {
					warn(w)
				}
				vm.ConfigName, vm.Config, vm.Disks = drv.ConfigFile(), fz.Config, fz.Disks
				for i := range vm.Disks {
					vm.Disks[i].Verify = j.Backup.VMTracking == "check"
				}
				vm.Check = fz.Check
				vm.Close = func() {
					if rerr := fz.Release(); rerr != nil {
						warn("virtual machine " + vm.Name + ": " + rerr.Error())
					}
				}
				log.Info("virtual machine frozen for backup", "vm", vm.Name, "hypervisor", drv.Kind(), "disks", len(vm.Disks))
				return nil
			}})
		}
		var m365err error
		if m := j.Backup.M365; m != nil {
			root, merr := m365Source(jctx, j.Backup, warn)
			if merr != nil {
				m365err = merr
				warn("Microsoft 365 was not backed up: " + strings.TrimPrefix(merr.Error(), "Microsoft 365: "))
			} else {
				opts.Virtual = append(opts.Virtual, backup.VirtualSource{Name: tree.M365Name, Root: root})
			}
		}
		req.Sources, stats, err = backup.Run(jctx, a.c, opts)
		if err != nil && m365err != nil && jctx.Err() == nil && len(opts.Paths)+len(opts.Images)+len(opts.VMs)+len(opts.Folders) == 0 && opts.Machine == nil && opts.SystemState == nil {
			err = m365err // nothing else was asked for: say what went wrong
		}
		for _, src := range opts.Images {
			src.Close() // a no-op for images the backup already closed
		}
		if opts.Machine != nil {
			for _, src := range opts.Machine.Images {
				if src.Close != nil {
					src.Close()
				}
			}
		}
		release()
		req.Stats = &stats
	case j.Type == api.JobRestore && j.Restore != nil:
		var stats restore.Stats
		get := func(id keys.ID) ([]byte, error) { return a.c.Get(jctx, id) }
		var sys *restore.SystemState
		if s := j.Restore.SystemState; s != nil {
			sys = &restore.SystemState{Registry: s.Registry, AuthoritativeSYSVOL: s.AuthoritativeSYSVOL,
				AuthoritativeSubtrees: s.AuthoritativeSubtrees, Restart: s.Restart}
		}
		stats, err = restore.Run(jctx, get, restore.Options{SystemState: sys,
			Sources: j.Restore.Sources, SourceOS: j.Restore.SourceOS, Include: j.Restore.Include,
			Target: j.Restore.Target, Overwrite: j.Restore.Overwrite, VMName: j.Restore.VMName, Disks: j.Restore.Disks, Connection: j.Restore.Connection,
			Progress: func(s restore.Stats) { setProgress(s) }, Warn: warn})
		req.Restore = &stats
	default:
		err = fmt.Errorf("this agent version does not support %q jobs", j.Type)
	}
	cancel()
	<-done
	if err != nil {
		req.Error = err.Error()
		req.Sources = nil
	}
	req.Warnings = warnings
	notify("finished", err)
	if ctx.Err() != nil {
		// Shutting down: the server fails the job when we reconnect.
		log.Warn("job interrupted by shutdown")
		return
	}
	if cerr := a.c.json(ctx, "POST", "/agent/v1/jobs/"+j.ID+"/complete", &req, nil); cerr != nil {
		log.Error("could not report the job result", "err", cerr)
		return
	}
	if err != nil {
		log.Error("job failed", "err", err)
	} else {
		log.Info("job finished", "warnings", len(warnings))
	}
}

// m365Source signs in to the tenant of a backup and returns what to
// back up from it.
func m365Source(ctx context.Context, b *api.BackupSpec, warn func(string)) (*backup.VNode, error) {
	c := b.M365Connection
	if c == nil {
		return nil, errors.New("Microsoft 365: the tenant of this policy was removed from the server")
	}
	client := m365.New(m365.Connection{TenantID: c.TenantID, ClientID: c.ClientID, ClientSecret: c.ClientSecret, LoginURL: c.LoginURL, GraphURL: c.GraphURL})
	return client.Source(ctx, m365.Spec{Users: b.M365.Users, Mail: b.M365.Mail, OneDrive: b.M365.OneDrive}, warn)
}

// systemState asks the VSS writers which files make up this machine's
// system state. ad is true on a domain controller.
func systemState() (sel *vss.Selection, names []string, ad bool, err error) {
	if runtime.GOOS != "windows" {
		return nil, nil, false, errors.New("the system state can only be backed up on Windows")
	}
	writers, err := vss.Writers(5 * time.Minute)
	if err != nil {
		return nil, nil, false, err
	}
	files, excludes, names := vss.SystemState(writers)
	if len(files) == 0 {
		return nil, nil, false, errors.New("no VSS writer reported system state files")
	}
	for _, w := range writers {
		ad = ad || (w.IsActiveDirectory() && len(w.Components) > 0)
	}
	return vss.NewSelection(files, excludes), names, ad, nil
}
