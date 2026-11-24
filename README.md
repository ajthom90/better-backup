# better-backup

A fully open-source backup platform: one server that stores deduplicated,
compressed, encrypted backups on any disk you give it, and agents for Windows,
macOS and Linux. It is meant to replace a backup appliance without the
appliance's capacity ceiling.

**Status: Phase 2 in progress.** File backup and restore work end to end on
all three operating systems, and Windows backups read from VSS snapshots.
Hyper-V and restoring a system state or Active Directory in place are on the
[roadmap](#roadmap) and not built yet. Do not make this your only
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
- **Windows system state**: the registry, boot and system files, and on a
  domain controller the Active Directory database and SYSVOL, as the
  system's own VSS writers list them. It can be restored to a folder;
  restoring it over a running system is not built yet.
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
- **Disaster recovery**: `bb-server restore` reads a repository directly and
  needs only the repository folder and its key.
- **Runs in Docker or natively** as a systemd unit, launchd daemon or Windows
  service. One static binary each for server, agent and CLI.

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

> **Copy `repo.key` somewhere safe now.** Backups are encrypted with it. If
> the key is lost, the backups cannot be read by anyone.

Use `--repo DIR` (or mount a volume at `/data/repo`) to put the repository on
a bigger disk or a NAS mount.

### 2. Enroll a machine

On the server (in Docker: `docker exec better-backup bb token create`):

```sh
bb token create
```

It prints a command to run on the machine you want to back up:

```sh
sudo bb-agent enroll --server https://backup.example.com:8443 --token ... --fingerprint sha256:...
sudo bb-agent service install
sudo bb-agent service start
```

On Windows, run the same commands without `sudo` from an elevated prompt.

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
restore a file, a folder, a disk or all of it, follow jobs and read their
warnings, forget snapshots, verify the repository, reclaim space and set up
email alerts. The `bb` command line does the same things for scripts. Sign in with the admin token
from `admin.json` in the data directory. Your browser will warn about the
certificate, because the server uses its own private certificate authority.

### Whole volumes

```sh
bb policy add --name fileserver-d --agent fileserver --volume D: --daily-at 01:00
bb restore SNAPSHOT --wait                      # back over D:
bb restore SNAPSHOT --target E: --wait          # onto another volume
bb restore SNAPSHOT --target /srv/d.img --agent linux-box --wait
```

Restoring over a volume replaces everything on it. The volume must not be in
use: Windows refuses to lock a volume with open files, and the system volume
cannot be restored from the running system (recovery media comes in Phase 6).
The target must be at least as large as the original volume.

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
443. That agent reads the disks and sends them on, so pick one close to the
hosts.

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

For now it restores to a folder only. Putting a system state back over a
running Windows, and the Directory Services Restore Mode steps for a domain
controller, come next.

### Email alerts

Set them up under "Email alerts" in the web UI, or from the command line:

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

### If the server is lost

All you need is the repository folder and `repo.key`:

```sh
bb-server snapshots --repo /mnt/backup-disk/repo --data /folder/holding/repo.key
bb-server restore --repo /mnt/backup-disk/repo --data /folder/holding/repo.key \
    --snapshot SNAPSHOT --target /restore/here
```

If the repository is gone too, download the offsite copy first:

```sh
export BB_S3_ACCESS_KEY='...' BB_S3_SECRET_KEY='...'
bb-server fetch --repo /mnt/new-disk/repo \
    --s3-endpoint https://s3.us-west-004.backblazeb2.com --s3-bucket my-backups
# or: bb-server fetch --repo /mnt/new-disk/repo --from-folder /mnt/offsite
```

Then use the two commands above, or start a new server with
`--repo /mnt/new-disk/repo` and `repo.key` in its data directory.

## Known limits

- The Windows agent must run as an administrator or as a service to take
  snapshots. If a snapshot cannot be made, the backup reads the live files,
  skips the ones in use, and finishes with warnings.
- VSS backups are "copy" backups: application logs (SQL Server, Exchange) are
  not truncated. Application-aware backup comes in Phase 6.
- Not yet preserved: NTFS audit entries (SACLs) and EFS encryption, Unix extended attributes and ACLs, hard links (each link is
  restored as its own file).
- No MSI or `.pkg` installers yet; `service install` does the same job.
  `.deb` and `.rpm` packages are built on release.
- The repository itself lives on a local disk or mounted share; S3 is for the
  offsite copy. The copy holds what the repository holds, so a snapshot
  forgotten here is also removed there at the next run.

## Roadmap

| Phase | Scope |
| --- | --- |
| 1. Foundation (this release) | Server, file agent, dedup store, restore in place, Docker and native services |
| 2. Windows VSS (done) | VSS snapshots, NTFS owners, permissions and alternate data streams, policies in the web UI, email alerts |
| 3. Hyper-V and images (in progress) | Done: volume and virtual disk images, VM backup from checkpoints and restore as a copy, change tracking for VM backups (untested on a real host). To do: VM restore in place |
| 4. System state and AD (in progress) | Done: system state backup through VSS writers, including the AD database and SYSVOL on a domain controller. To do: restore in place, DSRM-assisted authoritative and non-authoritative restore |
| 5. Scale and offsite (in progress) | Done: offsite copy to S3-compatible storage or a second disk. Done also: KVM virtual machines through libvirt, Proxmox VE virtual machines (untested on a real node), VMware vSphere virtual machines over the network (untested on a real host, full reads only). To do: VMware Changed Block Tracking, Postgres option, user roles |
| 6. Full recovery | Bare-metal recovery media, SQL Server and Exchange writers, object-level AD restore |

See [docs/architecture.md](docs/architecture.md) for the design and the
storage format.

## Building

Requires Go 1.26 or later.

```sh
make build    # binaries in ./bin
make test
make docker
```

## License

Apache-2.0. See [LICENSE](LICENSE).
