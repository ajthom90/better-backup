// Package changelog keeps the release notes.
//
// Every change that someone who runs better-backup would notice adds a
// small file to changelog.d, named NAME.KIND.md, with one or more lines
// of Markdown that say what changed in plain words. KIND is one of
// added, changed, fixed, removed or security. Separate files cannot
// conflict when several changes are written at the same time.
//
// A release moves those lines into CHANGELOG.md under the new version
// and removes the files. The same text is the body of the GitHub
// release, and the server shows it in the web UI.
package changelog

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Kinds are the kinds of change, in the order they are listed.
var Kinds = []struct{ Kind, Title string }{
	{"security", "Security"},
	{"added", "Added"},
	{"changed", "Changed"},
	{"fixed", "Fixed"},
	{"removed", "Removed"},
}

// Fragment is one file of changelog.d.
type Fragment struct {
	File  string // its name
	Kind  string
	Items []string // the entries, without the leading "- "
}

var fragmentName = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*)\.([a-z]+)\.md$`)

func title(kind string) string {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k.Title
		}
	}
	return ""
}

// ReadFragments reads the entries waiting in dir for the next release.
// README.md is skipped; any other file that is not a well-formed entry
// is an error, so that a misnamed one is not silently left out.
func ReadFragments(fsys fs.FS, dir string) ([]Fragment, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Fragment
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "README.md" || strings.HasPrefix(name, ".") {
			continue
		}
		m := fragmentName.FindStringSubmatch(name)
		if m == nil || title(m[2]) == "" {
			return nil, fmt.Errorf("%s: name it NAME.KIND.md, where KIND is one of %s", path.Join(dir, name), kindList())
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		items := Items(string(data))
		if len(items) == 0 {
			return nil, fmt.Errorf("%s is empty: write what changed, in a line or two", path.Join(dir, name))
		}
		out = append(out, Fragment{File: name, Kind: m[2], Items: items})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

func kindList() string {
	var k []string
	for _, x := range Kinds {
		k = append(k, x.Kind)
	}
	return strings.Join(k, ", ")
}

// Items splits the text of an entry file into entries. A line that
// starts with "- " or "* " starts an entry; so does the first line and
// any line after an empty one. Other lines continue the entry above.
func Items(text string) []string {
	var items []string
	blank := true
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			blank = true
			continue
		case strings.HasPrefix(t, "#"):
			// A heading has no place in an entry.
			blank = true
			continue
		case strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "):
			items = append(items, strings.TrimSpace(t[2:]))
		case blank || len(items) == 0:
			items = append(items, t)
		default:
			items[len(items)-1] += " " + t
		}
		blank = false
	}
	return items
}

// Render writes entries as release notes: a heading for each kind that
// has entries, and the entries as a list. It is empty when there are
// none.
func Render(frags []Fragment) string {
	var b strings.Builder
	for _, k := range Kinds {
		var items []string
		for _, f := range frags {
			if f.Kind == k.Kind {
				items = append(items, f.Items...)
			}
		}
		if len(items) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### " + k.Title + "\n\n")
		for _, it := range items {
			b.WriteString("- " + it + "\n")
		}
	}
	return b.String()
}

var sectionHead = regexp.MustCompile(`(?m)^## +\[?v?([0-9][^\]\s]*)\]?.*$`)

// Section returns the notes of one version from the text of
// CHANGELOG.md.
func Section(changelog, version string) (string, bool) {
	changelog = strings.ReplaceAll(changelog, "\r\n", "\n")
	version = strings.TrimPrefix(version, "v")
	heads := sectionHead.FindAllStringSubmatchIndex(changelog, -1)
	for i, h := range heads {
		if changelog[h[2]:h[3]] != version {
			continue
		}
		end := len(changelog)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		return strings.TrimSpace(changelog[h[1]:end]) + "\n", true
	}
	return "", false
}

// Versions lists the versions CHANGELOG.md has notes for, newest first
// (the order they are written in).
func Versions(changelog string) []string {
	var out []string
	for _, m := range sectionHead.FindAllStringSubmatch(changelog, -1) {
		out = append(out, m[1])
	}
	return out
}

// Insert adds the notes of a new version to the text of CHANGELOG.md,
// above the versions before it. date is YYYY-MM-DD.
func Insert(changelog, version, date, notes string) (string, error) {
	changelog = strings.ReplaceAll(changelog, "\r\n", "\n")
	if _, dup := Section(changelog, version); dup {
		return "", fmt.Errorf("CHANGELOG.md already has version %s", version)
	}
	if strings.TrimSpace(notes) == "" {
		notes = "No changes worth a note.\n"
	}
	section := "## " + version + " - " + date + "\n\n" + strings.TrimSpace(notes) + "\n"
	if loc := sectionHead.FindStringIndex(changelog); loc != nil {
		return changelog[:loc[0]] + section + "\n" + changelog[loc[0]:], nil
	}
	return strings.TrimRight(changelog, "\n") + "\n\n" + section, nil
}
