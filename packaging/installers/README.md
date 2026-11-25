Agent installers placed here before `docker build` are put in the server
image, and the server's web UI offers them under "Add an agent". The Docker
workflow puts the installers for every system here: Windows (.msi), Linux
(.deb and .rpm), each for x64 and ARM64, and macOS (.pkg). Only files named like
`better-backup-agent_VERSION_OS_ARCH.msi` (or .pkg, .deb, .rpm) are offered.
