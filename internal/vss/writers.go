package vss

import (
	"encoding/xml"
	"io"
	"path"
	"sort"
	"strings"
)

// FileSpec is one line of a writer's file list: the files in Path whose
// names match Spec, and with Recursive those in every folder below it.
type FileSpec struct {
	Path      string
	Spec      string
	Recursive bool
}

// Component is a unit a writer backs up as a whole, such as a database.
type Component struct {
	Name  string
	Files []FileSpec
}

// Writer is what one VSS writer reports about itself.
type Writer struct {
	ID    string
	Name  string
	Usage string // as the writer states it, for example BOOTABLE_SYSTEM_STATE
	// Components the writer wants backed up.
	Components []Component
	// Excludes are files the writer asks backups to leave out.
	Excludes []FileSpec
}

// systemWriters are the writers whose files make up the system state,
// besides every writer that declares itself bootable system state. The
// list follows what Windows Server Backup includes.
var systemWriters = map[string]bool{
	"system writer":                  true,
	"registry writer":                true,
	"com+ regdb writer":              true,
	"wmi writer":                     true,
	"ntds":                           true, // Active Directory database and logs
	"dfs replication service writer": true, // SYSVOL
	"frs writer":                     true, // SYSVOL on old domains
	"certificate authority":          true,
	"iis config writer":              true,
	"iis metabase writer":            true,
	"task scheduler writer":          true,
	"performance counters writer":    true,
	"vss metadata store writer":      true,
	"cluster database":               true,
	"cluster service writer":         true,
	"dhcp jet writer":                true,
	"wins jet writer":                true,
	"ts gateway writer":              true,
	"nps vss writer":                 true,
	"remote desktop services writer": true,
	"terminal services licensing":    true,
}

// IsSystemState reports whether a writer's files belong to the system
// state.
func (w Writer) IsSystemState() bool {
	u := strings.ToLower(strings.NewReplacer("_", "", " ", "").Replace(w.Usage))
	return strings.Contains(u, "bootablesystemstate") || systemWriters[strings.ToLower(strings.TrimSpace(w.Name))]
}

// IsActiveDirectory reports whether the writer is the one for the Active
// Directory database, which only exists on a domain controller.
func (w Writer) IsActiveDirectory() bool { return strings.EqualFold(strings.TrimSpace(w.Name), "NTDS") }

// parseWriter reads the XML document a writer hands to a requester. The
// document's layout is fixed by VSS: an IDENTIFICATION element, file
// lists under BACKUP_LOCATIONS grouped into components, and exclusions
// under EXCLUDE_FILES.
func parseWriter(doc string) (Writer, error) {
	var w Writer
	d := xml.NewDecoder(strings.NewReader(doc))
	d.Strict = false
	var stack []string
	inside := func(name string) bool {
		for _, s := range stack {
			if s == name {
				return true
			}
		}
		return false
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return w, nil
		}
		if err != nil {
			return w, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToUpper(t.Name.Local)
			attr := map[string]string{}
			for _, a := range t.Attr {
				attr[strings.ToLower(a.Name.Local)] = a.Value
			}
			switch {
			case name == "IDENTIFICATION":
				w.ID, w.Name, w.Usage = attr["writerid"], attr["friendlyname"], attr["usage"]
			case inside("BACKUP_LOCATIONS") && attr["componentname"] != "":
				w.Components = append(w.Components, Component{Name: attr["componentname"]})
			case attr["path"] != "":
				// Files that were moved aside are read from where they
				// are now.
				p := attr["path"]
				if alt := attr["alternatepath"]; alt != "" {
					p = alt
				}
				fs := FileSpec{Path: p, Spec: attr["filespec"], Recursive: yes(attr["recursive"])}
				switch {
				case inside("EXCLUDE_FILES"):
					w.Excludes = append(w.Excludes, fs)
				case inside("BACKUP_LOCATIONS") && len(w.Components) > 0:
					c := &w.Components[len(w.Components)-1]
					c.Files = append(c.Files, fs)
				}
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
}

func yes(v string) bool {
	v = strings.ToLower(v)
	return v == "yes" || v == "true" || v == "1"
}

// SystemState gathers the file lists of the writers that make up the
// system state, and the names of those writers.
func SystemState(writers []Writer) (files, excludes []FileSpec, names []string) {
	for _, w := range writers {
		// Every writer's exclusions apply: they name files that must
		// not be restored from a backup, whoever backs them up.
		excludes = append(excludes, w.Excludes...)
		if !w.IsSystemState() {
			continue
		}
		n := 0
		for _, c := range w.Components {
			files = append(files, c.Files...)
			n += len(c.Files)
			if !strings.EqualFold(strings.TrimSpace(w.Name), "Registry Writer") {
				continue
			}
			// The registry writer lists the hives but not their logs,
			// and a hive copied from a snapshot can hold changes that
			// are only complete with its logs. Windows refuses to load
			// such a hive on its own, so the logs come along.
			for _, f := range c.Files {
				if f.Spec != "" && !strings.ContainsAny(f.Spec, "*?") && !f.Recursive {
					files = append(files, FileSpec{Path: f.Path, Spec: f.Spec + ".LOG*"})
				}
			}
		}
		if n > 0 {
			names = append(names, w.Name)
		}
	}
	sort.Strings(names)
	return files, excludes, names
}

// Selection answers which files a set of file lists covers. Paths are
// Windows paths and compare without regard to case.
type Selection struct {
	exact     map[string][]string // folder → name patterns
	recursive map[string][]string
	ancestors map[string]bool
	exExact   map[string][]string
	exRecur   map[string][]string
	roots     map[string]bool
}

func norm(p string) string {
	p = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "/", `\`))
	p = strings.TrimPrefix(p, `\\?\`)
	if len(p) > 3 {
		p = strings.TrimRight(p, `\`)
	}
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	return p
}

// onDrive reports whether p is a path on a lettered drive.
func onDrive(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '\\' && (p[0] >= 'a' && p[0] <= 'z')
}

func parent(p string) string {
	i := strings.LastIndex(p, `\`)
	if i < 0 || len(p) <= 3 {
		return ""
	}
	if i == 2 {
		return p[:3]
	}
	return p[:i]
}

func pattern(spec string) string {
	spec = strings.ToLower(strings.TrimSpace(spec))
	// To Windows "*.*" means every file, with or without a dot.
	if spec == "" || spec == "*.*" {
		return "*"
	}
	return spec
}

// NewSelection builds a selection from file lists and exclusions. File
// lists that are not on a lettered drive are ignored.
func NewSelection(files, excludes []FileSpec) *Selection {
	s := &Selection{exact: map[string][]string{}, recursive: map[string][]string{}, ancestors: map[string]bool{},
		exExact: map[string][]string{}, exRecur: map[string][]string{}, roots: map[string]bool{}}
	for _, f := range files {
		p := norm(f.Path)
		if !onDrive(p) {
			continue
		}
		if f.Recursive {
			s.recursive[p] = append(s.recursive[p], pattern(f.Spec))
		} else {
			s.exact[p] = append(s.exact[p], pattern(f.Spec))
		}
		s.roots[p[:3]] = true
		for a := parent(p); a != ""; a = parent(a) {
			if s.ancestors[a] {
				break
			}
			s.ancestors[a] = true
		}
	}
	for _, f := range excludes {
		p := norm(f.Path)
		if !onDrive(p) {
			continue
		}
		if f.Recursive {
			s.exRecur[p] = append(s.exRecur[p], pattern(f.Spec))
		} else {
			s.exExact[p] = append(s.exExact[p], pattern(f.Spec))
		}
	}
	return s
}

// Roots lists the drives that hold selected files, as `C:\`.
func (s *Selection) Roots() []string {
	var out []string
	for r := range s.roots {
		out = append(out, strings.ToUpper(r))
	}
	sort.Strings(out)
	return out
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		// The patterns hold no backslashes, so path.Match treats the
		// name as one element on every system.
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func covered(exact, recursive map[string][]string, dir, name string) bool {
	if matchAny(exact[dir], name) {
		return true
	}
	if len(recursive) == 0 {
		return false
	}
	for d := dir; d != ""; d = parent(d) {
		if matchAny(recursive[d], name) {
			return true
		}
	}
	return false
}

// Match reports whether a backup should take the file at p, or, for a
// folder, whether it should look inside.
func (s *Selection) Match(p string, dir bool) bool {
	p = norm(p)
	if !onDrive(p) {
		return false
	}
	if dir {
		if s.ancestors[p] || len(s.exact[p]) > 0 {
			return true
		}
		for d := p; d != ""; d = parent(d) {
			if len(s.recursive[d]) > 0 {
				return true
			}
		}
		return false
	}
	d := parent(p)
	if d == "" {
		return false
	}
	name := p[strings.LastIndex(p, `\`)+1:]
	if !covered(s.exact, s.recursive, d, name) {
		return false
	}
	return !covered(s.exExact, s.exRecur, d, name)
}
