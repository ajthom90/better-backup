#!/usr/bin/env python3
"""Builds the better-backup recovery disc: a small Linux system that
starts on BIOS and UEFI machines (with Secure Boot), runs bb-agent as
its first process and restores a whole machine onto blank disks.

It needs no root. On Ubuntu 24.04 install the tools with:

    sudo apt-get install xorriso mtools grub-pc-bin grub-common \
        grub-efi-amd64-signed shim-signed kmod zstd busybox-static

Usage:  build.py --agent bin/bb-agent --out recovery.iso [--firmware]

The kernel, its drivers and the boot loaders are Ubuntu's own signed
packages, downloaded from the Ubuntu archive and checked against apt's
signed package lists. Nothing is compiled.
"""
import argparse
import glob
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request

PLACEHOLDER = "@@BB-RECOVERY-CONFIG@@"
CONFIG_SIZE = 384
VOLUME_ID = "BB_RECOVERY"

# Kernel module directories worth having on a machine whose only job is
# to reach the network and write to its disks. Whatever these modules
# depend on is added from the rest of the kernel.
WANTED = [
    "drivers/ata", "drivers/scsi", "drivers/nvme", "drivers/block", "drivers/message",
    "drivers/hv", "drivers/virtio", "drivers/xen", "drivers/vmw_vmci",
    "drivers/net/ethernet", "drivers/net/hyperv", "drivers/net/vmxnet3", "drivers/net/xen-netfront.ko",
    "drivers/net/virtio_net.ko", "drivers/net/phy", "drivers/net/mdio", "drivers/net/usb",
    "drivers/usb/host", "drivers/usb/storage", "drivers/hid", "drivers/input/serio", "drivers/input/keyboard",
    "drivers/video/fbdev/hyperv_fb.ko", "drivers/gpu/drm/hyperv", "drivers/pci/controller",
    "drivers/mmc/host", "drivers/mmc/core", "drivers/md/dm-mod.ko",
]
# Left out even though they sit in the directories above: large, and of
# no use for a restore.
UNWANTED = ["drivers/scsi/qla4xxx", "drivers/scsi/lpfc", "drivers/scsi/bfa", "drivers/scsi/cxgbi", "drivers/scsi/csiostor",
            "drivers/net/ethernet/sfc/siena", "drivers/net/ethernet/netronome", "drivers/net/ethernet/chelsio",
            "drivers/net/ethernet/cavium/liquidio", "drivers/net/ethernet/mellanox/mlxsw", "drivers/net/ethernet/marvell/octeon",
            "drivers/net/ethernet/microchip/sparx5", "drivers/net/ethernet/mscc", "drivers/hid/intel-ish-hid",
            "drivers/net/usb/hso", "drivers/input/keyboard/applespi"]
# Firmware some common server network and storage adapters cannot work
# without.
FIRMWARE = ["bnx2", "bnx2x", "tigon", "e100", "rtl_nic", "qed", "ql2*", "cxgb3", "ene-ub6250", "intel/ice/ddp"]

UDHCPC = r"""#!/bin/sh
# Applies what the DHCP client learned.
case "$1" in
deconfig)
	ip addr flush dev "$interface"
	ip link set "$interface" up
	;;
bound|renew)
	ip addr flush dev "$interface"
	ip addr add "$ip/${mask:-24}" dev "$interface"
	if [ -n "$router" ]; then
		ip route del default 2>/dev/null
		for r in $router; do ip route add default via "$r" dev "$interface" && break; done
	fi
	: > /etc/resolv.conf
	for d in $dns; do echo "nameserver $d" >> /etc/resolv.conf; done
	;;
esac
exit 0
"""

PROFILE = r"""export PATH=/bin:/sbin:/usr/bin:/usr/sbin
export PS1='recovery:\w# '
echo
echo 'better-backup recovery command line. Type "exit" to go back to the recovery screen.'
echo 'The log of the recovery program is in /run/recovery.log. Disks: cat /proc/partitions'
echo
"""


def run(*cmd, **kw):
    print("+", " ".join(cmd), flush=True)
    return subprocess.run(cmd, check=True, **kw)


def out(*cmd):
    return subprocess.run(cmd, check=True, stdout=subprocess.PIPE, text=True).stdout


# A download slower than this for half a minute is given up on and tried
# again, from the next place if there is one. Some mirrors crawl.
MIN_SPEED = 250 << 10
ARCHIVES = ["http://archive.ubuntu.com/ubuntu", "http://security.ubuntu.com/ubuntu"]


def download(url, dest, size):
    """Fetches url to dest, picking up where an earlier try stopped."""
    have = os.path.getsize(dest) if os.path.exists(dest) else 0
    req = urllib.request.Request(url, headers=dict({"User-Agent": "recovery-disc-build"}, **({"Range": "bytes=%d-" % have} if have else {})))
    with urllib.request.urlopen(req, timeout=30) as resp:
        if have and resp.status != 206:
            have = 0
        with open(dest, "ab" if have else "wb") as f:
            mark, got = time.monotonic(), 0
            while True:
                block = resp.read(1 << 20)
                if not block:
                    break
                f.write(block)
                got += len(block)
                if time.monotonic() - mark > 30:
                    if got < MIN_SPEED * 30:
                        raise OSError("too slow: %d kB/s" % (got / (time.monotonic() - mark) / 1024))
                    mark, got = time.monotonic(), 0
    if os.path.getsize(dest) != size:
        raise OSError("got %d of %d bytes" % (os.path.getsize(dest), size))


def fetch(pkg, cache):
    """Puts a package's .deb in cache, checked against the hash in apt's
    signed package lists."""
    url, name, size, digest = out("apt-get", "download", "--print-uris", pkg).split()[-4:]
    url, size, (algo, want) = url.strip("'"), int(size), digest.split(":")
    part = os.path.join(cache, name + ".part")
    urls = [a + url[url.index("/pool/"):] for a in ARCHIVES if "/pool/" in url]
    if url.startswith("http") and url not in urls:
        urls.append(url)
    last = None
    for attempt in range(6):
        for u in urls:
            print("+ fetch", u, flush=True)
            try:
                download(u, part, size)
            except Exception as e:  # the next place, or the next try
                print("  failed:", e, flush=True)
                last = e
                continue
            h = hashlib.new(algo.lower())
            with open(part, "rb") as f:
                for block in iter(lambda: f.read(1 << 20), b""):
                    h.update(block)
            if h.hexdigest() != want:
                os.remove(part)
                last = "wrong %s for %s" % (algo, name)
                print("  failed:", last, flush=True)
                continue
            os.rename(part, os.path.join(cache, name))
            return
        time.sleep(3)
    sys.exit("could not download %s: %s" % (pkg, last))


def cpio(root, dest):
    """Writes root as a newc cpio archive owned by root, with the device
    node the kernel wants for the first process's console. Done by hand
    because making that node in the file system would need root."""
    ino = 0

    def entry(f, name, mode, data=b"", rdev=(0, 0), nlink=1):
        nonlocal ino
        ino += 1
        n = name.encode() + b"\0"
        head = "070701" + "".join("%08X" % v for v in (
            ino, mode, 0, 0, nlink, 0, len(data), 0, 0, rdev[0], rdev[1], len(n), 0))
        f.write(head.encode() + n)
        f.write(b"\0" * (-(110 + len(n)) % 4))
        f.write(data)
        f.write(b"\0" * (-len(data) % 4))

    with open(dest, "wb") as f:
        entry(f, "dev", 0o040755, nlink=2)
        entry(f, "dev/console", 0o020600, rdev=(5, 1))
        for dirpath, dirs, files in os.walk(root):
            dirs.sort()
            rel = os.path.relpath(dirpath, root)
            if rel != ".":
                entry(f, rel, 0o040755, nlink=2)
            for name in sorted(files):
                path = os.path.join(dirpath, name)
                arc = name if rel == "." else rel + "/" + name
                if os.path.islink(path):
                    entry(f, arc, 0o120777, os.readlink(path).encode())
                else:
                    with open(path, "rb") as g:
                        entry(f, arc, 0o100000 | (0o755 if os.access(path, os.X_OK) else 0o644), g.read())
            for name in list(dirs):
                path = os.path.join(dirpath, name)
                if os.path.islink(path):
                    dirs.remove(name)
                    arc = name if rel == "." else rel + "/" + name
                    entry(f, arc, 0o120777, os.readlink(path).encode())
        entry(f, "TRAILER!!!", 0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--agent", required=True, help="a static linux/amd64 bb-agent binary")
    ap.add_argument("--out", required=True)
    ap.add_argument("--kernel", default="", help="kernel version, such as 6.8.0-151-generic (default: the distribution's current one)")
    ap.add_argument("--firmware", action="store_true", help="include firmware for adapters that need it (a larger download)")
    ap.add_argument("--cache", default="", help="directory to keep downloaded packages in")
    ap.add_argument("--version", default="dev")
    a = ap.parse_args()

    work = tempfile.mkdtemp(prefix="bb-recovery-")
    cache = os.path.abspath(a.cache) if a.cache else os.path.join(work, "dl")
    os.makedirs(cache, exist_ok=True)
    kver = a.kernel
    if not kver:
        for line in out("apt-cache", "depends", "linux-image-generic").splitlines():
            if "Depends: linux-image-" in line:
                kver = line.split("linux-image-", 1)[1].strip()
    if not kver:
        sys.exit("could not find the current kernel version; pass --kernel")
    print("kernel", kver)

    pkgs = ["linux-image-" + kver, "linux-modules-" + kver, "linux-modules-extra-" + kver]
    if a.firmware:
        # linux-firmware itself is an empty package that pulls in these.
        pkgs += ["linux-firmware-misc", "linux-firmware-qlogic", "linux-firmware-realtek", "linux-firmware-intel-misc"]
    tree = os.path.join(work, "pkg")
    for p in pkgs:
        if not glob.glob(os.path.join(cache, p + "_*.deb")):
            fetch(p, cache)
        run("dpkg-deb", "-x", glob.glob(os.path.join(cache, p + "_*.deb"))[0], tree)
    # Newer packages keep everything under /usr.
    def find(*parts):
        for base in (tree, os.path.join(tree, "usr")):
            p = os.path.join(base, *parts)
            if os.path.exists(p):
                return p
        sys.exit("not found in the kernel packages: " + "/".join(parts))

    moddir = find("lib", "modules", kver)
    run("depmod", "-b", os.path.dirname(os.path.dirname(os.path.dirname(moddir))), kver)
    deps = {}
    with open(os.path.join(moddir, "modules.dep")) as f:
        for line in f:
            mod, _, rest = line.partition(":")
            deps[mod] = rest.split()
    want = set()

    def add(mod):
        if mod not in want:
            want.add(mod)
            for d in deps.get(mod, []):
                add(d)

    for mod in deps:
        rel = mod[len("kernel/"):] if mod.startswith("kernel/") else mod
        if any(rel.startswith(w) for w in WANTED) and not any(rel.startswith(u) for u in UNWANTED):
            add(mod)
    print(len(want), "kernel modules")

    root = os.path.join(work, "root")
    for d in ("bin", "etc", "proc", "sys", "run", "tmp", "root", "lib/firmware"):
        os.makedirs(os.path.join(root, d))
    shutil.copy(a.agent, os.path.join(root, "init"))
    os.chmod(os.path.join(root, "init"), 0o755)
    bb = shutil.which("busybox") or "/bin/busybox"
    if b"statically linked" not in subprocess.run(["file", "-L", bb], stdout=subprocess.PIPE).stdout:
        sys.exit("busybox must be the static build (the busybox-static package)")
    shutil.copy(bb, os.path.join(root, "bin/busybox"))
    for applet in out(bb, "--list").split():
        if applet != "busybox":
            os.symlink("busybox", os.path.join(root, "bin", applet))
    for name, text, mode in (("etc/udhcpc.script", UDHCPC, 0o755), ("etc/profile", PROFILE, 0o644),
                             ("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n", 0o644), ("etc/group", "root:x:0:\n", 0o644),
                             ("etc/os-release", 'NAME="better-backup recovery"\nVERSION="%s"\n' % a.version, 0o644)):
        with open(os.path.join(root, name), "w") as f:
            f.write(text)
        os.chmod(os.path.join(root, name), mode)

    # Modules go in uncompressed: the archive is compressed as a whole,
    # and the recovery program loads them itself.
    dst = os.path.join(root, "lib/modules", kver)
    for mod in sorted(want):
        src = os.path.join(moddir, mod)
        to = os.path.join(dst, mod)
        os.makedirs(os.path.dirname(to), exist_ok=True)
        if mod.endswith(".zst"):
            subprocess.run(["zstd", "-dqf", src, "-o", to[:-4]], check=True)
        else:
            shutil.copy(src, to)
    for name in ("modules.order", "modules.builtin", "modules.builtin.modinfo"):
        if os.path.exists(os.path.join(moddir, name)):
            shutil.copy(os.path.join(moddir, name), dst)
    run("depmod", "-b", root, kver)
    for name in os.listdir(dst):  # the recovery program reads the text files only
        if name.endswith(".bin"):
            os.remove(os.path.join(dst, name))
    with open(os.path.join(dst, "modules.load"), "w") as f:
        f.write("hid_generic usbhid hv_vmbus dm_mod\n")
    if a.firmware:
        fw = find("lib", "firmware")
        for pat in FIRMWARE:
            if not glob.glob(os.path.join(fw, pat)):
                sys.exit("the firmware packages do not hold " + pat)
        for pat in FIRMWARE:
            for src in glob.glob(os.path.join(fw, pat)):
                to = os.path.join(root, "lib/firmware", os.path.relpath(src, fw))
                os.makedirs(os.path.dirname(to), exist_ok=True)
                if os.path.isdir(src):
                    shutil.copytree(src, to, symlinks=True)
                else:
                    shutil.copy(src, to, follow_symlinks=False)

    iso = os.path.join(work, "iso")
    os.makedirs(os.path.join(iso, "boot/grub/i386-pc"))
    os.makedirs(os.path.join(iso, ".disk"))
    shutil.copy(find("boot", "vmlinuz-" + kver), os.path.join(iso, "boot/vmlinuz"))
    os.chmod(os.path.join(iso, "boot/vmlinuz"), 0o644)
    cpio(root, os.path.join(work, "initrd.cpio"))
    run("zstd", "-q", "-19", "-T0", "-f", os.path.join(work, "initrd.cpio"), "-o", os.path.join(iso, "boot/initrd"))
    with open(os.path.join(iso, ".disk/info"), "w") as f:
        f.write("better-backup recovery %s\n" % a.version)
    # The server writes its address over the placeholder when the disc is
    # downloaded from its web page; until then the kernel ignores it.
    placeholder = PLACEHOLDER + "@" * (CONFIG_SIZE - len(PLACEHOLDER))
    with open(os.path.join(iso, "boot/grub/grub.cfg"), "w") as f:
        f.write("""set timeout=3
set default=0
menuentry "better-backup recovery %s" {
	linux /boot/vmlinuz quiet console=ttyS0,115200 console=tty0 %s
	initrd /boot/initrd
}
""" % (a.version, placeholder))

    # BIOS: GRUB's own image, for discs and for USB sticks.
    pc = "/usr/lib/grub/i386-pc"
    mods = ["biosdisk", "iso9660", "part_msdos", "part_gpt", "normal", "linux", "configfile", "search", "echo", "test", "serial", "all_video"]
    run("grub-mkimage", "-O", "i386-pc-eltorito", "-d", pc, "-p", "/boot/grub", "-o", os.path.join(iso, "boot/grub/i386-pc/eltorito.img"), *mods)
    for f in glob.glob(pc + "/*.mod") + glob.glob(pc + "/*.lst"):
        shutil.copy(f, os.path.join(iso, "boot/grub/i386-pc"))
    # UEFI: the signed shim and GRUB, in a small FAT image. GRUB finds
    # the disc by its /.disk/info file and reads /boot/grub/grub.cfg.
    efi = os.path.join(work, "efi.img")
    with open(efi, "wb") as f:
        f.truncate(10 << 20)
    run("mformat", "-i", efi, "-v", "BBEFI", "::")
    run("mmd", "-i", efi, "::/EFI", "::/EFI/BOOT")
    for src, name in (("/usr/lib/shim/shimx64.efi.signed.latest", "BOOTX64.EFI"), ("/usr/lib/grub/x86_64-efi-signed/gcdx64.efi.signed", "grubx64.efi"),
                      ("/usr/lib/shim/mmx64.efi", "mmx64.efi")):
        run("mcopy", "-i", efi, src, "::/EFI/BOOT/" + name)
        # Also as plain files, for USB sticks made by copying the disc's
        # files onto a FAT volume.
        os.makedirs(os.path.join(iso, "EFI/BOOT"), exist_ok=True)
        shutil.copy(src, os.path.join(iso, "EFI/BOOT", name))
    run("xorriso", "-as", "mkisofs", "-r", "-V", VOLUME_ID, "-o", a.out, "-J", "-joliet-long", "-l", "-iso-level", "3",
        "--grub2-mbr", pc + "/boot_hybrid.img", "-partition_offset", "16", "--mbr-force-bootable",
        "-append_partition", "2", "28732ac11ff8d211ba4b00a0c93ec93b", efi, "-appended_part_as_gpt",
        "-iso_mbr_part_type", "a2a0d0ebe5b9334487c068b6b72699c7",
        "-c", "/boot.catalog", "-b", "/boot/grub/i386-pc/eltorito.img", "-no-emul-boot", "-boot-load-size", "4", "-boot-info-table", "--grub2-boot-info",
        "-eltorito-alt-boot", "-e", "--interval:appended_partition_2:::", "-no-emul-boot", iso)
    with open(a.out, "rb") as f:
        if f.read().count(placeholder.encode()) != 1:
            sys.exit("the placeholder for the server's address is not in the image exactly once")
    shutil.rmtree(work)
    print("wrote", a.out, os.path.getsize(a.out) >> 20, "MiB")


if __name__ == "__main__":
    main()
