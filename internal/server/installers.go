package server

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/version"
)

// releases is where published installers are downloaded from when the
// server has no copy of its own.
const releases = "https://github.com/ajthom90/better-backup/releases"

// installerName matches agent installers as the release names them, for
// example better-backup-agent_1.2.0_windows_amd64.msi.
var installerName = regexp.MustCompile(`^better-backup-agent_([0-9A-Za-z.+~-]+)_(windows|darwin|linux)_([a-z0-9]+)\.(msi|pkg|deb|rpm)$`)

// installerDirs are searched for agent installers to hand out: a folder
// in the data directory, for the administrator to fill, and one beside
// the program, which the server's own installer fills.
func (s *Server) installerDirs() []string {
	dirs := []string{filepath.Join(s.cfg.DataDir, "installers")}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "installers"))
	}
	return dirs
}

// localInstallers maps the name of each installer the server holds to
// its file.
func (s *Server) localInstallers() map[string]string {
	found := map[string]string{}
	for _, dir := range s.installerDirs() {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if _, dup := found[e.Name()]; !dup && e.Type().IsRegular() && installerName.MatchString(e.Name()) {
				found[e.Name()] = filepath.Join(dir, e.Name())
			}
		}
	}
	return found
}

func (s *Server) handleInstallers(w http.ResponseWriter, r *http.Request) {
	out := api.Installers{Releases: releases, Files: []api.Installer{}}
	out.Hostname, _ = os.Hostname()
	for name, path := range s.localInstallers() {
		m := installerName.FindStringSubmatch(name)
		in := api.Installer{Name: name, Version: m[1], OS: m[2], Arch: m[3], Format: m[4], URL: "/installers/" + name}
		if st, err := os.Stat(path); err == nil {
			in.Size = st.Size()
		}
		out.Files = append(out.Files, in)
	}
	// A released server can point at the installers published with it.
	// (Builds from main are not: their files are replaced by the next
	// build.)
	if v, ok := version.Parse(Version); ok && !v.Edge() && v.Build == "" {
		m := []string{"", v.String()}
		have := map[string]bool{}
		for _, f := range out.Files {
			have[f.OS+"/"+f.Arch+"/"+f.Format] = true
		}
		for _, t := range []string{"windows/amd64/msi", "windows/arm64/msi", "linux/amd64/deb", "linux/arm64/deb", "linux/amd64/rpm", "linux/arm64/rpm", "darwin/universal/pkg"} {
			if have[t] {
				continue
			}
			p := strings.Split(t, "/")
			name := "better-backup-agent_" + m[1] + "_" + p[0] + "_" + p[1] + "." + p[2]
			out.Files = append(out.Files, api.Installer{Name: name, Version: m[1], OS: p[0], Arch: p[1], Format: p[2],
				URL: releases + "/download/v" + m[1] + "/" + name})
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	reply(w, http.StatusOK, out)
}

// handleInstallerFile serves an installer without signing in, so it can
// be fetched from the machine it is for. Installers hold no secrets.
func (s *Server) handleInstallerFile(w http.ResponseWriter, r *http.Request) {
	path, ok := s.localInstallers()[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}
