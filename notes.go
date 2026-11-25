// Package betterbackup holds the release notes, so that every program
// built from this repository carries them.
package betterbackup

import (
	"embed"

	"github.com/ajthom90/better-backup/internal/changelog"
)

// CHANGELOG.md has the notes of every release, and changelog.d the
// entries for the next one. See internal/changelog.
//
//go:embed CHANGELOG.md changelog.d
var notes embed.FS

// Changelog is the text of CHANGELOG.md.
func Changelog() string {
	b, _ := notes.ReadFile("CHANGELOG.md")
	return string(b)
}

// Unreleased is what changed since the last release, as Markdown.
func Unreleased() string {
	frags, err := changelog.ReadFragments(notes, "changelog.d")
	if err != nil {
		return ""
	}
	return changelog.Render(frags)
}

// ReleaseNotes is what is new in a version, as Markdown: its section of
// CHANGELOG.md or, for a build between releases, what changed since the
// last one.
func ReleaseNotes(version string) string {
	if s, ok := changelog.Section(Changelog(), version); ok {
		return s
	}
	return Unreleased()
}
