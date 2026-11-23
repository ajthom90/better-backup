# better-backup

A fully open-source backup platform: one server that stores deduplicated,
compressed, encrypted backups on any disk you give it, and agents for Windows,
macOS and Linux. It is meant to replace a backup appliance without the
appliance's capacity ceiling.

**Status: Phase 2 in progress.** File backup and restore work end to end on
all three operating systems, and Windows backups read from VSS snapshots.
Hyper-V, system state and Active Directory are on the [roadmap](#roadmap) and
not built yet. Do not make this your only
backup until it has run beside your current one for a while.

## What works today

- **File backup** with content-defined chunking: after the first run only
  changed data is read and sent, and identical data is stored once across
  every machine.
- **Restore in place** over the original files, or to another folder or
  another machine. Only files that differ from the backup are rewritten.
- **VSS snapshots on Windows**: each backup reads a frozen shadow copy of the
  volume, so files that are open or locked are backed up, and VSS-aware
  applications flush their data first. NTFS owners and permissions are
  backed up and restored.
- **Agents for Windows, macOS and Linux**, enrolled with a one-time token and
  authenticated with per-agent TLS certificates. Agents dial out to the
  server, so only the server needs an open port.
- **Schedules and retention**: run every N hours or daily at a set time; keep
  the last N, daily, weekly, monthly and yearly snapshots.
- **Encryption at rest** (AES-256-GCM) and zstd compression.
- **Repository maintenance**: garbage collection, and a full verify that reads
  and checks every stored chunk.
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

The web UI at `https://your-server:8443/` shows agents, policies, jobs and
snapshots, and can start backups and restores. Sign in with the admin token
from `admin.json` in the data directory. Your browser will warn about the
certificate, because the server uses its own private certificate authority.

### If the server is lost

All you need is the repository folder and `repo.key`:

```sh
bb-server snapshots --repo /mnt/backup-disk/repo --data /folder/holding/repo.key
bb-server restore --repo /mnt/backup-disk/repo --data /folder/holding/repo.key \
    --snapshot SNAPSHOT --target /restore/here
```

## Known limits

- The Windows agent must run as an administrator or as a service to take
  snapshots. If a snapshot cannot be made, the backup reads the live files,
  skips the ones in use, and finishes with warnings.
- VSS backups are "copy" backups: application logs (SQL Server, Exchange) are
  not truncated. Application-aware backup comes in Phase 6.
- Not yet preserved: NTFS audit entries (SACLs) and EFS encryption, Unix extended attributes and ACLs, hard links (each link is
  restored as its own file).
- Policies are created with the CLI; the web UI can run and restore them.
- No MSI or `.pkg` installers yet; `service install` does the same job.
  `.deb` and `.rpm` packages are built on release.
- One repository on a local disk or mounted share. S3-compatible storage and
  replication to a second site come in Phase 5.

## Roadmap

| Phase | Scope |
| --- | --- |
| 1. Foundation (this release) | Server, file agent, dedup store, restore in place, Docker and native services |
| 2. Windows VSS (in progress) | Done: VSS snapshots, NTFS owners, permissions and alternate data streams. To do: fuller web UI, email alerts |
| 3. Hyper-V and images | Host-level VM backup with Resilient Change Tracking, VM restore in place or as a copy, volume image backup |
| 4. System state and AD | System state through VSS writers, domain controller backup, DSRM-assisted authoritative and non-authoritative restore |
| 5. Scale and offsite | S3-compatible backends, replication, Proxmox and KVM providers, Postgres option, user roles |
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
