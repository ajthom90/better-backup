# Working in this repository

- **Release notes.** Every pull request that changes something a person
  running better-backup would notice adds a file under `changelog.d`, named
  `SHORT-NAME.KIND.md` (KIND: `added`, `changed`, `fixed`, `removed` or
  `security`), with a sentence or two in plain words for the administrator.
  `changelog.d/README.md` has the details. CI fails without one; a change
  nobody would notice gets the label `no-changelog` instead. Never edit
  `CHANGELOG.md` by hand: the release workflow writes it.
- **Versions.** Never hard-code a version. `go run ./internal/tools/release
  version` prints the version of a checkout, and `internal/version` compares
  versions. `docs/versioning.md` has the policy (Semantic Versioning, the
  stable and edge channels, image tags).
- **Releases.** A release is made by the Release workflow, started by the
  repository's owner. Do not push version tags or run that workflow without
  being asked to.
- **Web UI.** Every feature must be manageable from the web UI
  (`internal/server/ui/index.html`), and ships with its UI in the same pull
  request.
