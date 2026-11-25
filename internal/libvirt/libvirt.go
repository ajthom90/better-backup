// Package libvirt backs up and restores the virtual machines of a
// KVM host managed by libvirt. It drives libvirt through virsh. A
// running machine is read through libvirt's backup job, which serves
// each disk as it was when the job began while the machine keeps
// writing; a machine that is switched off is read from its disk files.
// Checkpoints let the next backup ask which parts were written since.
package libvirt

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/backup"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/nbd"
	"github.com/ajthom90/better-backup/internal/qemuimg"
)

// ConfigFile is the name of the entry that holds a machine's Config in
// a snapshot.
const ConfigFile = "libvirt.json"

// Config describes a machine well enough to define it again.
type Config struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid,omitempty"`
	State string `json:"state,omitempty"` // when backed up
	// XML is the machine's persistent definition.
	XML string `json:"xml"`
	// NVRAM is the firmware's variable store of a UEFI machine,
	// compressed with gzip.
	NVRAM []byte `json:"nvram,omitempty"`
	// Checkpoint names the libvirt checkpoint this backup made. The
	// next backup asks what was written since.
	Checkpoint string `json:"checkpoint,omitempty"`
	Disks      []Disk `json:"disks"`
}

// Disk is one of a machine's disks.
type Disk struct {
	// Target is the disk's name in the machine, such as "vda".
	Target string `json:"target"`
	// File is the name of the disk's image inside the snapshot.
	File string `json:"file"`
	// Path is the disk's file or device on the host when backed up.
	Path   string `json:"path"`
	Format string `json:"format"`
	Block  bool   `json:"block,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Bitmap says the checkpoint covers this disk.
	Bitmap bool `json:"bitmap,omitempty"`
}

func init() { hypervisor.Register(driver{}) }

type driver struct{}

func (driver) Kind() string       { return "libvirt" }
func (driver) Label() string      { return "KVM (libvirt)" }
func (driver) ConfigFile() string { return ConfigFile }

func (driver) Available() error {
	if runtime.GOOS != "linux" {
		return errors.New("KVM backup needs a Linux host with libvirt")
	}
	if _, err := exec.LookPath("virsh"); err != nil {
		return errors.New("KVM backup needs libvirt: virsh is not installed on this computer")
	}
	return qemuimg.Available()
}

// uri is the libvirt connection. BB_LIBVIRT_URI overrides the system
// one, mainly for tests.
func uri() string {
	if u := os.Getenv("BB_LIBVIRT_URI"); u != "" {
		return u
	}
	return "qemu:///system"
}

// virsh runs one virsh command and returns what it printed.
func virsh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "virsh", append([]string{"--connect", uri()}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(errb.String())
		msg = strings.TrimSpace(strings.TrimPrefix(msg, "error:"))
		if i := strings.LastIndex(msg, "\nerror:"); i >= 0 {
			// virsh prints a general line first and the reason last.
			msg = strings.TrimSpace(msg[i+len("\nerror:"):])
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (driver) List(ctx context.Context, _ *hypervisor.Connection) ([]string, error) {
	out, err := virsh(ctx, "list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// checkpointPrefix starts the name of every checkpoint made for one
// policy's backups.
func checkpointPrefix(scope string) string {
	if scope == "" {
		scope = "manual"
	}
	return "bb-" + clean(scope) + "-"
}

func clean(s string) string {
	return strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			return c
		}
		return '_'
	}, s)
}

func (d driver) Freeze(ctx context.Context, vm string, o hypervisor.Options) (*hypervisor.Frozen, error) {
	state, err := virsh(ctx, "domstate", vm)
	if err != nil {
		return nil, err
	}
	doc, err := virsh(ctx, "dumpxml", "--inactive", "--security-info", vm)
	if err != nil {
		return nil, fmt.Errorf("reading its definition: %w", err)
	}
	dom, err := parseDomain([]byte(doc))
	if err != nil {
		return nil, err
	}
	cfg := Config{Name: dom.Name, UUID: dom.UUID, State: state, XML: doc}
	f := &hypervisor.Frozen{}
	warn := func(format string, args ...any) {
		f.Warnings = append(f.Warnings, vm+": "+fmt.Sprintf(format, args...))
	}
	var skipped []string
	cfg.Disks, skipped = dom.disks()
	for _, w := range skipped {
		warn("%s", w)
	}
	if len(cfg.Disks) == 0 {
		return nil, errors.New("it has no disks that can be backed up")
	}
	if dom.swtpm() {
		warn("the machine has an emulated TPM, whose contents are not backed up; a restored machine gets an empty one")
	}
	if p := strings.TrimSpace(dom.OS.NVRAM.Path); p != "" {
		if cfg.NVRAM, err = readNVRAM(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			warn("its UEFI settings (%s) could not be read and are not in this backup: %v", p, err)
		}
	}
	var prev *Config
	if p := new(Config); len(o.Previous) > 0 && json.Unmarshal(o.Previous, p) == nil {
		prev = p
	}
	live := state == "running" || state == "paused" || state == "pmsuspended"
	if live {
		err = d.freezeLive(ctx, vm, o, &cfg, prev, f, warn)
	} else {
		// Read in full, even with change tracking: nothing recorded
		// what was written to the files while no QEMU had them open.
		// This is no warning, or every backup of a machine that is
		// kept switched off would end with one.
		err = d.freezeOff(ctx, &cfg, f)
	}
	if err != nil {
		return nil, err
	}
	for i := range f.Disks {
		cfg.Disks[i].Size = f.Disks[i].Size
	}
	if f.Config, err = json.Marshal(cfg); err != nil {
		for _, s := range f.Disks {
			s.Close()
		}
		return nil, errors.Join(err, f.Release())
	}
	return f, nil
}

// readNVRAM returns a UEFI variable store, compressed.
func readNVRAM(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > 16<<20 {
		return nil, errors.New("it is unexpectedly large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(raw)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// source turns an open export into something the archiver reads. Parts
// the server says read as zeros are not read at all.
func source(name string, c *nbd.Client, closer func()) (backup.ImageSource, error) {
	src := backup.ImageSource{Name: name, Size: c.Size(), R: c, SkipZero: true, Close: closer}
	if c.Has(nbd.Allocation) {
		ext, err := c.Status(nbd.Allocation)
		if err != nil {
			return src, err
		}
		src.Used = []backup.Extent{}
		for _, e := range ext {
			if e.Flags&2 == 0 {
				src.Used = append(src.Used, backup.Extent{Off: e.Off, Len: e.Len})
			}
		}
	}
	return src, nil
}

// freezeOff opens the disk files of a machine that is not running.
// QEMU's image locking keeps the machine from starting while they are
// being read.
func (driver) freezeOff(ctx context.Context, cfg *Config, f *hypervisor.Frozen) error {
	for i := range cfg.Disks {
		dk := &cfg.Disks[i]
		im, err := qemuimg.Open(ctx, dk.Path, dk.Format, false, nbd.Allocation)
		if err == nil {
			var src backup.ImageSource
			if src, err = source(dk.File, im.Client, func() { im.Close() }); err == nil {
				f.Disks = append(f.Disks, src)
				continue
			}
			im.Close()
		}
		for _, s := range f.Disks {
			s.Close()
		}
		return fmt.Errorf("opening its disk %s (%s): %w", dk.Target, dk.Path, err)
	}
	f.Release = func() error { return nil }
	return nil
}

// socketDirs lists folders where QEMU may be able to create the socket
// it serves a machine's disks on. The machine's own folder under
// libvirt's state is the first choice: QEMU can write there under
// AppArmor and SELinux, and other users cannot reach it.
func socketDirs(ctx context.Context, vm string) (dirs []string, cleanup func()) {
	cleanup = func() {}
	if id, err := virsh(ctx, "domid", vm); err == nil && id != "" && id != "-" {
		for _, root := range []string{"/var/lib/libvirt/qemu", "/run/libvirt/qemu"} {
			if m, _ := filepath.Glob(filepath.Join(root, "domain-"+id+"-*")); len(m) == 1 {
				dirs = append(dirs, m[0])
			}
		}
	}
	// Elsewhere: a folder QEMU, running as another user, can create a
	// socket in but nobody can list, with a name nobody can guess.
	if tmp, err := os.MkdirTemp("", "bb-nbd-"); err == nil {
		os.Chmod(tmp, 0o733)
		dirs = append(dirs, tmp)
		cleanup = func() { os.RemoveAll(tmp) }
	}
	return dirs, cleanup
}

// staleAfter is how long another policy's checkpoint may go unused
// before it is taken for the remains of a deleted policy.
const staleAfter = 60 * 24 * time.Hour

// checkpoints reads the table virsh prints of a machine's checkpoints.
// stale lists the ones this program made for other policies so long
// ago that those policies must be gone.
func checkpoints(table, prefix string, now time.Time) (have map[string]bool, stale []string) {
	have = map[string]bool{}
	for _, l := range lines(table) {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		made, err := time.Parse("2006-01-02 15:04:05 -0700", strings.Join(f[len(f)-3:], " "))
		if err != nil {
			continue
		}
		name := strings.Join(f[:len(f)-3], " ")
		have[name] = true
		if strings.HasPrefix(name, "bb-") && !strings.HasPrefix(name, prefix) && now.Sub(made) > staleAfter {
			stale = append(stale, name)
		}
	}
	return have, stale
}

// tracked reports whether the previous backup's checkpoint covers a
// disk, so that this backup can ask for its changes.
func tracked(prev *Config, d Disk) bool {
	if prev == nil {
		return false
	}
	for _, p := range prev.Disks {
		if p.Target == d.Target && p.File == d.File && p.Bitmap {
			return true
		}
	}
	return false
}

// backupXML describes a backup job. With incremental set, the disks
// that checkpoint covers are served with their changes since; the
// others are served in full.
func backupXML(cfg, prev *Config, socket, incremental, scratchDir string) string {
	var b strings.Builder
	b.WriteString("<domainbackup mode='pull'>")
	if incremental != "" {
		b.WriteString("<incremental>" + text(incremental) + "</incremental>")
	}
	b.WriteString("<server transport='unix'" + attr("socket", socket) + "/><disks>")
	for _, d := range cfg.Disks {
		b.WriteString("<disk" + attr("name", d.Target) + " backup='yes' type='file'")
		if incremental != "" && !tracked(prev, d) {
			b.WriteString(" backupmode='full'")
		}
		b.WriteString(">")
		if d.Block {
			// libvirt puts the scratch file, which holds what the
			// machine overwrites during the backup, beside the disk's
			// file. A block device has no such place.
			b.WriteString("<scratch" + attr("file", scratchDir+"/bb-scratch-"+clean(cfg.Name)+"-"+clean(d.Target)+".qcow2") + "/>")
		}
		b.WriteString("</disk>")
	}
	b.WriteString("</disks></domainbackup>")
	return b.String()
}

func checkpointXML(cfg *Config, name string) string {
	var b strings.Builder
	b.WriteString("<domaincheckpoint><name>" + text(name) + "</name><description>better-backup change tracking</description><disks>")
	for _, d := range cfg.Disks {
		if d.Bitmap {
			b.WriteString("<disk" + attr("name", d.Target) + " checkpoint='bitmap'/>")
		} else {
			b.WriteString("<disk" + attr("name", d.Target) + " checkpoint='no'/>")
		}
	}
	b.WriteString("</disks></domaincheckpoint>")
	return b.String()
}

// scratchDir is where scratch files for block devices go.
func scratchDir(chosen string) string {
	if chosen != "" {
		return chosen
	}
	if d := os.Getenv("BB_SCRATCH_DIR"); d != "" {
		return d
	}
	return "/var/lib/libvirt/images"
}

// begin starts a backup job, trying each socket folder in turn. It
// returns the socket in use.
func begin(ctx context.Context, vm string, cfg, prev *Config, dirs []string, incremental, checkpoint, scratch string) (string, error) {
	tmp, err := os.MkdirTemp("", "bb-libvirt-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	args := []string{"backup-begin", vm, filepath.Join(tmp, "backup.xml")}
	if checkpoint != "" {
		if err := os.WriteFile(filepath.Join(tmp, "checkpoint.xml"), []byte(checkpointXML(cfg, checkpoint)), 0o600); err != nil {
			return "", err
		}
		args = append(args, filepath.Join(tmp, "checkpoint.xml"))
	}
	var last error
	for _, dir := range dirs {
		socket := filepath.Join(dir, "bb-"+keys.RandomHex(8)+".sock")
		if err := os.WriteFile(filepath.Join(tmp, "backup.xml"), []byte(backupXML(cfg, prev, socket, incremental, scratchDir(scratch))), 0o600); err != nil {
			return "", err
		}
		_, last = virsh(ctx, args...)
		if last == nil {
			return socket, nil
		}
		if ctx.Err() != nil || !strings.Contains(strings.ToLower(last.Error()), "socket") {
			break
		}
	}
	return "", last
}

// freezeLive starts a libvirt backup job for a running machine and
// connects to the disks it serves.
func (driver) freezeLive(ctx context.Context, vm string, o hypervisor.Options, cfg *Config, prev *Config, f *hypervisor.Frozen, warn func(string, ...any)) error {
	prefix := checkpointPrefix(o.Scope)
	have := map[string]bool{}
	var stale []string
	if out, err := virsh(ctx, "checkpoint-list", vm); err == nil {
		have, stale = checkpoints(out, prefix, time.Now())
	}
	// Is a backup job in the way? One of this program's, left by an
	// agent that was stopped half way, is ended. Its socket's name
	// tells it from another program's.
	if job, err := virsh(ctx, "backup-dumpxml", vm); err == nil {
		if !strings.Contains(job, "/bb-") || !strings.Contains(job, ".sock") {
			return errors.New("another program's backup job is running on the machine")
		}
		if _, err := virsh(ctx, "domjobabort", vm); err != nil {
			return fmt.Errorf("ending a backup job that an earlier run left behind: %w", err)
		}
	}
	dirs, cleanup := socketDirs(ctx, vm)

	// Ask the guest to flush its file systems, if it runs the QEMU
	// guest agent. Without one the backup is what a power cut would
	// leave, which file systems and databases are built to survive.
	frozen := false
	fctx, stop := context.WithTimeout(ctx, 60*time.Second)
	if _, err := virsh(fctx, "domfsfreeze", vm); err == nil {
		frozen = true
	}
	stop()
	var socket, base string
	var err error
	// Only qcow2 disks can record what is written to them. The others
	// are read in full every time.
	trackable := false
	for i := range cfg.Disks {
		cfg.Disks[i].Bitmap = o.Tracking && cfg.Disks[i].Format == "qcow2"
		trackable = trackable || cfg.Disks[i].Bitmap
	}
	if trackable {
		cfg.Checkpoint = prefix + clean(o.Tag)
		if have[cfg.Checkpoint] {
			cfg.Checkpoint += "-" + keys.RandomHex(3)
		}
		if prev != nil && prev.Checkpoint != "" && !have[prev.Checkpoint] {
			warn("the checkpoint of the last backup (%s) is gone, so the disks are read in full", prev.Checkpoint)
		}
		if prev != nil && prev.Checkpoint != "" && have[prev.Checkpoint] {
			base = prev.Checkpoint
			if socket, err = begin(ctx, vm, cfg, prev, dirs, base, cfg.Checkpoint, o.ScratchDir); err != nil {
				warn("the changes since the last backup could not be asked for, so the disks are read in full: %v", err)
				base = ""
			}
		}
		if socket == "" {
			if socket, err = begin(ctx, vm, cfg, nil, dirs, "", cfg.Checkpoint, o.ScratchDir); err != nil {
				warn("change tracking could not be used, the disks are read in full: %v", err)
				cfg.Checkpoint = ""
				for i := range cfg.Disks {
					cfg.Disks[i].Bitmap = false
				}
			}
		}
	}
	if socket == "" {
		socket, err = begin(ctx, vm, cfg, nil, dirs, "", "", o.ScratchDir)
	}
	if frozen {
		if _, terr := virsh(context.WithoutCancel(ctx), "domfsthaw", vm); terr != nil {
			warn("the guest's file systems were frozen for the backup and could not be released; run 'virsh domfsthaw %s' on the host: %v", vm, terr)
		}
	}
	if err != nil {
		cleanup()
		return fmt.Errorf("starting its backup job: %w", err)
	}
	// QEMU ends the job by itself if it cannot keep the old contents of
	// what the machine overwrites, such as when the disk that holds the
	// scratch files is full. From then on the disks it serves are no
	// longer the disks as they were, and nothing read is to be trusted.
	f.Check = func() error {
		job, err := virsh(ctx, "backup-dumpxml", vm)
		if err != nil || !strings.Contains(job, socket) {
			return errors.New("its backup job ended before the disks were read, so what was read may mix old and new contents (is the disk that holds the scratch files full?)")
		}
		return nil
	}
	f.Release = func() error {
		_, err := virsh(context.WithoutCancel(ctx), "domjobabort", vm)
		cleanup()
		if err != nil {
			return fmt.Errorf("ending its backup job failed; run 'virsh domjobabort %s' on the host: %w", vm, err)
		}
		return nil
	}
	fail := func(err error) error {
		for _, s := range f.Disks {
			s.Close()
		}
		f.Disks = nil
		if cfg.Checkpoint != "" {
			virsh(context.WithoutCancel(ctx), "checkpoint-delete", vm, cfg.Checkpoint)
		}
		return errors.Join(err, f.Release())
	}

	// The checkpoints of earlier backups are no longer needed, except
	// the one the last finished backup made: if this job fails, the
	// next one still starts from there. Removing a checkpoint folds
	// its record of changes into the one before, so that stays right.
	for name := range have {
		if strings.HasPrefix(name, prefix) && name != cfg.Checkpoint && (name != base || cfg.Checkpoint == "") {
			if _, err := virsh(ctx, "checkpoint-delete", vm, name); err != nil {
				warn("the old checkpoint %s could not be removed: %v", name, err)
			}
		}
	}
	// So are the ones of policies that no longer exist.
	for _, name := range stale {
		virsh(ctx, "checkpoint-delete", vm, name)
	}

	for i := range cfg.Disks {
		dk := &cfg.Disks[i]
		// libvirt names each export after its disk and, in an
		// incremental job, the bitmap of changes "backup-" plus that.
		bitmap := nbd.DirtyBitmap("backup-" + dk.Target)
		contexts := []string{nbd.Allocation}
		if base != "" && tracked(prev, *dk) {
			contexts = append(contexts, bitmap)
		}
		c, err := nbd.Dial("unix", socket, dk.Target, contexts...)
		if err != nil {
			return fail(fmt.Errorf("opening its disk %s: %w", dk.Target, err))
		}
		src, err := source(dk.File, c, func() { c.Close() })
		if err != nil {
			c.Close()
			return fail(fmt.Errorf("reading the layout of its disk %s: %w", dk.Target, err))
		}
		if base != "" && tracked(prev, *dk) {
			switch {
			case !c.Has(bitmap):
				warn("the disk %s is read in full: the backup job does not offer its changes", dk.Target)
			default:
				ext, err := c.Status(bitmap)
				if err != nil {
					warn("the disk %s is read in full: its changes could not be listed: %v", dk.Target, err)
					break
				}
				src.Tracked = true
				for _, e := range ext {
					if e.Flags&1 != 0 {
						src.Changed = append(src.Changed, backup.Extent{Off: e.Off, Len: e.Len})
					}
				}
			}
		}
		f.Disks = append(f.Disks, src)
	}
	return nil
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}
