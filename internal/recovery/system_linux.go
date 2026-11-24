package recovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/disklayout"
)

// VolumeID is the label of the recovery disc's file system.
const VolumeID = "BB_RECOVERY"

const busybox = "/bin/busybox"

// Main runs the recovery disc. As the first process of the machine it
// also does what an init system would: mounts the kernel's file systems,
// loads drivers and brings up the network. It does not return.
func Main(version string) {
	pid1 := os.Getpid() == 1
	if pid1 {
		setupSystem()
	}
	con := openConsole()
	logFile, _ := os.OpenFile("/run/recovery.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var logOut io.Writer = io.Discard
	if logFile != nil {
		logOut = logFile
	}
	log := slog.New(slog.NewTextHandler(logOut, nil))
	slog.SetDefault(log)

	con.printf("\nbetter-backup recovery %s\n\nLoading drivers...\n", version)
	if pid1 {
		n := loadDrivers(log)
		log.Info("drivers loaded", "modules", n)
	}
	con.printf("Starting the network...\n")
	nw := &network{log: log, procs: map[string]*exec.Cmd{}}
	if pid1 {
		nw.start()
		nw.wait(15 * time.Second)
	}

	cfg := api.RecoveryConfig{}
	for _, f := range strings.Fields(readFile("/proc/cmdline")) {
		if v, ok := strings.CutPrefix(f, "bb.server="); ok {
			cfg.Server = v
		}
		if v, ok := strings.CutPrefix(f, "bb.fingerprint="); ok {
			cfg.Fingerprint = v
		}
	}
	ui := &screen{con: con, version: version, nw: nw}
	for {
		if cfg.Server == "" {
			ui.pause()
			con.printf("\nThis recovery disc does not know its server. Download the disc from the\nserver's web page (Bare-metal recovery) to have the address built in,\nor type it here.\n\n")
			cfg.Server = askServer(con)
			ui.resume()
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			Run(ctx, Options{Config: cfg, Version: version, Dir: "/run/bb-agent", Hardware: scanHardware, Log: log,
				Show:  ui.show,
				Clock: setClock,
				Command: func(cmd string) {
					ui.pause()
					con.printf("\n%s...\n", map[string]string{"reboot": "Restarting", "poweroff": "Switching off"}[cmd])
					shutdown(cmd)
				}})
		}()
		// The keyboard: a letter and Enter.
		change := ui.keys()
		cancel()
		<-done
		if change {
			cfg = api.RecoveryConfig{}
		}
	}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// setupSystem does the first steps of an init system.
func setupSystem() {
	os.Setenv("PATH", "/bin:/sbin:/usr/bin:/usr/sbin")
	os.Setenv("HOME", "/root")
	os.Setenv("TERM", "linux")
	for _, m := range []struct{ src, dst, fs, opts string }{
		{"proc", "/proc", "proc", ""}, {"sysfs", "/sys", "sysfs", ""}, {"devtmpfs", "/dev", "devtmpfs", "mode=0755"},
		{"tmpfs", "/run", "tmpfs", "mode=0755"}, {"tmpfs", "/tmp", "tmpfs", ""},
	} {
		os.MkdirAll(m.dst, 0o755)
		unix.Mount(m.src, m.dst, m.fs, 0, m.opts)
	}
	os.MkdirAll("/dev/pts", 0o755)
	unix.Mount("devpts", "/dev/pts", "devpts", 0, "")
	os.MkdirAll("/etc", 0o755)
	os.MkdirAll("/root", 0o700)
	unix.Sethostname([]byte("bb-recovery"))
	// Quiet the kernel's own messages on the screen; they stay in dmesg.
	os.WriteFile("/proc/sys/kernel/printk", []byte("3 4 1 3\n"), 0o644)
	exec.Command(busybox, "ip", "link", "set", "lo", "up").Run()
}

func setClock(t time.Time) {
	if d := time.Since(t); d > 2*time.Minute || d < -2*time.Minute {
		tv := unix.NsecToTimeval(t.UnixNano())
		unix.Settimeofday(&tv)
	}
}

func shutdown(cmd string) {
	unix.Sync()
	time.Sleep(time.Second)
	if os.Getpid() != 1 {
		os.Exit(0)
	}
	how := unix.LINUX_REBOOT_CMD_RESTART
	if cmd == "poweroff" {
		how = unix.LINUX_REBOOT_CMD_POWER_OFF
	}
	unix.Reboot(how)
	select {}
}

// console is the machine's screen and keyboard: the first virtual
// terminal, the serial port when the kernel was told to use it, and the
// console a hypervisor gives a virtual machine in place of a serial
// port (hvc0: Apple's Virtualization, Xen, a virtio console under KVM).
type console struct {
	mu    sync.Mutex
	outs  []io.Writer
	terms []*terminal
	in    chan key
}

// terminal is one device of the console.
type terminal struct {
	path   string
	f      *os.File
	paused atomic.Bool // a command line has the device; leave its keyboard alone
}

// key is a line typed on one of the console's terminals.
type key struct {
	text string
	from *terminal
}

func openConsole() *console {
	c := &console{in: make(chan key, 8)}
	devs := []string{"/dev/tty1"}
	// A serial port: ttyS0 on a PC, ttyAMA0 on an ARM machine.
	for _, f := range strings.Fields(readFile("/proc/cmdline")) {
		if v, ok := strings.CutPrefix(f, "console="); ok {
			if name, _, _ := strings.Cut(v, ","); (strings.HasPrefix(name, "tty") || strings.HasPrefix(name, "hvc")) && name != "tty0" {
				devs = append(devs, "/dev/"+name)
			}
		}
	}
	// It is there only on a machine that has one.
	devs = append(devs, "/dev/hvc0")
	if os.Getpid() != 1 {
		devs = []string{"/dev/tty"}
	}
	seen := map[string]bool{}
	for _, d := range devs {
		if seen[d] {
			continue
		}
		seen[d] = true
		f, err := os.OpenFile(d, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		t := &terminal{path: d, f: f}
		c.outs = append(c.outs, f)
		c.terms = append(c.terms, t)
		go t.read(c.in)
	}
	if len(c.outs) == 0 {
		c.outs = []io.Writer{os.Stdout}
	}
	return c
}

// read passes on the lines typed on the terminal. It reads in short
// turns where the device allows, so that it can stand back while a
// command line uses the device.
func (t *terminal) read(in chan<- key) {
	if t.f.SetReadDeadline(time.Now().Add(time.Second)) != nil {
		sc := bufio.NewScanner(t.f)
		for sc.Scan() {
			in <- key{strings.TrimSpace(sc.Text()), t}
		}
		return
	}
	var line []byte
	buf := make([]byte, 256)
	for {
		if t.paused.Load() {
			line = line[:0]
			time.Sleep(100 * time.Millisecond)
			continue
		}
		t.f.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, err := t.f.Read(buf)
		for _, b := range buf[:n] {
			if b != '\n' && b != '\r' {
				line = append(line, b)
				continue
			}
			in <- key{strings.TrimSpace(string(line)), t}
			line = line[:0]
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
	}
}

func (c *console) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", "\r\n")
	for _, o := range c.outs {
		io.WriteString(o, s)
	}
}

func (c *console) ask(prompt string) string {
	c.printf("%s", prompt)
	return (<-c.in).text
}

func askServer(c *console) string {
	for {
		v := strings.TrimRight(c.ask("Server address (for example https://backup.example.com:8443): "), "/")
		if v != "" && !strings.Contains(v, "://") {
			v = "https://" + v
		}
		if strings.HasPrefix(v, "https://") && len(v) > len("https://") {
			if !strings.Contains(v[len("https://"):], ":") {
				v += ":8443"
			}
			return v
		}
		c.printf("That is not an address the server can be reached at.\n")
	}
}

// screen draws the state of the recovery.
type screen struct {
	con     *console
	version string
	nw      *network
	mu      sync.Mutex
	state   State
	paused  bool
	last    string
}

func (u *screen) pause()  { u.mu.Lock(); u.paused = true; u.mu.Unlock() }
func (u *screen) resume() { u.mu.Lock(); u.paused, u.last = false, ""; u.mu.Unlock(); u.draw() }

func (u *screen) show(s State) {
	u.mu.Lock()
	u.state = s
	u.mu.Unlock()
	u.draw()
}

func (u *screen) draw() {
	u.mu.Lock()
	s, paused := u.state, u.paused
	u.mu.Unlock()
	if paused {
		return
	}
	var b strings.Builder
	line := strings.Repeat("-", 72)
	fmt.Fprintf(&b, "better-backup recovery %s\n%s\n", u.version, line)
	addrs := u.nw.addresses()
	if len(addrs) == 0 {
		fmt.Fprintf(&b, "Network:   no address yet (press N and Enter to set one by hand)\n")
	} else {
		fmt.Fprintf(&b, "Network:   %s\n", strings.Join(addrs, ", "))
	}
	fmt.Fprintf(&b, "Server:    %s\n", s.Server)
	if s.Fingerprint != "" {
		fmt.Fprintf(&b, "           %s\n", s.Fingerprint)
	}
	if s.Code != "" {
		fmt.Fprintf(&b, "\n    Recovery code:   %s\n\n", s.Code)
		if !s.Approved {
			fmt.Fprintf(&b, "Open the better-backup web page, choose \"Bare-metal recovery\" and approve\nthe machine that shows this code. Then choose the backup to restore.\n")
		}
	}
	fmt.Fprintf(&b, "\nStatus:    %s\n", s.Status)
	if s.Problem != "" {
		fmt.Fprintf(&b, "Problem:   %s\n", s.Problem)
	}
	fmt.Fprintf(&b, "%s\nType a letter and press Enter:  N network   S server address   C command line\n                                R restart   P power off\n", line)
	text := b.String()
	u.mu.Lock()
	same := text == u.last
	u.last = text
	u.mu.Unlock()
	if !same {
		u.con.printf("\x1b[2J\x1b[H%s> ", text)
	}
}

// keys handles the keyboard until the administrator asks for another
// server, which it reports by returning true.
func (u *screen) keys() (changeServer bool) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if os.Getpid() == 1 {
				u.nw.start() // adapters whose drivers were slow to load
			}
			u.draw() // the network may have come up meanwhile
			continue
		case k := <-u.con.in:
			key := k.text
			u.pause()
			switch strings.ToLower(key) {
			case "n":
				u.nw.configure(u.con)
			case "s":
				if strings.EqualFold(u.con.ask("Change the server address? Any restore in progress is abandoned. (y/N) "), "y") {
					return true
				}
			case "c":
				if os.Getpid() == 1 {
					shell(k.from)
				}
			case "r", "p":
				cmd := map[string]string{"r": "reboot", "p": "poweroff"}[strings.ToLower(key)]
				if strings.EqualFold(u.con.ask("Really "+map[string]string{"reboot": "restart", "poweroff": "switch off"}[cmd]+" the machine? (y/N) "), "y") {
					shutdown(cmd)
				}
			}
			u.resume()
		}
	}
}

// shell runs a command line where it was asked for and waits for it to
// end. From the screen that is the second virtual terminal, which it
// shows: the first stays with the recovery screen. From a serial port
// or a hypervisor's console it is that device itself, which the
// recovery screen leaves alone meanwhile.
func shell(from *terminal) {
	if from != nil && from.path != "/dev/tty1" && from.f.SetReadDeadline(time.Now().Add(time.Second)) == nil {
		from.paused.Store(true)
		defer from.paused.Store(false)
		time.Sleep(300 * time.Millisecond) // the read that was under way
		// The shell opens the device itself, as the terminal of a
		// session of its own, so that Ctrl-C works in it.
		cmd := exec.Command(busybox, "setsid", "-c", busybox, "sh", "-l")
		if f, err := os.OpenFile(from.path, os.O_RDWR, 0); err == nil {
			defer f.Close()
			cmd.Stdin, cmd.Stdout, cmd.Stderr = f, f, f
			cmd.Run()
			return
		}
	}
	exec.Command(busybox, "openvt", "-c", "2", "-f", "-s", "-w", "--", busybox, "sh", "-l").Run()
	exec.Command(busybox, "chvt", "1").Run()
}

// network keeps a DHCP client running on every network adapter.
type network struct {
	log   *slog.Logger
	mu    sync.Mutex
	procs map[string]*exec.Cmd
}

func interfaces() []string {
	var out []string
	entries, _ := os.ReadDir("/sys/class/net")
	for _, e := range entries {
		// Real adapters have a device behind them.
		if _, err := os.Stat("/sys/class/net/" + e.Name() + "/device"); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

func (n *network) start() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ifc := range interfaces() {
		if n.procs[ifc] != nil {
			continue
		}
		exec.Command(busybox, "ip", "link", "set", ifc, "up").Run()
		cmd := exec.Command(busybox, "udhcpc", "-f", "-i", ifc, "-s", "/etc/udhcpc.script", "-t", "5", "-T", "3", "-A", "10", "-S")
		if err := cmd.Start(); err != nil {
			n.log.Warn("starting the DHCP client", "interface", ifc, "err", err)
			continue
		}
		n.procs[ifc] = cmd
		go cmd.Wait()
	}
}

func (n *network) addresses() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifc.Name+" "+ipn.String())
			}
		}
	}
	return out
}

func (n *network) wait(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end) && len(n.addresses()) == 0; {
		time.Sleep(500 * time.Millisecond)
	}
}

// configure sets an address by hand, for networks without DHCP.
func (n *network) configure(c *console) {
	ifs := interfaces()
	if len(ifs) == 0 {
		c.printf("\nNo network adapter was found. The recovery disc may lack a driver for it.\n")
		c.ask("Press Enter to go back. ")
		return
	}
	n.start() // adapters that appeared since the start
	c.printf("\nNetwork adapters: %s\n", strings.Join(ifs, ", "))
	ifc := ifs[0]
	if len(ifs) > 1 {
		if v := c.ask("Adapter to configure [" + ifc + "]: "); v != "" {
			ifc = v
		}
	}
	addr := c.ask("Address with prefix length, such as 192.168.1.50/24 (empty: keep using DHCP): ")
	if addr == "" {
		return
	}
	if _, _, err := net.ParseCIDR(addr); err != nil {
		c.printf("That is not an address with a prefix length.\n")
		c.ask("Press Enter to go back. ")
		return
	}
	gw := c.ask("Gateway (empty: none): ")
	dns := c.ask("DNS server (empty: none): ")
	n.mu.Lock()
	if p := n.procs[ifc]; p != nil && p.Process != nil {
		p.Process.Kill()
	}
	n.procs[ifc] = &exec.Cmd{} // configured by hand: no DHCP client
	n.mu.Unlock()
	run := func(args ...string) {
		if out, err := exec.Command(busybox, args...).CombinedOutput(); err != nil {
			c.printf("%s: %s\n", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
	}
	run("ip", "link", "set", ifc, "up")
	run("ip", "addr", "flush", "dev", ifc)
	run("ip", "addr", "add", addr, "dev", ifc)
	if gw != "" {
		exec.Command(busybox, "ip", "route", "del", "default").Run()
		run("ip", "route", "add", "default", "via", gw, "dev", ifc)
	}
	if dns != "" {
		os.WriteFile("/etc/resolv.conf", []byte("nameserver "+dns+"\n"), 0o644)
	}
	c.ask("Done. Press Enter to go back. ")
}

// scanHardware describes the machine and the disks a restore can go to.
func scanHardware() *api.RecoveryHardware {
	hw := &api.RecoveryHardware{Firmware: disklayout.FirmwareBIOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Disks: []api.RecoveryDisk{}}
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		hw.Firmware = disklayout.FirmwareUEFI
	}
	hw.Product = strings.TrimSpace(readFile("/sys/class/dmi/id/sys_vendor") + " " + readFile("/sys/class/dmi/id/product_name"))
	for _, l := range strings.Split(readFile("/proc/meminfo"), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			hw.MemoryBytes = kb << 10
		}
	}
	entries, _ := os.ReadDir("/sys/block")
	for _, e := range entries {
		name := e.Name()
		skip := false
		for _, p := range []string{"loop", "ram", "sr", "zram", "fd", "dm-", "md", "nbd"} {
			skip = skip || strings.HasPrefix(name, p)
		}
		sectors, _ := strconv.ParseInt(readFile("/sys/block/"+name+"/size"), 10, 64)
		if skip || sectors == 0 {
			continue
		}
		d := api.RecoveryDisk{Path: "/dev/" + name, Size: sectors * 512,
			Model: strings.TrimSpace(readFile("/sys/block/"+name+"/device/vendor") + " " + readFile("/sys/block/"+name+"/device/model"))}
		d.SectorSize, _ = strconv.Atoi(readFile("/sys/block/" + name + "/queue/logical_block_size"))
		if f, err := os.Open(d.Path); err == nil {
			id := make([]byte, 64)
			if n, _ := f.ReadAt(id, 0x8000); n == len(id) && string(id[1:6]) == "CD001" && strings.TrimSpace(string(id[40:])) == VolumeID {
				d.Media = true
				d.Contents = "the recovery disc"
			} else if layout, err := disklayout.Read(f, d.Size, d.SectorSize); err == nil {
				for i := range layout.Partitions {
					p := &layout.Partitions[i]
					p.FS, p.Label = disklayout.Sniff(io.NewSectionReader(f, p.Offset, p.Size))
				}
				d.Contents = layout.Summary()
			} else {
				d.Contents = "empty"
			}
			f.Close()
		}
		hw.Disks = append(hw.Disks, d)
	}
	sort.Slice(hw.Disks, func(i, j int) bool { return hw.Disks[i].Path < hw.Disks[j].Path })
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		nic := api.RecoveryNIC{Name: ifc.Name, MAC: ifc.HardwareAddr.String()}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				nic.Addrs = append(nic.Addrs, ipn.String())
			}
		}
		hw.NICs = append(hw.NICs, nic)
	}
	return hw
}

// loadDrivers loads the kernel modules for the hardware that is there,
// the way udev would: every device says what it is in a "modalias", and
// the modules list the aliases they serve. Loading a driver can reveal
// more devices (a disk behind a controller), so it goes round a few
// times.
func loadDrivers(log *slog.Logger) int {
	var uts unix.Utsname
	unix.Uname(&uts)
	dir := "/lib/modules/" + unix.ByteSliceToString(uts.Release[:])
	deps := map[string][]string{} // module name -> its file, then the files it needs
	if f, err := os.Open(dir + "/modules.dep"); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			file, rest, ok := strings.Cut(sc.Text(), ":")
			if ok {
				deps[moduleName(file)] = append([]string{file}, strings.Fields(rest)...)
			}
		}
		f.Close()
	}
	type alias struct{ pattern, module string }
	var aliases []alias
	if f, err := os.Open(dir + "/modules.alias"); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if p := strings.Fields(sc.Text()); len(p) == 3 && p[0] == "alias" {
				aliases = append(aliases, alias{p[1], strings.ReplaceAll(p[2], "-", "_")})
			}
		}
		f.Close()
	}
	loaded := map[string]bool{}
	var load func(name string) bool
	load = func(name string) bool {
		name = strings.ReplaceAll(name, "-", "_")
		files, ok := deps[name]
		if !ok || loaded[name] {
			return false
		}
		loaded[name] = true
		for _, dep := range files[1:] {
			load(moduleName(dep))
		}
		f, err := os.Open(dir + "/" + files[0])
		if err != nil {
			return false
		}
		defer f.Close()
		if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil && err != unix.EEXIST {
			log.Debug("loading a module", "module", name, "err", err)
			return false
		}
		return true
	}
	// Things no device asks for by alias.
	for _, m := range strings.Fields(readFile(dir + "/modules.load")) {
		load(m)
	}
	total := 0
	seen := map[string]bool{}
	for round := 0; round < 8; round++ {
		n := 0
		filepath.WalkDir("/sys/devices", func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "modalias" || seen[path] {
				return nil
			}
			seen[path] = true
			ma := readFile(path)
			if ma == "" {
				return nil
			}
			for _, a := range aliases {
				if !loaded[a.module] && glob(a.pattern, ma) && load(a.module) {
					n++
				}
			}
			return nil
		})
		total += n
		if n == 0 && round > 0 {
			break
		}
		time.Sleep(700 * time.Millisecond) // let new devices show up
	}
	return total
}

func moduleName(file string) string {
	base := filepath.Base(file)
	if i := strings.Index(base, ".ko"); i >= 0 {
		base = base[:i]
	}
	return strings.ReplaceAll(base, "-", "_")
}

// glob matches a module alias pattern: * for any run of characters, ?
// for one, and [..] for one of a set.
func glob(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if glob(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
		case '[':
			end := strings.IndexByte(pattern, ']')
			if end < 0 || len(s) == 0 {
				return false
			}
			set, ok := pattern[1:end], false
			neg := strings.HasPrefix(set, "!") || strings.HasPrefix(set, "^")
			if neg {
				set = set[1:]
			}
			for i := 0; i < len(set); i++ {
				if i+2 < len(set) && set[i+1] == '-' {
					ok = ok || (set[i] <= s[0] && s[0] <= set[i+2])
					i += 2
				} else {
					ok = ok || set[i] == s[0]
				}
			}
			if ok == neg {
				return false
			}
			pattern, s = pattern[end+1:], s[1:]
			continue
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
		}
		pattern, s = pattern[1:], s[1:]
	}
	return len(s) == 0
}
