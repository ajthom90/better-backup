# Architecture

better-backup has three programs:

- **`bb-server`** owns the repository, hands jobs to agents, runs schedules,
  retention and maintenance, and serves the admin API and web UI.
- **`bb-agent`** runs on each protected machine. It reads files, cuts them
  into chunks, asks the server which chunks are new, and uploads only those.
  It also performs restores.
- **`bb`** is the admin command line. It talks to the server's admin API.

```
 agents (Windows, macOS, Linux)            server                    storage
 ┌──────────────────────────┐   mTLS   ┌──────────────────┐     ┌──────────────┐
 │ walk, chunk, hash, upload │ ───────▶ │ agent API        │     │ pack files   │
 │ only missing chunks       │  :8443   │ scheduler        │ ──▶ │ snapshots    │
 │ restore                   │ ◀─────── │ dedup repository │     │ index.db     │
 └──────────────────────────┘          │ admin API, web UI│     └──────────────┘
                                       └──────────────────┘
```

## Source layout

| Package | Role |
| --- | --- |
| `internal/chunker` | FastCDC content-defined chunking |
| `internal/keys` | Chunk ids, key derivation, AES-256-GCM, Argon2id key wrapping |
| `internal/repo` | The repository: pack files, index, snapshots, GC, verify |
| `internal/tree` | Snapshot and directory metadata format |
| `internal/backup` | Agent-side file walker and uploader |
| `internal/restore` | Restore, in place or to a target directory |
| `internal/server` | Server: jobs, scheduling, retention, HTTP APIs, private CA, web UI |
| `internal/agent` | Agent: enrollment, job loop |
| `internal/api` | Wire types and the pinned-TLS client configuration |
| `internal/vss` | Windows Volume Shadow Copy requester and backup privileges |
| `internal/svc` | Windows service, systemd and launchd integration |
| `internal/hypervisor` | What virtual machine hosts have in common: the driver interface |
| `internal/hyperv` | Hyper-V driver (PowerShell and WMI) |
| `internal/libvirt` | KVM driver (libvirt backup jobs and checkpoints through `virsh`) |
| `internal/proxmox` | Proxmox VE driver (`qm`, `pvesm`, and QEMU's machine protocol) |
| `internal/vmware` | VMware vSphere driver (snapshots and OVF export and import, through govmomi) |
| `internal/vmdk` | Reads and writes VMware's stream-optimized disk format |
| `internal/nbd`, `internal/qemuimg`, `internal/qmp` | NBD client, `qemu-nbd` wrapper and QMP client that read and write QEMU disks |

Data sources other than files (volume images, Hyper-V and other hypervisors,
system state) will plug in beside `internal/backup`: each produces entries and
chunks for the same repository, so dedup, retention and restore plumbing are
shared.

## Storage format (version 1)

A repository is a directory:

```
config.json          repository id; master key wrapped with Argon2id + AES-256-GCM
packs/ab/<id>.pack   pack files (about 32 MiB each)
snapshots/<id>.snap  encrypted snapshot records
index.db             SQLite: chunk id -> (pack, offset, length). A cache.
```

**Chunks.** File data is cut by FastCDC (512 KiB minimum, 1 MiB average,
8 MiB maximum, normalized chunking). A chunk's id is the keyed BLAKE3 hash of
its plaintext. The key is derived from the repository master key, so ids do
not reveal whether a known file is present to someone who only sees ids.

**Packs.** Each chunk is zstd-compressed when that makes it smaller, then
sealed with AES-256-GCM under a subkey derived from the master key and the
chunk's own id, with the id as associated data. A pack is:

```
"BBPACK1\n"
sealed chunk, sealed chunk, ...
sealed trailer: for each chunk (id, offset, sealed length, plaintext length)
uint32 trailer length
"BBPKEND1"
```

A pack is written to a temp file, fsynced, renamed into place and only then
indexed. Because every pack carries its own trailer, `index.db` can be deleted
and is rebuilt from the packs on the next open.

**Snapshots and trees.** A snapshot lists its source roots. A directory is
stored as one or more pages (JSON arrays of entries, about 1 MiB each), and
each page is itself a chunk. A file entry lists its chunk ids; files with more
than 1024 chunks keep the list in index chunks instead. Unchanged subtrees
produce identical pages, so they deduplicate between snapshots.

**Garbage collection** marks every chunk reachable from every snapshot,
deletes packs with no live chunks, and rewrites packs that are less than half
live. It refuses to delete anything if a snapshot refers to a chunk the
repository does not have. GC and verify run only while no backup or restore is
active, and new jobs wait until they finish.

## Security model

- **Transport.** The server runs a private certificate authority, created on
  first start. Agents and the CLI pin that CA by SHA-256 fingerprint and
  accept only certificates it issued for server authentication. Each agent
  holds its own client certificate, issued at enrollment from a one-time
  token. Revoking an agent blocks its certificate immediately.
- **At rest.** Everything in the repository except `config.json` is
  encrypted. The master key is wrapped by a passphrase: either
  `BB_REPO_PASSPHRASE`, or a random key the server generates in `repo.key`.
- **Trust in agents.** The server verifies every uploaded chunk against its
  id, so an agent cannot poison data that other machines deduplicate against.
  An agent can fetch a chunk only if it knows its id, and ids are only learned
  from the agent's own snapshots or by already having the content. Agents can
  transfer chunks only while they have a running job.
- **The server holds the key.** This is what allows dedup across machines and
  restores that need no secret from the agent. It means whoever controls the
  server and its key can read the backups; protect both accordingly.

## Jobs

Agents long-poll `GET /agent/v1/jobs/next`. A backup job carries the paths,
the exclusions and the previous snapshot's roots, which the agent uses to skip
files whose size and modification time are unchanged. While a job runs the
agent reports progress every ten seconds; a job with no report for five
minutes is failed. A finished backup is committed as a snapshot only after its
chunks are flushed to disk, and then the policy's retention rules are applied.

## Windows snapshots

On Windows a backup job first asks VSS for a snapshot of every volume that
holds a backed-up path (`internal/vss`). The agent is a VSS requester: it
gathers writer metadata, so VSS-aware applications flush and freeze, creates
the snapshot set, and reads files from the shadow copy device
(`\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopyN\...`) while recording the
original paths. When the backup ends it reports completion to the writers and
releases the snapshot. The backup type is "copy", so application logs are left
alone.

If a volume cannot be snapshotted (a network share, or the agent is not
elevated), the job continues on the live files and records a warning.

Each entry also stores the file's security descriptor (owner, group and DACL).
On restore the DACL is applied first and the owner second, because setting
another account as owner needs the restore privilege. A file that inherited
its permissions inherits again from the folder it is restored into; a file
with inheritance blocked keeps its exact permissions.

## Virtual machines

A hypervisor is a `hypervisor.Driver`: it lists the host's machines, freezes
one (`Freeze`) into a configuration blob and a set of disks to image, and
builds a machine again from such a blob (`NewRestore`). A machine is stored as
an entry of type `vm` holding the configuration as a small file, whose name
says which driver wrote it (`vm.json` for Hyper-V, `libvirt.json` for KVM,
`proxmox.json` for Proxmox VE, `vmware.json` for VMware vSphere),
and one image per disk. Disks are always stored as their raw contents in
fixed 1 MiB blocks, whatever the format on the host, so any disk can be
restored anywhere as a plain image.

Change tracking is the driver's business up to one point: it hands the
archiver a list of byte ranges written since the previous backup, and the
archiver takes every other block from that backup without reading it. The
driver learns what the previous backup was from its configuration blob, which
the archiver passes back, so the state that matters (a Hyper-V reference
point, a libvirt checkpoint name) travels with the snapshot it belongs to. A
backup that fails leaves the previous checkpoint in place for the next try.

The KVM driver reads running machines through libvirt's pull-mode backup
job: QEMU exports each disk over NBD on a unix socket inside the machine's
private folder, as of the moment the job began, and `internal/nbd` reads the
allocation map, the dirty bitmap and the data from there. Stopped machines
and restores go through `qemu-nbd`, which handles the image formats.

Proxmox VE does not use libvirt, so its driver talks to each machine's QEMU
directly over the QMP socket Proxmox keeps for it (`internal/proxmox/live.go`).
It builds by hand what a libvirt backup job is underneath: a scratch qcow2
image on top of each disk, a `blockdev-backup` job with `sync=none` that
copies into it what the machine is about to overwrite, and an NBD export of
the scratch image; all disks start in one QMP transaction, so they are frozen
at the same instant. Change tracking is a dirty bitmap per backup, added in
that same transaction, and a frozen copy of the previous backup's bitmap is
exported beside the data. Bitmaps are not persistent, so they work on raw
volumes too and vanish with the QEMU process. Proxmox's services use the same
socket, which serves one client at a time, so the driver connects only for
the moments it needs. After the disks are read the driver checks that every
job is still running, because QEMU ends a job it cannot keep its promise for
(a full scratch folder), and then the machine is left out of the backup.

VMware vSphere is reached over its management API rather than from an agent
on the host, so a policy names a server from a list the better-backup server
keeps, and the job carries that server's address and account to the agent
when it is handed over (`hypervisor.Connection`; it is not stored with the
job). The driver takes a snapshot, asks for an export lease on it
(`ExportSnapshot`) and downloads each disk as a stream-optimized VMDK, which
`internal/vmdk` unpacks front to back as the archiver asks for blocks; the
machine's description is the OVF descriptor the server writes
(`OvfManager.CreateDescriptor`), kept in the configuration blob with small
files such as the firmware settings. A restore gives that descriptor back
(`CreateImportSpec`, `ImportVApp`) and uploads each disk as a stream-optimized
VMDK written on the fly. Its grains are stored rather than compressed, which
makes the length of the stream known before the first byte: the upload needs
that, and the disk never has to be spooled. The lease is kept alive from a
timer, and after the disks are read the driver checks that every stream ended
the way the format says and that the lease is still good, or the machine is
left out. This path reads whole disks; Changed Block Tracking needs random
reads of a snapshot's disks (NBD through VDDK, or NBDSSL) and is not built.

## Offsite copy

`internal/offsite` keeps a second copy of the repository in a folder or in an
S3-compatible bucket. It relies on the repository's files never changing once
written: a run lists the snapshot records, then the pack files, uploads the
packs the copy lacks, then the snapshots, then `config.json`, and finally
removes files the repository no longer has. Listing snapshots before packs and
uploading them after means the copy never holds a snapshot whose data is
missing. The index is not copied; it is rebuilt from the pack trailers.

The copy runs beside backups and restores but never beside garbage collection,
which moves data between packs. If more than half of the copy's files would be
removed, the run stops without removing anything, since that usually means the
repository's disk is damaged or not mounted. The S3 client is a small
Signature Version 4 implementation in `s3.go`, tested against the signatures
in Amazon's documentation and against an in-process server that recomputes
every signature. `TestRealS3` runs the same round trip against a real service
when `BB_TEST_S3_ENDPOINT` is set.
