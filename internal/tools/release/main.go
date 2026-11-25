// Command release works out version numbers and release notes. The
// workflows and the packaging scripts all ask it, so that there is one
// answer. docs/versioning.md describes the policy.
//
//	go run ./internal/tools/release version            the version of this checkout
//	go run ./internal/tools/release next KIND          the release after the last: KIND is patch, minor or major
//	go run ./internal/tools/release msi VERSION        the number a Windows installer carries
//	go run ./internal/tools/release tags VERSION       the Docker tags an image of VERSION gets
//	go run ./internal/tools/release notes [VERSION]    release notes: of VERSION, or what is waiting in changelog.d
//	go run ./internal/tools/release check              that the entries in changelog.d are well formed
//	go run ./internal/tools/release prepare VERSION    move the entries into CHANGELOG.md as VERSION
//
// It is run from the root of the repository.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/better-backup/internal/changelog"
	"github.com/ajthom90/better-backup/internal/version"
)

func main() {
	out, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	fmt.Print(out)
}

func run(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: release version | next KIND | msi VERSION | tags VERSION | notes [VERSION] | check | prepare VERSION")
	}
	arg := func(i int) string {
		if len(args) > i {
			return args[i]
		}
		return ""
	}
	switch args[0] {
	case "version":
		v, err := current()
		if err != nil {
			return "", err
		}
		return v.String() + "\n", nil
	case "next":
		v, err := next(arg(1))
		if err != nil {
			return "", err
		}
		return v.String() + "\n", nil
	case "msi":
		v, ok := version.Parse(arg(1))
		if !ok {
			return "", fmt.Errorf("%q is not a version such as 1.2.0", arg(1))
		}
		return v.MSI() + "\n", nil
	case "tags":
		v, ok := version.Parse(arg(1))
		if !ok {
			return "", fmt.Errorf("%q is not a version such as 1.2.0", arg(1))
		}
		rel, err := released()
		if err != nil {
			return "", err
		}
		return strings.Join(tagsFor(v, rel), "\n") + "\n", nil
	case "notes":
		return notes(arg(1))
	case "check":
		frags, err := changelog.ReadFragments(os.DirFS("."), "changelog.d")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d entries for the next release\n", len(frags)), nil
	case "prepare":
		return prepare(arg(1), time.Now().UTC())
	}
	return "", fmt.Errorf("unknown command %q", args[0])
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// released lists the versions that have a tag, oldest first.
func released() ([]version.V, error) {
	out, err := git("tag", "--list", "v[0-9]*")
	if err != nil {
		return nil, err
	}
	var vs []version.V
	for _, t := range strings.Fields(out) {
		if v, ok := version.Parse(t); ok {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return version.Compare(vs[i], vs[j]) < 0 })
	return vs, nil
}

// current is the version of the checked-out commit: its tag if it has a
// version tag, and otherwise an edge build counted from the newest
// version tag behind it.
func current() (version.V, error) {
	if shallow, _ := git("rev-parse", "--is-shallow-repository"); shallow == "true" {
		return version.V{}, errors.New("this checkout has only part of the history, which the version is counted from; fetch all of it (actions/checkout: fetch-depth: 0)")
	}
	// The newest version among the tags on this commit.
	if out, err := git("tag", "--points-at", "HEAD", "--list", "v[0-9]*"); err == nil && out != "" {
		var best *version.V
		for _, t := range strings.Fields(out) {
			if v, ok := version.Parse(t); ok && (best == nil || version.Compare(v, *best) > 0) {
				best = &v
			}
		}
		if best != nil {
			return *best, nil
		}
	}
	// The newest version among the tags behind it. git describe would
	// take the nearest instead, which is not the same after a merge.
	out, err := git("tag", "--merged", "HEAD", "--list", "v[0-9]*")
	if err != nil {
		return version.V{}, err
	}
	var last *version.V
	var lastTag string
	for _, t := range strings.Fields(out) {
		if v, ok := version.Parse(t); ok && (last == nil || version.Compare(v, *last) > 0) {
			last, lastTag = &v, t
		}
	}
	if last == nil {
		n, err := count("HEAD")
		if err != nil {
			return version.V{}, err
		}
		return version.EdgeBeforeFirst(n), nil
	}
	n, err := count(lastTag + "..HEAD")
	if err != nil {
		return version.V{}, err
	}
	return version.EdgeAfter(*last, n), nil
}

func count(revs string) (int, error) {
	out, err := git("rev-list", "--count", revs)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// next is the release that follows the newest one.
func next(kind string) (version.V, error) {
	rel, err := released()
	if err != nil {
		return version.V{}, err
	}
	if len(rel) == 0 {
		if kind != "major" && kind != "minor" && kind != "patch" {
			return version.V{}, fmt.Errorf("%q is not major, minor or patch", kind)
		}
		if kind == "major" {
			return version.V{Major: 1}, nil
		}
		return version.First, nil
	}
	return rel[len(rel)-1].Bump(kind)
}

// tagsFor is version.DockerTags, told what else was released.
func tagsFor(v version.V, rel []version.V) []string {
	minor, major, all := true, true, true
	for _, r := range rel {
		if !r.Stable() || version.Compare(r, v) <= 0 {
			continue
		}
		all = false
		if r.Major == v.Major {
			major = false
			if r.Minor == v.Minor {
				minor = false
			}
		}
	}
	return v.DockerTags(minor, major, all)
}

func notes(ver string) (string, error) {
	if ver != "" {
		if _, ok := version.Parse(ver); !ok {
			return "", fmt.Errorf("%q is not a version such as 1.2.0", ver)
		}
		log, err := os.ReadFile("CHANGELOG.md")
		if err != nil {
			return "", err
		}
		v, _ := version.Parse(ver)
		if s, ok := changelog.Section(string(log), v.String()); ok {
			return s, nil
		}
	}
	frags, err := changelog.ReadFragments(os.DirFS("."), "changelog.d")
	if err != nil {
		return "", err
	}
	if s := changelog.Render(frags); s != "" {
		return s, nil
	}
	return "No changes worth a note.\n", nil
}

// prepare moves the entries of changelog.d into CHANGELOG.md under a
// new version, and removes them.
func prepare(ver string, now time.Time) (string, error) {
	v, ok := version.Parse(ver)
	if !ok {
		return "", fmt.Errorf("%q is not a version such as 1.2.0", ver)
	}
	if v.Edge() || v.Build != "" {
		return "", fmt.Errorf("%s is not a version to release", v)
	}
	rel, err := released()
	if err != nil {
		return "", err
	}
	for _, r := range rel {
		if version.Compare(r, v) == 0 {
			return "", fmt.Errorf("version %s was released already", v)
		}
	}
	frags, err := changelog.ReadFragments(os.DirFS("."), "changelog.d")
	if err != nil {
		return "", err
	}
	log, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		return "", err
	}
	// A release candidate has the notes too, and leaves the entries for
	// the release itself.
	if !v.Stable() {
		return v.String() + "\n", nil
	}
	text, err := changelog.Insert(string(log), v.String(), now.Format("2006-01-02"), changelog.Render(frags))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile("CHANGELOG.md", []byte(text), 0o644); err != nil {
		return "", err
	}
	for _, f := range frags {
		if err := os.Remove(filepath.Join("changelog.d", f.File)); err != nil {
			return "", err
		}
	}
	return v.String() + "\n", nil
}
