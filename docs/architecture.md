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
in Amazon's documentation and against MinIO in CI.
