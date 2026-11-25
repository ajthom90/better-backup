# Entries for the next release's notes

Every pull request that changes something a person running better-backup
would notice adds one file here. The files become the notes of the next
release: in `CHANGELOG.md`, on the GitHub release, and in the server's web
UI under "Updates". Builds from the main branch show them as "what changed
since the last release".

Name the file `SHORT-NAME.KIND.md`, for example `ntfy-alerts.added.md`.
KIND is one of:

| Kind       | For                                                    |
|------------|--------------------------------------------------------|
| `added`    | something new                                          |
| `changed`  | something that works differently than before           |
| `fixed`    | a bug that is gone                                     |
| `removed`  | something that is no longer there                      |
| `security` | a fix that matters for security                        |

In the file, say what changed in a sentence or two, for the person who
runs the server, not for the one who reads the code. Start a line with
`- ` for each further entry. Say what to do if the change needs something
from them ("after updating, ...").

    Alerts can go to an ntfy topic, set under Alerts and reports.

A change nobody would notice (tests, refactoring, CI) needs no entry: give
the pull request the label `no-changelog` instead.

`go run ./internal/tools/release check` checks the files; `go run ./internal/tools/release
notes` prints the notes as they stand.
