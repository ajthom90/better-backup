# better-backup

A fully open-source backup platform: one server that stores deduplicated,
compressed, encrypted backups on any disk you give it, and agents for Windows,
macOS and Linux. It is meant to replace a backup appliance without the
appliance's capacity ceiling.

**Status: Phase 2 in progress.** File backup and restore work end to end on
all three operating systems, and Windows backups read from VSS snapshots.
Newer parts, such as restoring a system state or Active Directory in place,
have had little testing on real machines; the sections below say which. Do not make this your only
backup until it has run beside your current one for a while.

## What works today

- **File backup** with content-defined chunking: after the first run only
  changed data is read and sent, and identical data is stored once across
  every machine.
- **Restore in place** over the original files, or to another folder or
  another machine. Only files that differ from the backup are rewritten.
- **VSS snapshots on Windows**: each backup reads a frozen shadow copy of the
  volume, so files that are open or locked are backed up, and VSS-aware
  applications flush their data first. NTFS owners, permissions and
  alternate data streams are backed up and restored.
- **Volume images**: back up a whole volume block by block (`C:`, or a block
  device on Linux) and restore it over a volume, onto another disk, or into
  an image file. Windows volumes are read from a VSS snapshot and free space
  on NTFS is skipped.
- **ZFS snapshots, and TrueNAS SCALE**: on a machine with ZFS, files are read
  from snapshots of their datasets and ZFS volumes from a snapshot too, all
  taken at one instant. The agent runs on TrueNAS SCALE as an app, and also
  backs up the system's configuration.
- **Windows system state**: the registry, boot and system files, and on a
  domain controller the Active Directory database and SYSVOL, as the
  system's own VSS writers list them. It can be restored to a folder, or
  over the machine it came from, including Active Directory on a domain
  controller in Directory Services Restore Mode.
- **Bare-metal recovery**: a policy can back up the whole machine (the disk
  layout and every partition, including the hidden boot partitions). Start a
  blank machine or a new virtual machine from the recovery disc, approve it
  in the web UI, and the backup is put back onto its disks so that it starts
  like the original. A whole machine can also be restored as virtual disks,
  with a new Hyper-V machine built around them. Tested in emulators; a
  restored Windows server has not been started on real hardware yet.
- **Microsoft 365**: the mailboxes and OneDrive of a tenant, read through
  Microsoft Graph. After the first backup only new and changed messages and
  files are downloaded. They are restored into a folder, each message as an
  `.eml` file. Tested against a stand-in for Microsoft Graph, not yet
  against a real tenant.
- **Search and download**: find a file by name across every snapshot, and
  download a file, or a folder as a zip file, in the browser.
- **Agents for Windows, macOS and Linux**, enrolled with a one-time token and
  authenticated with per-agent TLS certificates. Agents dial out to the
  server, so only the server needs an open port.
- **Schedules and retention**: run every N hours or daily at a set time; keep
  the last N, daily, weekly, monthly and yearly snapshots.
- **Email alerts** when a backup, restore or repository check fails, and when
  a scheduled backup is missed, for example because a machine is switched off.
- **Encryption at rest** (AES-256-GCM) and zstd compression.
- **Repository maintenance**: garbage collection, and a full verify that reads
  and checks every stored chunk.
- **Offsite copy**: a second copy of every backup in S3-compatible storage
  (Amazon S3, Backblaze B2, Wasabi, MinIO) or on another disk or share,
  brought up to date every day. It is encrypted like the original.
- **Ransomware protection**: deleted snapshots can be brought back for a
  week, a retention lock, backups that look encrypted are noticed and the
  older ones kept, and the offsite copy can be locked with S3 Object Lock.
  See [Ransomware protection](#ransomware-protection).
- **Disaster recovery**: `bb-server restore` reads a repository directly and
  needs only the repository folder and its key.
- **Runs in Docker or natively** as a systemd unit, launchd daemon or Windows
  service. One static binary each for server, agent and CLI.
- **x64 and ARM64** on every system; see [Processors](#processors).

## Processors

Everything is built for Intel/AMD (x64) and for ARM64 machines:

| | x64 | ARM64 |
| --- | --- | --- |
| Windows: programs, server and agent installers (`.msi`) | yes | yes |
| Linux: programs, `.deb` and `.rpm` packages | yes | yes |
| macOS: programs, agent installer (`.pkg`) | yes | yes (one installer for both) |
| Docker images, server and agent | yes | yes (one tag for both) |
| Bare-metal recovery disc | yes | yes, for machines that start by UEFI |

Every commit is tested on both kinds of processor on Windows, Linux and
macOS, and the installers are installed and used on both.

What to know about ARM64:

- **Windows on ARM** needs Windows 11. Open-file snapshots (VSS) work as on
  x64. The agent's settings app shows its page with WebView2, which
  Windows 11 includes; without it the page opens in the browser instead.
  Install the ARM64 installer there, not the x64 one: Windows would run the
  x64 agent, but an emulated program is not expected to be able to take
  snapshots.
- **Hyper-V on ARM64** (Windows 11 on ARM can host ARM64 guests) goes
  through the same code as on x64, but nobody has run it there yet. The
  same holds for KVM, Proxmox VE and ZFS on ARM64 Linux.
- **Linux on ARM** means 64-bit (`uname -m` prints `aarch64`): a Raspberry
  Pi 4 or 5 with a 64-bit system, an ARM server or cloud machine. There is
  no build for 32-bit ARM systems.
- **The recovery disc for ARM64** starts ARM64 machines that start by UEFI
  the way a PC does: ARM servers, and ARM virtual machines (on Hyper-V,
  KVM, VMware Fusion, Parallels, UTM). It does not start a Raspberry Pi,
  and most ARM laptops (Snapdragon PCs) will not find their disk or
  network with it. A whole-machine backup of those can still be restored
  as files and disks, and as disk images to attach to a virtual machine.
  It has recovered an emulated ARM64 machine; nobody has used it on a real
  one yet.
- A server of either kind hands out agent installers for both.

## Quick start

### 1. Start the server

With Docker:

```sh
docker run -d --name better-backup -p 8443:8443 \
  -e BB_TLS_NAMES=backup.example.com \
  -v /srv/better-backup:/data \
  ghcr.io/ajthom90/better-backup:latest
```

Images are published to GitHub's registry: `:latest` and `:vX.Y.Z` for releases,
and `:edge` for the newest commit on `main`. Until the first release exists, use
`:edge`. The agent image is `ghcr.io/ajthom90/better-backup-agent`.

On Windows, run the server installer: see [Windows installers](#windows-installers).

Or natively (Linux, macOS, Windows), from a release archive:

```sh
sudo bb-server service install --tls-name backup.example.com
sudo bb-server service start
```

On first start the server creates its repository, an admin token and a
repository key, all in its data directory:

| OS | Default data directory |
| --- | --- |
| Linux | `/var/lib/better-backup` |
| macOS | `/Library/Application Support/better-backup` |
| Windows | `C:\ProgramData\better-backup` |
| Docker | `/data` |

Then open `https://THE-SERVER:8443/` in a browser. The first visit asks you
to create the administrator; nothing on the server's own disk is needed for
that. (A server that already has agents asks for the admin token from
`admin.json` as well, so that an upgrade never leaves it open to whoever
finds the page first.)

> **Download the repository key now.** The page reminds you until you have:
> "Download the key" saves `repo.key`. Backups are encrypted with it. If the
> key is lost, the backups cannot be read by anyone.

### Users, roles and the activity log

Under "Users" an administrator adds the people who may sign in:

| Role | May |
| --- | --- |
| Administrator | everything, including users, server settings and the repository key |
| Operator | run backups and restores, manage agents and policies, delete snapshots |
| Viewer | look only |

Passwords are stored as Argon2id hashes. Repeated wrong sign-ins for a name
are slowed down, a sign-in ends after 12 hours without use, and changing a
password ends that user's other sign-ins. The "Activity log" lists who
signed in and who changed or started what, for two years.

The admin token in `admin.json` still works, for the `bb` command line and as
the way back in if every administrator is locked out ("Use the admin token
instead" on the sign-in page). Replace it under "Settings".

"Settings" also holds the names and IP addresses the server's certificate
carries (the same as `--tls-name`, without a restart).

Use `--repo DIR` (or mount a volume at `/data/repo`) to put the repository on
a bigger disk or a NAS mount.

### 2. Enroll a machine

Each machine to back up runs the agent. Installing it and connecting it to
the server ("enrolling") takes one **enrollment code**, which carries the
server's address, a one-time token and the server's fingerprint. Get a code
from **Add an agent** in the web UI, or on the server (in Docker:
`docker exec better-backup bb token create`):

```sh
bb token create                                   # valid 24 hours, for one machine
bb token create --for https://backup.example.com:8443   # name the address agents use
```

The agent runs in the background from the moment it is installed, and waits
to be enrolled. What it backs up, and when, is then set on the server.

#### macOS

Download `better-backup-agent_VERSION_darwin_universal.pkg` from the
[releases](https://github.com/ajthom90/better-backup/releases) and open it. It
installs the agent (a launchd daemon that also runs when nobody is signed in),
the **Better Backup** app and the `bb-agent` command, for Intel and Apple
silicon Macs.

The app opens when the installer finishes. Paste the enrollment code into it
and press **Connect**. Afterwards it shows whether the Mac reaches the server
and how the last backups went, and has a **Back Up Now** button. Any
administrator of the Mac can use it.

macOS keeps Mail, Messages, Safari, Photos and other apps' data away from
programs until you allow it. The app says so and walks you through it: in
System Settings, Privacy & Security, **Full Disk Access**, add
`/usr/local/bin/bb-agent`. The agent restarts by itself once it has access.
Without it backups still run, and leave those folders out with a warning.

- Until the installer is signed with an Apple Developer ID, macOS refuses to
  open it by double-click: right-click it and choose **Open**, or allow it
  under System Settings, Privacy & Security, or install it from Terminal with
  `sudo installer -pkg FILE.pkg -target /`. An unsigned agent also has to be
  given Full Disk Access again after each update.
- Without questions (scripts, management tools): put a file `enroll.conf`
  beside the package holding `code=bb1...` and optionally `name=...`, then run
  `sudo installer -pkg FILE.pkg -target /`. Or use the script below.
- Remove it with `sudo bb-agent-uninstall`.

#### Linux

One line installs the `.deb` or `.rpm` package (or, on other systems, the
plain program as a service) and enrolls the machine:

```sh
curl -fsSL https://raw.githubusercontent.com/ajthom90/better-backup/main/packaging/install.sh | sudo sh -s -- CODE
```

The same line works on macOS. Or install the package yourself and enroll
after:

```sh
sudo apt install ./better-backup-agent_VERSION_linux_amd64.deb   # or: sudo dnf install ./...rpm
                                                                 # on ARM64: ..._linux_arm64.deb
sudo bb-agent enroll          # asks for the code; or: sudo bb-agent enroll CODE
```

To enroll without anyone at the keyboard, write `code=bb1...` (and optionally
`name=...`) to `/var/lib/better-backup-agent/enroll.conf`; the agent picks it
up within seconds, and deletes it.

#### On the machine, afterwards

```sh
sudo bb-agent status      # connected? what is it doing? how did the last backups go?
sudo bb-agent backup      # back this machine up now
sudo bb-agent unenroll    # forget the server
sudo bb-agent setup       # all of this on a web page, served to this machine only
```

On Linux these need root, or membership of a group named `better-backup` if
you create one (`sudo groupadd better-backup`, then restart the agent). On
macOS, administrators need no `sudo`. The desktop app also builds for Linux
(`make app`), but is not packaged there yet.

On Windows, run `bb-agent enroll CODE` from an elevated prompt, then
`bb-agent service install` and `bb-agent service start`.

### 3. Create a policy and back up

```sh
bb agents
bb policy add --name documents --agent my-laptop \
    --path /home --exclude '*.tmp' \
    --daily-at 02:00 --keep-daily 14 --keep-weekly 8 --keep-monthly 12
bb policy run documents --wait
```

### 4. Restore

```sh
bb snapshots
bb ls SNAPSHOT /home/alice
bb restore SNAPSHOT --wait                        # in place: put everything back
bb restore SNAPSHOT --path /home/alice/report.docx --wait
bb restore SNAPSHOT --target /tmp/restored --wait # to another folder
bb restore SNAPSHOT --agent other-machine --target /srv/restore --wait
```

Everything can be done in the web UI at `https://your-server:8443/`: add and
revoke agents, create and edit policies, start backups, browse a snapshot and
restore a file, a folder, a disk or all of it, rebuild a whole machine from
the recovery disc, follow jobs and read their
warnings, forget snapshots, verify the repository, reclaim space and set up
email alerts. The `bb` command line does the same things for scripts. Sign in with the name and
password you created on the first visit. Your browser will warn about the
certificate, because the server uses its own private certificate authority.

**Find a file.** The box above the list of snapshots finds files and folders
by name in every snapshot: part of the name, or a pattern such as `*.xlsx`.
Each match is shown as the newest snapshot that holds it has it, with how
many snapshots hold it, so a file that was deleted last week is found in the
last snapshot taken before that. From a match you can open its folder,
download it or restore it.

Searches are answered from an index of file names, so they take a fraction of
a second however many files and snapshots there are. The server builds the
index by itself and keeps it up to date in the background after each backup
and whenever snapshots are forgotten; backups do not wait for it. It is a
file of its own, `search.db` in the server's data folder, of roughly 130
bytes for each different file or folder ever backed up (about 700 MB for
five million files), and it does not grow with the number of snapshots. It
holds nothing that is not in the repository: if it is lost, or after the
server is recovered from the offsite copy, it is built again from the
snapshots, and until that is done searches read the snapshots themselves,
which is slower. **Settings**, **Search index** shows its state and has a
button to build it again. A search for only two letters cannot use the index
of names and reads all of them, which takes a few seconds with millions of
files.

**Download in the browser.** In Browse, and in the search results, an
operator or administrator can download a file as it is, or a folder as a zip
file, straight to their own computer, without an agent. The data comes from
the server, so nothing on the backed-up machine is touched. Each download is
written to the activity log. A disk image downloads as a raw image; links
inside a folder are left out of the zip file, and so are owners and
permissions, which a zip file cannot hold. For those, restore to a folder.

### Windows installers

Each release has two installers for Windows, each for x64 (`amd64` in the
file name) and for ARM64 (`arm64`):
`better-backup-server_VERSION_windows_amd64.msi` and
`better-backup-agent_VERSION_windows_amd64.msi`.

**The server installer** asks for the folder to keep the backups in and the
port (8443). It installs the server as the service "better-backup server",
opens the port in Windows Firewall, adds the `bb` command to the path and
puts "better-backup web UI" in the Start menu. The first visit to the web UI
asks you to create the administrator's name and password.

**The agent installer** asks for an enrollment code. Get one in the web UI:
"Add an agent" shows the installer to download (the server carries the
matching ones, for x64 and ARM64), the code, and reports the machine when it has registered.
The code holds the server's address, a one-time token and the server's
fingerprint. The agent runs as the service "better-backup agent" and
connects out to the server, so it needs no firewall rule.

Both install without dialogs, for scripts and Group Policy:

```bat
msiexec /i better-backup-server_1.2.0_windows_amd64.msi /qn REPO_DIR=D:\Backups PORT=8443
msiexec /i better-backup-agent_1.2.0_windows_amd64.msi /qn ENROLL_CODE=bb1.eyJz... AGENT_NAME=FILESERVER
```

| Property | Installer | Meaning |
| --- | --- | --- |
| `REPO_DIR` | server | Folder for the backups. Empty: `C:\ProgramData\better-backup\repo` |
| `PORT` | server | Port for agents and the web UI. Default 8443 |
| `ENROLL_CODE` | agent | Code from "Add an agent". Empty: install now, register later |
| `AGENT_NAME` | agent | Name shown on the server. Empty: the computer's name |

A code works once. For Group Policy, where one command installs many
machines, set "Machines this code is for" in the dialog; that code works for
a week.

Installing a newer version over an older one upgrades it and keeps the
settings; the server's are remembered in the registry and win over the
command line on an upgrade. Removing either program keeps its data: the
backups, `repo.key` and the server's database, and the agent's identity in
`C:\ProgramData\better-backup-agent`, so a reinstalled agent is the same
agent. The installers close both data folders to everyone but administrators
and SYSTEM. The services log to `server.log` and `agent.log` in those folders.

A server in Docker carries the Windows agent installers too, for x64 and
ARM64 (the `:edge` image carries the newest build, version `0.0.N`). To offer another installer
from any server, put it in the folder `installers` in the server's data
directory.

#### Unsigned installers and antivirus

The installers and programs are not code-signed yet, so Windows does not know
their publisher:

- A downloaded installer may be blocked. Right-click it, Properties, tick
  **Unblock**; or run `Unblock-File better-backup-agent_*.msi` in PowerShell.
- SmartScreen may say "Windows protected your PC". Choose **More info**, then
  **Run anyway**.
- Antivirus may distrust a new, unsigned program that reads every file and
  talks to the network, which is what a backup agent does. Exclude the folder
  `C:\Program Files\better-backup` and the processes `bb-agent.exe` and
  `bb-server.exe`; excluding `C:\ProgramData\better-backup-agent` and the
  server's backup folder from real-time scanning also keeps backups fast.

The installers are built by `packaging/windows/build.ps1` and are signed when
the release workflow has a code signing certificate (the secrets
`SIGN_PFX_BASE64` and `SIGN_PFX_PASSWORD`). Without one, Windows SmartScreen
warns about an unknown publisher on the first run.

### Whole volumes

```sh
bb policy add --name fileserver-d --agent fileserver --volume D: --daily-at 01:00
bb restore SNAPSHOT --wait                      # back over D:
bb restore SNAPSHOT --target E: --wait          # onto another volume
bb restore SNAPSHOT --target /srv/d.img --agent linux-box --wait
```

Restoring over a volume replaces everything on it. The volume must not be in
use: Windows refuses to lock a volume with open files, and the system volume
cannot be restored from the running system (see
[bare-metal recovery](#bare-metal-recovery)).
The target must be at least as large as the original volume.

### TrueNAS SCALE and other ZFS systems

On a machine with ZFS the agent reads from ZFS snapshots, as it reads from VSS
snapshots on Windows. When a backup starts it takes a snapshot of every
dataset that holds one of the policy's folders or is mounted below one, reads
the files from there, and removes the snapshots when it is done. Datasets of
one pool are frozen at the same instant. Nothing needs to be switched on, and
it works the same on Proxmox VE or any Linux with OpenZFS. Tick "Read live
files" in the policy (`--no-snapshot`) to go without.

A ZFS volume (zvol), such as the disk of a virtual machine or an iSCSI
extent, is backed up as an image. Give it as a volume, `/dev/zvol/tank/vm-disk`.
It is frozen together with the folders, and restoring it in place writes the
image back onto the volume.

**Installing on TrueNAS SCALE** (24.10 or newer). In the web UI choose "Add an
agent". Under "On TrueNAS SCALE" it shows a short text for a custom app, with
the enrollment token filled in. Change the folder to one on a pool of yours,
then in TrueNAS open Apps, Discover Apps, the menu at the top right, "Install
via YAML", and paste it. The NAS shows up under Agents a few seconds later.

The app is privileged and shares the host's process ids, and with
`BB_HOST_ROOT=/proc/1/root` the agent works in the file system of TrueNAS
itself rather than the container's. So policies name the real paths
(`/mnt/tank/documents`), snapshots are taken with the system's own `zfs`
command, and a restore in place writes to the datasets. Its identity is kept
in the folder you chose, so it survives TrueNAS updates.

**The TrueNAS configuration.** Tick "TrueNAS: back up the system
configuration" in the policy (`--truenas-config`). The backup then holds
`truenas-config.tar`: the settings database, the secret seed that unlocks the
passwords stored in it, and root's SSH keys, the same file TrueNAS makes
under Manage Configuration with "Export Password Secret Seed" ticked. To put
it back, restore "TrueNAS configuration" to a folder and upload the file in
TrueNAS under System, General Settings, Manage Configuration, Upload File.
After a rebuild that brings back the pools, shares, users and apps settings;
then restore the data.

**Permissions.** On Linux and macOS a backup keeps each file's extended
attributes beside its owner, group and mode, and a restore puts them back.
That covers POSIX ACLs, the NFSv4 ACLs TrueNAS uses for SMB shares, and what
Samba stores for Windows clients, such as the hidden and read-only flags.

### Hyper-V virtual machines

Install the agent on the Hyper-V host and name the machines to protect:

```sh
bb policy add --name vms --agent hyperv01 --vm web01 --vm sql01 --daily-at 23:00
bb policy add --name all-vms --agent hyperv01 --vm '*' --daily-at 23:00
bb restore SNAPSHOT --path web01 --target D:\Restored --name web01-restored --wait
bb restore SNAPSHOT --path 'web01\disk-scsi-0-0.vhdx' --target D:\Restored\disk.vhdx --wait
```

The web UI does the same: list the machines under "Virtual machines" when you
add a policy, and give the new machine a name in the restore dialog.

Each machine is backed up while it runs. The agent takes a checkpoint (a
production checkpoint if the machine is set to use them, so applications in
the guest are flushed), images every virtual disk as it was at that moment,
and removes the checkpoint. Empty parts of a disk are not stored.

A restore builds a new machine, switched off, with its disks under the target
folder and the same generation, processors, memory, disk positions, network
adapters and secure boot setting. Single disks can be restored on any agent.

**Over the original.** To roll a machine back, tick "Put it back over the
original virtual machine" in the restore dialog, or use `--replace`:

```sh
bb restore SNAPSHOT --path web01 --replace --wait
```

The disks from the backup are written first, into a new folder near the
machine's first disk (or under the target, if you give one); the job says where.
Only then is the machine that is there shut down (turned off if it has not
shut down after two minutes) and renamed to "web01 (replaced DATE TIME)", and
the restored one created under its name, switched off. Nothing is deleted: if
the restore fails the old machine gets its name back, and after a good one
you delete the old machine and its disks yourself once you are sure. Do not
run both, since they have the same network addresses. If no machine of that
name exists, a new one is made, which then needs a target. This also works
for KVM with libvirt, where it is tested against a real machine; on Hyper-V
it has not been run yet. It is not built for Proxmox VE or VMware.

Backup and restore have been run on a real Hyper-V host, but the automated
tests cover only the disk imaging underneath, because the test machines have
no Hyper-V.

By default every backup reads the disks in full and uploads only the blocks
that changed. With change tracking a backup also reads only what changed,
which is much faster:

```sh
bb policy add --name vms --agent hyperv01 --vm '*' --vm-tracking check --daily-at 23:00
```

`--vm-tracking on` (in the web UI: "Read only what changed since the last
backup") takes each backup from a Hyper-V recovery snapshot and leaves a
reference point behind, so Hyper-V tracks what is written to the disks
(Resilient Change Tracking, Windows Server 2016 and later). The next backup
asks each disk what changed and takes the rest from the previous backup. The
first backups after switching it on still read everything, and so does any
backup where the tracking cannot be used.

`--vm-tracking check` works the same way but reads every block anyway and
warns if a block the tracking called unchanged had changed. Change tracking
has not yet run on a real host, and a mistake in it would mean backups that
silently hold old data, so run with `check` for a while and switch to `on`
only if no such warning appears.

Known gaps: a machine cannot yet be restored over the original, and
pass-through disks, shared disks, virtual TPMs and other device settings are
not carried over.

### KVM virtual machines (libvirt)

Install the agent on the KVM host. It needs `virsh` and `qemu-utils`
(`qemu-img` on Fedora and RHEL), which a libvirt host normally has, and it
runs as root. The same commands and the same fields in the web UI apply:

```sh
bb policy add --name vms --agent kvm01 --vm web01 --vm db01 --vm-tracking check --daily-at 23:00
bb policy add --name all-vms --agent kvm01 --vm '*' --daily-at 23:00
bb restore SNAPSHOT --path web01 --target /var/lib/libvirt/images --name web01-restored --wait
bb restore SNAPSHOT --path web01/disk-vda --target /tmp/vda.img --wait
```

The agent works out by itself whether its host runs Hyper-V, Proxmox VE or
libvirt; a policy can also say so (`--hypervisor libvirt`, "Hypervisor" in
the web UI). VMware is different: see below.

A running machine is backed up through a libvirt backup job (libvirt 7.2 and
QEMU 4.2 or later): QEMU serves every disk as it was when the job began while
the machine keeps writing. If the guest runs the QEMU guest agent, its file
systems are flushed and frozen for that moment; otherwise the backup holds
what a power cut would leave. A machine that is switched off is read from its
disk files, and cannot be started until that backup has finished. Parts of a
disk that hold no data are neither read nor stored. The machine's definition
and, for UEFI machines, its firmware settings are stored with the disks.

`--vm-tracking on` makes a libvirt checkpoint with every backup, so the next
one asks QEMU which parts of each disk were written since and reads only
those. Only qcow2 disks can be tracked; raw files and block devices are read
in full every time, as is every disk after the machine was backed up while
switched off. As with Hyper-V, run with `--vm-tracking check` first.

A restore defines a new machine, switched off, from the saved definition, so
every device setting comes back. Its disks are written as qcow2 files into a
folder named after the machine under the target, whatever they were before.
If the original machine is gone the new one keeps its identity (UUID);
beside the original it gets a new one but the same network (MAC) addresses.

The automated tests run all of this against a real libvirt and QEMU on every
change: full, tracked and switched-off backups of a machine with qcow2 and
raw disks and UEFI firmware, and restores that are compared with the
originals. Not covered by them, and so untested: guests with the guest agent,
disks on block devices (LVM, for which the scratch file goes to
`/var/lib/libvirt/images` or `BB_SCRATCH_DIR`), hosts with SELinux, and
real workloads writing during a backup.

Known gaps: network disks (Ceph RBD, iSCSI), pass-through and shared disks
are skipped with a warning, the contents of an emulated TPM are not backed
up, and a machine cannot yet be restored over the original.

### Proxmox VE virtual machines

Install the agent on each Proxmox VE node (the Linux package, running as
root). It uses the node's own tools and needs nothing else installed. The
commands and the web UI are the same as for the other hypervisors; a machine
is named by its name or by its id:

```sh
bb policy add --name vms --agent pve01 --vm web01 --vm 105 --vm-tracking check --daily-at 23:00
bb policy add --name all-vms --agent pve01 --vm '*' --daily-at 23:00
bb restore SNAPSHOT --path web01 --name web01-restored --wait
bb restore SNAPSHOT --path web01 --target local-lvm --name web01-restored --wait
bb restore SNAPSHOT --path web01/disk-scsi0 --target /tmp/scsi0.img --wait
```

A policy covers the machines of the node its agent is on, so a cluster needs
an agent and a policy per node; a machine that migrates to another node is
picked up by that node's `'*'` policy and read in full the first time.
Containers (LXC) are not covered.

A running machine is backed up through its QEMU, the way Proxmox's own
backup does it but with standard QEMU commands: a backup job keeps the old
contents of whatever the machine overwrites, so every disk is read as it was
at one instant, on any kind of storage. What the machine overwrites during
the backup is kept in `/var/tmp` until the backup ends (set
`BB_SCRATCH_DIR` in the agent's environment to use another folder); a busy
machine needs room there. If the guest agent is enabled for the machine, its
file systems are flushed and frozen for that instant. A machine that is
switched off is read from its volumes. Disks marked "no backup" in Proxmox
are left out, as Proxmox's own backup leaves them out.

`--vm-tracking on` keeps a dirty bitmap in the running QEMU, like Proxmox
Backup Server does, so the next backup reads only what was written since.
It works on every kind of storage. The record lives in QEMU's memory: after
a machine is stopped and started, or migrated, its next backup reads the
disks in full again (and still stores only what changed).

A restore creates a new machine, switched off, with the saved settings and
its disks as new volumes on the storage named as the target, or with no
target on the storages they were on. The machine gets its old id if that is
free again and the next free id otherwise. It keeps its network (MAC)
addresses and its SMBIOS identity and gets a new VM generation id, as with
Proxmox's own restore.

**This has not run on a real Proxmox VE node.** The automated tests run a
real QEMU set up the way Proxmox sets its machines up, so reading running
and stopped machines, the change tracking and the restored disks are tested
for real on every change. Proxmox's own tools (`qm`, `pvesm`, `pvesh`) are
replaced by stand-ins in those tests, so whether the real tools accept every
call, LVM volume activation, Ceph storage, and Proxmox VE 9's new disk
naming are untested. Try it on a machine that does not matter first, and
check a restore.

Known gaps: the contents of a TPM are not backed up, disks passed through
from the host are skipped with a warning, cloud-init passwords have to be set
again, and a machine cannot yet be restored over the original.

### VMware vSphere virtual machines (ESXi and vCenter)

VMware is the one hypervisor that is backed up over the network: no agent
goes on the ESXi host. Tell the server about the vCenter Server or the ESXi
host ("VMware servers" in the web UI, or `bb vmware add`), then make a policy
that names it and any agent that can reach its management address on port
443. With a vCenter Server the disks come from the ESXi hosts themselves, so
the agent has to reach those on port 443 as well. The agent reads the disks
and sends them on, so pick one close to the hosts.

```sh
BB_VMWARE_PASSWORD=... bb vmware add --name lab --url vcenter.example.com --user backup@vsphere.local
bb policy add --name vmware --agent files01 --hypervisor vmware --vmware-server lab --vm web01 --vm db01 --daily-at 23:00
bb restore SNAPSHOT --path web01 --name web01-restored --wait
bb restore SNAPSHOT --path web01 --name web01-restored --target datastore2 --vmware-server lab --wait
```

Most servers present a certificate they made themselves. Leave the
thumbprint empty at first: the first backup then fails with the SHA-256
thumbprint the server shows. Compare it with what the server itself displays
and save it with the server (`--thumbprint`), and from then on only that
certificate is accepted. The password is stored on the better-backup server
and handed to the agent with each job; it is not written to the agent's disk
and the API never returns it.

A backup takes a snapshot of the machine (quiesced through VMware Tools when
they are running), downloads the snapshot's disks the way "Export OVF
template" does, and removes the snapshot. A restore imports the machine again
as a new one, switched off, with thin disks, on the datastore named as the
target or else the one it was on, on a host that has that datastore, in its
old folder and resource pool if they are still there. Its network adapters
are connected to the networks of the same names and get new MAC addresses.

**This has not run on a real ESXi host or vCenter Server.** The automated
tests run against the vSphere simulator of VMware's own Go library, which
answers the management calls but holds no real disks, and the disk format is
checked both ways against QEMU. What that leaves untested: whether a real
host accepts the disks this program uploads on restore, what a real server
puts in a machine's description, vSAN and other special datastores, and
speed. Try it on a machine that does not matter first, and check a restore.

Known gaps: every backup downloads the disks in full (only what changed is
stored, but Changed Block Tracking is not used yet, so backups of large
machines are slow); the free ESXi license does not allow backups at all;
independent disks and physical-mode raw device mappings are left out with a
warning; encrypted machines and virtual TPMs cannot be exported; and a
machine cannot yet be restored over the original.

### Microsoft 365 (mailboxes and OneDrive)

better-backup reads a tenant as an application that you register in it and
allow to read mail and files. It cannot change or delete anything there.

Register the app once, in the Microsoft Entra admin center
(<https://entra.microsoft.com>), as an administrator:

1. **App registrations**, **New registration**. Name it, for example
   `better-backup`. Leave **Accounts in this organizational directory only**
   selected and the redirect address empty. **Register**.
2. From the overview, note the **Application (client) ID** and the
   **Directory (tenant) ID**.
3. **API permissions**, **Add a permission**, **Microsoft Graph**,
   **Application permissions**: tick `User.Read.All`, `Mail.Read` and
   `Files.Read.All`. Then **Grant admin consent**.
4. **Certificates & secrets**, **New client secret**. Copy its **Value** (not
   the Secret ID); it is shown only once. Microsoft lets a secret last two
   years at most. When it runs out, backups fail and say so: create a new
   one and paste it over the old.

In the web UI choose **Microsoft 365** at the top, **Add a tenant**, and
fill in the three values. **Test the connection** signs in and counts the
users. The same steps are shown there.

Then create a policy and, under **Microsoft 365**, choose the tenant, mail,
OneDrive or both, and the users: `*` for every user who has a licence and is
not a guest, or sign-in names, one per line. A shared mailbox has no
licence, so name it by its address. The machine the policy names only runs
the backup: any agent that reaches the internet will do, for example one on
the backup server itself.

What a snapshot holds:

```
Microsoft 365/
  alice@example.com/
    Mail/Inbox/2026-11-20 0930 Quarterly numbers [ca0df2c9].eml
    Mail/Inbox/Projects/...
    OneDrive/Reports/2026.docx
```

A message is stored once and not downloaded again; a draft is downloaded
again when it changes. A OneDrive file is downloaded again when its content
changes. If Microsoft refuses or fails for one message, file or folder, the
backup keeps what the last snapshot held of it and finishes with a warning,
so a passing failure does not make data vanish from the newest snapshot.

Restore from **Browse** on a snapshot, into a folder on any agent. `.eml`
files open in Outlook and other mail programs.

Not built yet: restoring straight back into a mailbox or OneDrive; calendars,
contacts, SharePoint sites and Teams; the read and flagged marks of
messages; older versions of OneDrive files; the "recoverable items" of a
mailbox. Every backup lists each mailbox folder and OneDrive folder again,
which on very large tenants takes a while.

### Windows system state

```sh
bb policy add --name dc01-state --agent dc01 --system-state --daily-at 00:30
bb restore SNAPSHOT --path "System state" --target D:\Recovered --wait
```

Or tick "Windows: back up the system state" on a policy in the web UI. The
agent asks the VSS writers on the machine which files make up its state (on a
domain controller that includes `ntds.dit`, its logs and SYSVOL), and reads
them from a snapshot. The first run stores several gigabytes of Windows
files; later runs only store what changed. In a snapshot the files sit under
"System state", then the drive letter, then their original path.

#### Restoring a system state over its machine

```sh
bb restore SNAPSHOT --system-state-in-place --wait
```

In the web UI: Restore on the snapshot, then "Put the Windows system state
back over this machine". It only works on the machine the backup came from.

**This has run only on test machines so far. Try it on a machine you can
afford to lose, and take a checkpoint or image first.** Microsoft does not
support replacing the registry of a running Windows Server 2016 or later, and
this does it anyway, the way backup programs did before; `--no-registry`
leaves the registry alone.

What happens:

- Files that are not in use are replaced at once. Files in use are written
  beside the originals and Windows swaps them in at the next restart.
- The registry hives are restored to `System32\config\bb-restore`, then
  made the machine's hives for the next start. Before that, the restored
  SYSTEM hive is given what the running one says a restore must keep (the
  `KeysNotToRestore` list: disk and volume assignments, drivers and services
  installed since the backup, and the list of files to swap in).
- System state backups made before this feature existed lack the registry's
  log files, and Windows may refuse to load their hives. The job then says
  the registry was not restored. Take a new system state backup first.
- Nothing is final until Windows restarts. `--restart` does that when the
  restore is done; otherwise the job ends with a reminder.

#### Restoring Active Directory on a domain controller

Active Directory cannot be replaced while it runs, so the domain controller
must be in Directory Services Restore Mode (DSRM). The agent, installed as a
service, starts there too.

1. On the domain controller, as an administrator: `bcdedit /set safeboot dsrepair`,
   then restart. Sign in as `.\Administrator` with the DSRM password. In DSRM
   the machine may not be able to look up names, so if the agent reaches the
   server by name, make sure that name still resolves (a hosts file entry
   works).
2. Start the restore from the web UI or with
   `bb restore SNAPSHOT --system-state-in-place --restart --wait`.
3. The restart leaves DSRM (the restore runs `bcdedit /deletevalue safeboot`
   when it restarts the machine; do that yourself if you restart by hand).

That is a **non-authoritative** restore: the database is put back, Active
Directory is told it came from a backup, and after the restart the domain
controller catches up from the others. SYSVOL does the same. It is the right
choice for a single failed domain controller.

For an **authoritative** restore, which makes the restored objects win over
the newer ones on the other domain controllers (the way to bring back
deleted users or OUs), name what to bring back:

```sh
bb restore SNAPSHOT --system-state-in-place --authoritative "OU=Sales,DC=example,DC=com" --restart --wait
```

After the files are back, the agent runs `ntdsutil "authoritative restore"`
for each name. `--authoritative-sysvol` does the same for SYSVOL (group
policies and scripts): use it only when every domain controller lost SYSVOL.
A backup older than the forest's tombstone lifetime (180 days by default,
60 on old forests) must not be restored.

### Bare-metal recovery

Bare-metal recovery rebuilds a whole machine on blank hardware, or in a new
virtual machine, when the original is gone.

**Back up.** Tick "Back up the whole machine" on the machine's policy in the
web UI, or:

```sh
bb policy add --name fs01-machine --agent fs01 --whole-machine --daily-at 01:00
```

This saves the partition table of the disk the system starts from and an
image of every partition on it: the EFI system partition, the recovery
partition and the Windows or Linux volumes. On Windows the volumes are read
from a VSS snapshot and free space on NTFS is skipped. Other disks are added
with `--machine-disk` (or the field under the tick box): disk numbers on
Windows, such as `0` and `1`; devices on Linux, such as `/dev/sda`. Data that
is already backed up as files is stored once, so a machine backup next to a
file backup costs little extra space.

**Recover.**

1. In the web UI choose "Bare-metal recovery" and download the recovery disc.
   The server writes its own address and fingerprint into the image, so check
   the address shown is one the machine can reach. The Docker image carries
   the disc. Other installs download `better-backup-recovery.iso` from the
   releases page once and upload it on the same page. For an ARM64 machine
   use the second button, "Download the disc for ARM64 machines"
   (`better-backup-recovery-arm64.iso`); see [Processors](#processors) for
   the machines it starts.
2. Start the machine from the disc: attach it to a virtual machine as a DVD,
   or write it to a USB stick (`dd`, Rufus in DD mode, balenaEtcher). It
   starts on BIOS and UEFI machines. It gets its address by DHCP; an address
   can be typed in on its console.
3. The machine shows a six-character recovery code and appears on the
   "Bare-metal recovery" page. Approve it there if the codes match.
4. Choose "Restore a backup onto it", pick the backup and which of the
   machine's disks each disk of the backup goes onto, and start. Everything
   on the chosen disks is erased.
5. When the restore is finished, remove the disc and restart the machine from
   the same page.

The partition tables are written back with their identifiers, so Windows
finds its boot files, drive letters and recovery partition as before.

**As a virtual machine, without the disc.** Restore the "Whole machine" entry
of a snapshot into a folder to get the disks as files (`.vhdx` on a Windows
agent, `.img` elsewhere). Give a machine name as well, on an agent that runs
on a Hyper-V host, and a new virtual machine is built around the disks,
switched off and not connected to the network:

```sh
bb restore SNAPSHOT --path "Whole machine" --agent hyperv01 --target D:\Recovered --name fs01-recovered --wait
```

Things to know:

- The target disk must be at least as large as the original and have the
  same sector size (512 bytes on nearly every disk and on every virtual disk
  made here). Extra space is left unused.
- The machine must start the way the original did: UEFI (Hyper-V generation
  2) or BIOS (generation 1). The page warns when they differ.
- With Secure Boot on, the recovery disc needs the "Microsoft UEFI
  Certificate Authority" to be trusted. On a Hyper-V generation 2 machine
  choose that template under Security while recovering, then switch back to
  "Microsoft Windows" before starting the restored Windows. Physical machines
  trust it as delivered.
- The recovery disc needs about 1.5 GB of memory.
- The machine must have the same kind of processor as the original: x64 or
  ARM64. The page warns when they differ.
- Windows restored onto very different hardware may lack the driver for its
  new disk controller. Recovering into a virtual machine of the same kind, or
  onto similar hardware, avoids this. Injecting drivers is not built.
- On Linux the partitions are read while in use, without a snapshot, so a
  busy system can leave its file systems needing a check at first start.
  LVM and software RAID members are copied as they are.
- BitLocker volumes, dynamic disks and Storage Spaces are untested.
- A recovery code only lets a machine ask; it receives nothing until an
  administrator approves it. An approved machine may read backups until it
  restarts or has been silent for two minutes.

### Health, alerts and reports

The top of the web UI answers "is everything protected?": a verdict, a list
of what needs attention (a failed or overdue backup, a machine without a
policy, a nearly full disk, a failing or missing offsite copy, a repository
key nobody has downloaded), and for every policy its state, last good
backup, size and what it added in the last seven days. Under "Repository" it
shows how fast the repository grows and how long the disk will last.

A job that is waiting or running can be stopped with "Cancel"; an agent
stops within about ten seconds. Reclaiming space and checking data cannot be
stopped once they have started.

The server reads and checks all stored data every 30 days, and warns when
less than 10% of the repository's disk is free. Change both under
"Settings".

Under "Alerts and reports" you can also have a summary of all backups
emailed every day or every Monday, and give a webhook address (Slack,
Microsoft Teams, Discord, Mattermost) that receives every alert and summary,
with or without email.

Set email up under "Alerts and reports" in the web UI, or from the command line:

```sh
export BB_SMTP_PASSWORD='...'    # only if the mail server needs a sign-in
bb alerts set --smtp smtp.example.com:587 --smtp-user backups@example.com \
    --from backups@example.com --to admin@example.com
bb alerts test
```

The server emails when a job fails and when a scheduled policy goes one and a
half schedule periods without a successful backup (36 hours for a daily
policy). Add `--on-warning` to also hear about backups that finish with
warnings, such as skipped files. The SMTP password is kept in the server's
database in the data directory, so protect that directory like `repo.key`.

### Offsite copy

Set it up under "Offsite copy" in the web UI, or from the command line:

```sh
export BB_S3_SECRET_KEY='...'
bb offsite set --s3-endpoint https://s3.us-west-004.backblazeb2.com \
    --s3-bucket my-backups --s3-access-key 004abc... --daily-at 03:00
bb offsite test
bb offsite run --wait      # the first copy sends everything
```

For a second disk or a mounted share use `bb offsite set --folder /mnt/offsite`
instead. The folder must exist: if the disk is not connected the copy fails,
and you get an email if alerts are on, rather than filling the wrong disk.

Each run sends the pack files and snapshots the copy lacks and removes the
ones that were forgotten and reclaimed, so the copy matches the repository. It
runs alongside backups. If most of the repository has disappeared, which
looks like a dead disk rather than a clean-up, nothing is removed from the
copy.

The copy is as unreadable without `repo.key` as the repository is, and the key
is not part of it. **Keep `repo.key` somewhere other than the server**, or
the offsite copy cannot be used after the server is gone. The S3 secret key is
kept in the server's database, like the SMTP password.

### Ransomware protection

Ransomware goes for the backups before it shows itself. There are three ways
it, or the person behind it, can reach them, and the server guards each.
Everything here is under "Ransomware protection" in the web UI.

**From a backed-up machine.** An agent can only add to the repository. Its
API has no call that deletes anything, and it can read only chunks it already
knows. What a taken-over machine can do is back up its encrypted files, so
that the retention rules push the good backups out. So the server compares
every backup with the one before it and looks for:

- files replaced by files of the same name with a new ending
  (`report.docx` gone, `report.docx.locked` in its place),
- the same new file with a name like a ransom note's in many folders,
- half the files changed or gone, or a quarter when the new data does not
  compress, as encrypted data does not,
- a burst of data that does not compress, five times what the policy's
  backups usually add. This is what an encrypted disk looks like in an image
  or virtual machine backup.

The server measures this on what it receives; it does not take the machine's
word. A backup that looks wrong is still kept, finishes with warnings, shows
at the top of the page and goes out as an alert. From then on **nothing older
of that policy is forgotten**, whatever its retention rules say, until an
administrator chooses "This was expected" or deletes the bad backup. Each of
these signs also has honest causes (a big reorganisation, converting a photo
library), which is why a person decides.

**With a stolen password.** Someone signed in as an administrator could
delete snapshots or set a policy to keep nothing.

- A deleted snapshot is only set aside, for 7 days unless you change it, and
  "Bring back" restores it. This holds for snapshots deleted by hand and for
  those the retention rules forget. Their disk space is freed when the days
  are over, and there is no way to empty the list early.
- With a retention lock of N days, no snapshot can be deleted before it is N
  days old, by anyone or by retention. It is off until you set it; a policy
  then holds at least N days of snapshots, so the disk must have room.
- Deleting a snapshot takes an administrator, who types their password
  again, and everyone who gets alerts is told. So are the people who got
  alerts until now when someone changes where alerts go.
- A change that makes any of this weaker takes effect three days later. It
  is announced at once and can be cancelled until then. A change that makes
  it stronger applies immediately.

**By taking over the server.** Whoever controls the server's disk can erase
the repository, and nothing the server enforces stops that. What survives is
an offsite copy that the server itself cannot delete: under "Offsite copy",
set "Lock the copy against deletion" to a number of days. This needs a bucket
created with Object Lock (Amazon S3, Backblaze B2, Wasabi and MinIO have
it). Then:

- everything uploaded is locked until a date at least that many days away,
  and each run moves the date on for everything the repository still has;
- deleting from the copy only hides a file, and overwriting only adds a
  version. The old data stays until its lock has run out, and the server
  then removes it;
- when recovering a lost server, "The copy was damaged or deleted too" takes
  the copy as it was at a time you choose, ignoring everything done to it
  since.

In "Compliance" mode nobody can lift a lock, not even the account owner, so
try a small number of days first: you pay for the storage until the locks run
out. A folder cannot be locked. The storage keys the server holds should not
be allowed to change the bucket's settings or delete the bucket.

What this does not cover: the lock days must be longer than the time it
takes you to notice an attack, and the repository key must be kept somewhere
other than the server, as always.

### If the server is lost

Everything can be done from the browser. You need the repository key
(`repo.key`, downloaded from the web UI) and either the offsite copy or the
disk the repository is on.

**From the offsite copy.** Install a new server and open its page. Instead
of creating an administrator choose "Recover a lost server instead…", say
where the copy is and give the key. The page shows the download and then
the sign-in of the old server. If the copy is locked and was damaged too,
open "The copy was damaged or deleted too" and give a time before the damage.

**From the repository disk.** Start the new server with `--repo` pointing at
the old repository. It finds backups but no key, and its page asks for the
key.

Either way the server gets back its users, agents, policies, settings and
its certificate authority: the server saves these into the repository,
encrypted with the repository key, every day, soon after a change and before
each offsite copy. Agents carry on without being enrolled again when the new
server has the old one's address. A copy made by an older version holds only
the backups; the page then asks for a new administrator, and agents are
enrolled again.

Only a new server, without users, agents or backups, can be replaced this
way.

The same from a command line, for a restore without starting a server:

```sh
bb-server snapshots --repo /mnt/backup-disk/repo --data /folder/holding/repo.key
bb-server restore --repo /mnt/backup-disk/repo --data /folder/holding/repo.key \
    --snapshot SNAPSHOT --target /restore/here

export BB_S3_ACCESS_KEY='...' BB_S3_SECRET_KEY='...'
bb-server fetch --repo /mnt/new-disk/repo \
    --s3-endpoint https://s3.us-west-004.backblazeb2.com --s3-bucket my-backups
# or: bb-server fetch --repo /mnt/new-disk/repo --from-folder /mnt/offsite
```

## Known limits

- The Windows agent must run as an administrator or as a service to take
  snapshots. If a snapshot cannot be made, the backup reads the live files,
  skips the ones in use, and finishes with warnings.
- VSS backups are "copy" backups: application logs (SQL Server, Exchange) are
  not truncated. Application-aware backup comes in Phase 6.
- Not yet preserved: NTFS audit entries (SACLs) and EFS encryption, Unix extended attributes and ACLs, hard links (each link is
  restored as its own file). Windows keeps most system files as hard links
  into its component store, so a system state restored in place uses more
  disk space than before.
- No MSI installer yet; `service install` does the same job on Windows.
- The macOS installer is not signed or notarized until the release workflow is
  given an Apple Developer ID (see `.github/workflows/release.yml`).
- Microsoft 365 backup has only met a stand-in for Microsoft Graph, and
  restores into a folder only. See the Microsoft 365 section for what it
  leaves out.
- The repository itself lives on a local disk or mounted share; S3 is for the
  offsite copy. The copy holds what the repository holds, so a snapshot
  forgotten here is also removed there at the next run, unless the copy is
  locked.
- The ransomware checks compare file backups folder by folder. Image and
  virtual machine backups are judged only by how much new data they add and
  whether it compresses. The locked offsite copy has run against MinIO, not
  yet against Amazon S3 or Backblaze B2.

## Roadmap

| Phase | Scope |
| --- | --- |
| 1. Foundation (this release) | Server, file agent, dedup store, restore in place, Docker and native services |
| 2. Windows VSS (done) | VSS snapshots, NTFS owners, permissions and alternate data streams, policies in the web UI, email alerts |
| 3. Hyper-V and images (in progress) | Done: volume and virtual disk images, VM backup from checkpoints and restore as a copy, change tracking for VM backups (untested on a real host). To do: VM restore in place |
| 4. System state and AD (in progress) | Done: system state backup through VSS writers, including the AD database and SYSVOL on a domain controller. restore over the running system with the registry, non-authoritative and authoritative Active Directory restore in DSRM (little tested on real machines) |
| 5. Scale and offsite (in progress) | Done: offsite copy to S3-compatible storage or a second disk. Done also: KVM virtual machines through libvirt, Proxmox VE virtual machines (untested on a real node), VMware vSphere virtual machines over the network (untested on a real host, full reads only). Done also: users with roles, an activity log, and recovering a lost server in the browser. Done also: health report, summaries, webhooks, cancelling jobs, scheduled checks. Done also: ransomware protection (delayed deletion, retention lock, detection, locked offsite copy). To do: VMware Changed Block Tracking, Postgres option, two-factor sign-in |
| 6. Full recovery (in progress) | Done: whole-machine backup, bare-metal recovery disc, restore as a new Hyper-V machine. To do: driver injection for different hardware, SQL Server and Exchange writers, object-level AD restore |

See [docs/architecture.md](docs/architecture.md) for the design and the
storage format.

## Building

Requires Go 1.26 or later.

```sh
make build    # bb, bb-agent and bb-server in ./bin
make app      # the desktop app, for this machine (needs cgo; on Linux also
              # the libgtk-3-dev and libwebkit2gtk-4.1-dev packages)
make test
make docker
make recovery # the bare-metal recovery disc, in ./bin/recovery.iso
make recovery-arm64 # the same for ARM64 machines, in ./bin/recovery-arm64.iso
```

The macOS installer is built on a Mac by `packaging/macos/build.sh VERSION`.

`make recovery` needs an Ubuntu 24.04 machine with the tools named at the top
of `packaging/recovery/build.py`.

## License

Apache-2.0. See [LICENSE](LICENSE).
