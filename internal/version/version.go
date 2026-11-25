// Package version reads and compares the version numbers of
// better-backup. They follow Semantic Versioning 2.0.0:
//
//	1.4.2          a release (the "stable" channel)
//	1.5.0-rc.1     a release candidate
//	1.4.3-edge.12  the twelfth build from the main branch after 1.4.2
//	               (the "edge" channel)
//
// docs/versioning.md has the whole policy.
package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// V is a parsed version.
type V struct {
	Major, Minor, Patch int
	// Pre are the dot-separated parts after "-": "rc", "1".
	Pre []string
	// Build is what follows "+". It takes no part in comparisons.
	Build string
}

var pattern = regexp.MustCompile(`^v?(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})(?:[-~]([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z.-]+))?$`)

// Parse reads a version, with or without a leading "v". Linux packages
// write the "-" before a pre-release as "~"; that is read too.
func Parse(s string) (V, bool) {
	m := pattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return V{}, false
	}
	var v V
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
	}
	v.Build = m[5]
	return v, true
}

// Valid says whether s is a version.
func Valid(s string) bool { _, ok := Parse(s); return ok }

// String is the version without a leading "v".
func (v V) String() string {
	s := v.Core()
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Core is the three numbers alone.
func (v V) Core() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Stable says whether v is a release, not a pre-release or an edge
// build.
func (v V) Stable() bool { return len(v.Pre) == 0 }

// Edge says whether v is a build from the main branch.
func (v V) Edge() bool {
	for i, p := range v.Pre {
		if p == "edge" && i < len(v.Pre)-1 {
			return true
		}
	}
	return false
}

func number(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, _ := strconv.Atoi(s)
	return n, true
}

// Compare returns -1, 0 or 1 as a is older than, the same as or newer
// than b, by the rules of Semantic Versioning: a pre-release is older
// than its release, and numeric parts compare as numbers.
func Compare(a, b V) int {
	for _, d := range [3]int{a.Major - b.Major, a.Minor - b.Minor, a.Patch - b.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		x, xn := number(a.Pre[i])
		y, yn := number(b.Pre[i])
		switch {
		case xn && yn:
			if x != y {
				return sign(x - y)
			}
		case xn:
			return -1 // numbers sort before words
		case yn:
			return 1
		default:
			if c := strings.Compare(a.Pre[i], b.Pre[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(a.Pre) - len(b.Pre))
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// Newer says whether version a is newer than version b. A version that
// does not parse (a build from source says "dev") is neither newer nor
// older than anything.
func Newer(a, b string) bool {
	x, ok1 := Parse(a)
	y, ok2 := Parse(b)
	return ok1 && ok2 && Compare(x, y) > 0
}

// Same says whether a and b are the same version, however they are
// written ("v1.2.0", "1.2.0", "1.2.0~rc.1" for "1.2.0-rc.1").
func Same(a, b string) bool {
	x, ok1 := Parse(a)
	y, ok2 := Parse(b)
	if !ok1 || !ok2 {
		return a == b
	}
	return Compare(x, y) == 0
}

// Bump returns the release that follows v: kind is "major", "minor" or
// "patch". A pre-release becomes its own release when that fits the
// kind: 1.3.0-rc.1 bumped by "minor" is 1.3.0.
func (v V) Bump(kind string) (V, error) {
	n := V{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
	pre := len(v.Pre) > 0
	switch kind {
	case "major":
		if !(pre && v.Minor == 0 && v.Patch == 0) {
			n = V{Major: v.Major + 1}
		}
	case "minor":
		if !(pre && v.Patch == 0) {
			n = V{Major: v.Major, Minor: v.Minor + 1}
		}
	case "patch":
		if !pre {
			n.Patch++
		}
	default:
		return V{}, fmt.Errorf("%q is not major, minor or patch", kind)
	}
	return n, nil
}

// EdgeAfter is the version of the build n commits after the tagged
// version last: the next patch release's pre-release "edge.n", so that
// it is newer than last and older than whatever release comes next.
// After a pre-release such as 1.3.0-rc.1 it is 1.3.0-rc.1.edge.n.
func EdgeAfter(last V, n int) V {
	e := V{Major: last.Major, Minor: last.Minor, Patch: last.Patch}
	if len(last.Pre) > 0 {
		e.Pre = append(append([]string{}, last.Pre...), "edge", strconv.Itoa(n))
		return e
	}
	e.Patch++
	e.Pre = []string{"edge", strconv.Itoa(n)}
	return e
}

// First is the version the builds before the first release count
// towards.
var First = V{Minor: 1}

// EdgeBeforeFirst is the version of build n when nothing was released
// yet.
func EdgeBeforeFirst(n int) V {
	return V{Minor: First.Minor, Pre: []string{"edge", strconv.Itoa(n)}}
}

// MSI is the version a Windows installer carries. Windows Installer
// compares the first three numbers only; a pre-release puts its last
// number in the fourth, which Windows shows and does not compare. The
// installers therefore allow one build of a version to replace another
// of the same three numbers (AllowSameVersionUpgrades), and refuse to
// go back to lower ones.
func (v V) MSI() string {
	if len(v.Pre) == 0 {
		return v.Core()
	}
	n := 0
	for _, p := range v.Pre {
		if x, ok := number(p); ok {
			n = x
		}
	}
	if n > 65535 {
		n = 65535
	}
	return fmt.Sprintf("%s.%d", v.Core(), n)
}

// DockerTags are the tags an image of this version is published under.
// newest says that no higher release exists, which is what "latest" and
// the shorter tags promise.
//
//	1.4.2          1.4.2, 1.4, 1, latest   (1.4 and 1 only when newest of their line)
//	0.4.2          0.4.2, 0.4, latest      (no "0" tag: 0.x releases may break each other)
//	1.5.0-rc.1     1.5.0-rc.1
//	1.4.3-edge.12  1.4.3-edge.12, edge
func (v V) DockerTags(newestOfMinor, newestOfMajor, newest bool) []string {
	tags := []string{strings.SplitN(v.String(), "+", 2)[0]}
	switch {
	case v.Edge():
		tags = append(tags, "edge")
	case v.Stable():
		if newestOfMinor {
			tags = append(tags, fmt.Sprintf("%d.%d", v.Major, v.Minor))
		}
		if newestOfMajor && v.Major > 0 {
			tags = append(tags, strconv.Itoa(v.Major))
		}
		if newest {
			tags = append(tags, "latest")
		}
	}
	return tags
}
