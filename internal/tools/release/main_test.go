package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// repo makes an empty git repository and works in it.
func repo(t *testing.T) func(args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	g := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g("init", "-q", "-b", "main")
	os.Mkdir("changelog.d", 0o755)
	os.WriteFile("changelog.d/README.md", []byte("how"), 0o644)
	os.WriteFile("CHANGELOG.md", []byte("# Changelog\n"), 0o644)
	g("add", ".")
	g("commit", "-q", "-m", "first")
	return g
}

func want(t *testing.T, wanted string, args ...string) {
	t.Helper()
	got, err := run(args)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	if strings.TrimSpace(got) != wanted {
		t.Errorf("%v = %q, want %q", args, strings.TrimSpace(got), wanted)
	}
}

func TestVersions(t *testing.T) {
	g := repo(t)
	commit := func() { g("commit", "-q", "--allow-empty", "-m", "more") }
	commit()
	want(t, "0.1.0-edge.2", "version")
	want(t, "0.1.0", "next", "minor")
	want(t, "0.1.0", "next", "patch")
	want(t, "1.0.0", "next", "major")

	g("tag", "v0.1.0")
	want(t, "0.1.0", "version")
	want(t, "0.1.0\n0.1\nlatest", "tags", "0.1.0")
	commit()
	commit()
	want(t, "0.1.1-edge.2", "version")
	want(t, "0.1.1-edge.2\nedge", "tags", "0.1.1-edge.2")
	want(t, "0.1.1.2", "msi", "0.1.1-edge.2")
	want(t, "0.2.0", "next", "minor")

	g("tag", "v0.2.0-rc.1")
	want(t, "0.2.0-rc.1", "version")
	want(t, "0.2.0-rc.1", "tags", "0.2.0-rc.1")
	commit()
	want(t, "0.2.0-rc.1.edge.1", "version")
	want(t, "0.2.0", "next", "minor")
	g("tag", "v0.2.0")
	g("tag", "v1.0.0")
	want(t, "1.0.0", "version")
	// A fix for an older line does not take "latest" from a newer one.
	want(t, "0.2.1\n0.2", "tags", "0.2.1")
	want(t, "1.0.1\n1.0\n1\nlatest", "tags", "1.0.1")
	if _, err := run([]string{"prepare", "1.0.0"}); err == nil || !strings.Contains(err.Error(), "released already") {
		t.Errorf("releasing a version twice: %v", err)
	}
}

func TestPrepare(t *testing.T) {
	repo(t)
	want(t, "No changes worth a note.", "notes")
	os.WriteFile("changelog.d/one.added.md", []byte("A new thing.\n"), 0o644)
	os.WriteFile("changelog.d/two.fixed.md", []byte("- A fix.\n"), 0o644)
	want(t, "2 entries for the next release", "check")
	notes := "### Added\n\n- A new thing.\n\n### Fixed\n\n- A fix."
	want(t, notes, "notes")
	want(t, notes, "notes", "0.1.0")

	// A release candidate leaves the entries for the release.
	want(t, "0.1.0-rc.1", "prepare", "v0.1.0-rc.1")
	want(t, "2 entries for the next release", "check")

	want(t, "0.1.0", "prepare", "0.1.0")
	want(t, "0 entries for the next release", "check")
	want(t, notes, "notes", "v0.1.0")
	log, _ := os.ReadFile("CHANGELOG.md")
	if !strings.HasPrefix(string(log), "# Changelog\n\n## 0.1.0 - 20") {
		t.Errorf("CHANGELOG.md:\n%s", log)
	}
	if _, err := os.Stat("changelog.d/README.md"); err != nil {
		t.Error("the README went too")
	}

	os.WriteFile("changelog.d/bad.md", []byte("x"), 0o644)
	if _, err := run([]string{"check"}); err == nil {
		t.Error("a misnamed entry passed")
	}
	if _, err := run([]string{"prepare", "0.1.1-edge.3"}); err == nil {
		t.Error("released an edge build")
	}
}
