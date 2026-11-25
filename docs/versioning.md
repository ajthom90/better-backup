# Versions, channels and releases

## Version numbers

better-backup follows [Semantic Versioning 2.0.0](https://semver.org).

| Version | What it is |
|---|---|
| `1.4.2` | A release. |
| `1.5.0-rc.1` | A release candidate: a release being tried before it is one. |
| `1.4.3-edge.12` | A build from the `main` branch: the 12th commit after release 1.4.2. |
| `0.1.0-edge.170` | A build from `main` before the first release: the 170th commit. |
| `dev` | Built from source with no version given. |

What the three numbers promise:

- **Patch** (`1.4.2` to `1.4.3`): fixes. Nothing to learn, nothing to change.
- **Minor** (`1.4.x` to `1.5.0`): new features. What worked keeps working.
- **Major** (`1.x` to `2.0.0`): something you rely on changed in a way that
  needs your attention: a setting that moved, a step to take after updating,
  an old agent that can no longer connect. The release notes say what to do.

While the first number is 0, the project is young and a minor release may
do what a major one does later. The release notes still say so.

What never breaks without a major release, once 1.0.0 is out: the repository
format (a newer server reads every older repository), the protocol between
server and agents within one major version, and the admin API under
`/api/v1`.

A build from `main` is numbered as a pre-release of the *next* patch
release, so that every ordering comes out right: `1.4.2` < `1.4.3-edge.1` <
`1.4.3-edge.12` < `1.4.3` and < `1.5.0`. The number is the count of commits
since the last version tag, so it only grows.

One build carries one version everywhere: `bb-server version`, the web UI,
the agents, the names of the installers and packages, and the image tags.
The version is written without a `v`; only the git tag has one (`v1.4.2`).

`go run ./internal/tools/release version` prints the version of a checkout;
every workflow asks it. The rules are in `internal/version`, with tests.

### What each kind of package does with it

| Package | Version it carries | Going back to an older one |
|---|---|---|
| Windows `.msi` | `1.4.2`; a pre-release puts its last number fourth: `1.4.3.12` | Refused |
| Linux `.deb`, `.rpm` | `1.4.2`; a pre-release as `1.4.3~edge.12`, which sorts before `1.4.3` | The package manager warns that it is a downgrade |
| macOS `.pkg` | `1.4.2` (the bundle); the programs say the full version | Installs whatever it is given |
| Container image | the tag | Whatever tag you run |

Windows Installer compares only the first three numbers. Two builds from
`main` share them (`1.4.3-edge.12` and `1.4.3-edge.15` are both 1.4.3 to
Windows), so the installers let a build replace another with the same three
numbers. That is what makes one edge build install over another, by hand or
when an agent updates itself. The server decides which is newer by the full
version before it offers an agent anything, so agents never go backwards by
themselves.

## Channels

| | stable | edge |
|---|---|---|
| What | Releases | Every commit on `main` |
| Image tags | `:1.4.2`, `:1.4`, `:1` (from 1.0.0), `:latest` | `:edge`, `:1.4.3-edge.12`, `:sha-COMMIT` |
| On GitHub | A release per version, tagged `v1.4.2` | One pre-release tagged `edge`, replaced by each build |
| Release notes | The version's section of `CHANGELOG.md` | What is waiting in `changelog.d` |

- `:latest` is the newest release. It never points at a release candidate
  or at a fix for an older line.
- `:1.4` follows the fixes of the 1.4 line; `:1` every 1.x release. There is
  no `:0` tag, because 0.x minor releases may break each other.
- `:edge` is the newest build from `main`. Right after a release is made
  from `main`, `:edge` is that release.
- A release candidate (`1.5.0-rc.1`) gets its own tag only, and is a
  pre-release on GitHub. Servers on the edge channel hear of it; servers on
  the stable channel do not.

## Telling the administrator

The server reads
`https://api.github.com/repos/ajthom90/better-backup/releases` twice a day
(and an hour after a failed try). The request has no body and no parameters
that say anything about the server; the comparison happens on the server.
On the stable channel it looks at the releases; on the edge channel also at
the `edge` pre-release (whose title is its version) and at release
candidates. If something is newer than the running version, the web UI
shows it under **Updates** with the notes of every version in between, and
one alert goes out per version through the server's alerts.

The channel defaults to the one the running build came from. The check and
the alert can each be turned off in the web UI. `BB_UPDATE_URL` points the
check somewhere else; the answer must look like GitHub's. The code is
`internal/server/updatecheck.go`.

## Release notes

Every pull request that changes something a person running better-backup
would notice adds a file to `changelog.d` (its README says how). CI refuses
a pull request without one unless it has the label `no-changelog`. Separate
files mean that pull requests written at the same time never conflict.

At a release the entries move into `CHANGELOG.md` under the new version and
the files are removed. The same text becomes the body of the GitHub release,
which is what servers show under Updates. `CHANGELOG.md` and the waiting
entries are also built into the server, so "what is new in the version that
runs" works without the internet.

## Making a release

1. On GitHub, open Actions, pick **Release**, press **Run workflow** on
   `main`, and type `patch`, `minor` or `major` (or a version written out,
   such as `1.5.0-rc.1`). Tick **Dry run** to build everything and publish
   nothing.
2. The workflow writes `CHANGELOG.md`, commits it to `main`, tags the commit,
   runs the tests, builds the installers for Windows and macOS (signed and
   notarized where the secrets are set), the archives and Linux packages,
   the recovery discs and the images, which carry that version's agent
   installers.
3. It pushes the images and then publishes the GitHub release with every
   file and a `SHA256SUMS`. The release appears last, so a server never
   hears of a version whose files are not there yet.

Servers on the stable channel show the new version within twelve hours.
Agents follow their server when it is updated.

A fix for an older line (say 1.4.3 when 1.5.0 is out) is made on a branch
from `v1.4.2`: write the `CHANGELOG.md` section there
(`go run ./internal/tools/release prepare 1.4.3`), commit, and push the tag
`v1.4.3`. The workflow publishes it without moving `:latest`.

If a release goes wrong, do not move or reuse its tag: fix the problem and
release the next patch version.
