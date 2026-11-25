package changelog

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestFragments(t *testing.T) {
	fsys := fstest.MapFS{
		"changelog.d/README.md":             {Data: []byte("how to")},
		"changelog.d/update-check.added.md": {Data: []byte("The server says when a newer version is out.\nIt shows what changed.\n\nSecond entry.\n")},
		"changelog.d/a-fix.fixed.md":        {Data: []byte("- One fix\n  over two lines\n- Another fix\n")},
		"changelog.d/sec.security.md":       {Data: []byte("* Closed a hole\n")},
	}
	frags, err := ReadFragments(fsys, "changelog.d")
	if err != nil {
		t.Fatal(err)
	}
	want := "### Security\n\n- Closed a hole\n\n### Added\n\n- The server says when a newer version is out. It shows what changed.\n- Second entry.\n\n### Fixed\n\n- One fix over two lines\n- Another fix\n"
	if got := Render(frags); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	for name, why := range map[string]string{"oops.md": "NAME.KIND.md", "x.feature.md": "KIND is one of", "empty.added.md": "is empty"} {
		bad := fstest.MapFS{"changelog.d/" + name: {Data: []byte(map[bool]string{true: " \n", false: "text"}[name == "empty.added.md"])}}
		if _, err := ReadFragments(bad, "changelog.d"); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if Render(nil) != "" {
		t.Error("nothing should render as nothing")
	}
}

func TestSections(t *testing.T) {
	log := "# Changelog\n\nWhat changed in each release.\n"
	log, err := Insert(log, "0.1.0", "2026-12-01", "### Added\n\n- First\n")
	if err != nil {
		t.Fatal(err)
	}
	log, err = Insert(log, "0.2.0", "2026-12-20", "### Fixed\n\n- Second\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "# Changelog\n\nWhat changed in each release.\n\n## 0.2.0 - 2026-12-20\n\n### Fixed\n\n- Second\n\n## 0.1.0 - 2026-12-01\n\n### Added\n\n- First\n"
	if log != want {
		t.Errorf("got:\n%s", log)
	}
	if got, ok := Section(log, "v0.1.0"); !ok || got != "### Added\n\n- First\n" {
		t.Errorf("0.1.0: %q %v", got, ok)
	}
	if got, ok := Section(log, "0.2.0"); !ok || got != "### Fixed\n\n- Second\n" {
		t.Errorf("0.2.0: %q %v", got, ok)
	}
	if _, ok := Section(log, "0.2"); ok {
		t.Error("0.2 is not a version in it")
	}
	if _, err := Insert(log, "0.2.0", "2026-12-21", "x"); err == nil {
		t.Error("the same version twice")
	}
	if v := Versions(log); strings.Join(v, " ") != "0.2.0 0.1.0" {
		t.Errorf("versions: %v", v)
	}
	if s, _ := Insert("# Changelog\n", "1.0.0", "2027-01-01", ""); !strings.Contains(s, "No changes worth a note.") {
		t.Errorf("empty notes: %s", s)
	}
}
