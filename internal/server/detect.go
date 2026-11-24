package server

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ajthom90/better-backup/internal/keys"
	"github.com/ajthom90/better-backup/internal/tree"
)

// This file looks at a finished backup for the marks ransomware leaves on
// a machine's files. The server does the looking itself, on what it was
// sent, and compares with the backup before: the machine that was
// attacked is not asked.
//
// What it looks for:
//
//   - files replaced by copies with a new ending (report.docx gone,
//     report.docx.locked there instead);
//   - the same new file, with a name like a ransom note's, in many folders;
//   - most files changed or gone since the last backup;
//   - new data that does not compress, as encrypted data does not, in an
//     amount far beyond what the policy's backups usually add.
//
// Each of these also happens for honest reasons, so a finding does not
// fail the backup. It is kept, marked, and people are told; the one
// thing the server does on its own is to stop forgetting older backups
// of that policy until someone has looked.

// jobData is what the server measured of the chunks a job uploaded.
type jobData struct {
	raw, wire int64 // bytes before and after the agent's compression
}

// Thresholds. Variables so that tests can reach them.
var (
	// detectMinFiles: backups of fewer files are not judged by counts.
	detectMinFiles int64 = 50
	// detectVisit caps how many directory entries a comparison reads.
	detectVisit int64 = 2_000_000
	// detectBurstBytes: new data below this is never a burst.
	detectBurstBytes int64 = 1 << 30
	// detectDenseBytes: compression is judged on at least this much.
	detectDenseBytes int64 = 16 << 20
)

// noteWords are what the names of ransom notes have in common.
var noteWords = []string{"decrypt", "ransom", "recover_", "recover-", "recovery_", "restore_files", "restore-files", "restore_my", "restore-my",
	"how_to", "how-to", "how to", "howto", "read_me", "read-me", "readme_", "_readme", "!readme", "readme!", "unlock", "help_", "your_files", "your files", "#decrypt", "instructions"}

// lockedEndings are endings ransomware families give the files they
// encrypt.
var lockedEndings = []string{".locked", ".lock", ".encrypted", ".enc", ".crypt", ".crypted", ".crypto", ".lockbit", ".wncry", ".wcry", ".ryuk", ".conti", ".locky", ".cerber",
	".zepto", ".odin", ".thor", ".aesir", ".osiris", ".akira", ".blackcat", ".alphv", ".hive", ".royal", ".play", ".clop", ".medusa", ".rhysida", ".babuk", ".darkside",
	".revil", ".sodinokibi", ".phobos", ".dharma", ".stop", ".djvu", ".makop", ".globeimposter", ".maze", ".egregor", ".netwalker", ".ragnar", ".avaddon", ".blackbasta", ".basta", ".pay2key"}

func looksLikeNote(name string) bool {
	n := strings.ToLower(name)
	for _, w := range noteWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// treeDiff is what changed between two backups of the same sources.
type treeDiff struct {
	added, removed, modified int64
	// renamed counts removed files that came back in the same folder
	// under their old name with a new ending, by that ending.
	renamed  int64
	endings  map[string]int64
	notes    map[string]int // new file name -> folders it appeared in
	locked   int64          // new files with an ending ransomware uses
	visited  int64
	partial  bool
	readFail error
}

func sameIDs(a, b []keys.ID) bool { return slices.Equal(a, b) }

func sameContent(a, b *tree.Entry) bool {
	return a.Size == b.Size && sameIDs(a.Content, b.Content) && sameIDs(a.ContentIndex, b.ContentIndex)
}

// ending returns what follows the last dot of a name, with the dot.
func ending(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return strings.ToLower(name[i:])
	}
	return ""
}

func stem(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// compareDirs compares two versions of one folder, given as their page
// lists, and goes into the folders that are in both and differ.
func (d *treeDiff) compareDirs(get tree.Getter, old, cur []keys.ID) {
	if d.partial || d.readFail != nil || sameIDs(old, cur) {
		return
	}
	read := func(pages []keys.ID) map[string]*tree.Entry {
		m := map[string]*tree.Entry{}
		for _, p := range pages {
			es, err := tree.ReadPage(get, p)
			if err != nil {
				d.readFail = err
				return m
			}
			d.visited += int64(len(es))
			for i := range es {
				m[es[i].Name] = &es[i]
			}
		}
		return m
	}
	was, is := read(old), read(cur)
	if d.readFail != nil {
		return
	}
	if d.visited > detectVisit {
		d.partial = true
	}
	// New names, by everything that stands before one of their dots:
	// "a.docx.locked" is found under "a.docx" and under "a".
	fresh := map[string]string{}
	for name, e := range is {
		o := was[name]
		switch {
		case o == nil && e.Type == tree.TypeFile:
			d.added++
			if looksLikeNote(name) {
				d.notes[name]++
			}
			if slices.Contains(lockedEndings, ending(name)) {
				d.locked++
			}
			for i := len(name) - 1; i > 0; i-- {
				if name[i] == '.' {
					fresh[name[:i]] = name
				}
			}
		case o != nil && e.Type == tree.TypeFile && o.Type == tree.TypeFile:
			if !sameContent(o, e) {
				d.modified++
			}
		case o != nil && e.Type == tree.TypeDir && o.Type == tree.TypeDir:
			d.compareDirs(get, o.Subtree, e.Subtree)
		}
	}
	for name, o := range was {
		if o.Type != tree.TypeFile || is[name] != nil {
			continue
		}
		d.removed++
		to, ok := fresh[name] // report.docx -> report.docx.locked
		if !ok {
			to, ok = fresh[stem(name)] // report.docx -> report.locked
		}
		if ok {
			d.renamed++
			d.endings[ending(to)]++
		}
	}
}

func compareSnapshots(get tree.Getter, old, cur *tree.Snapshot) *treeDiff {
	d := &treeDiff{endings: map[string]int64{}, notes: map[string]int{}}
	for i := range cur.Sources {
		e := &cur.Sources[i]
		if e.Type != tree.TypeDir {
			continue
		}
		for k := range old.Sources {
			if o := &old.Sources[k]; o.Name == e.Name && o.Type == tree.TypeDir {
				d.compareDirs(get, o.Subtree, e.Subtree)
			}
		}
	}
	return d
}

func count(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// inspect says what about a new backup looks like ransomware. parent is
// the policy's backup before it, history all its earlier ones, and data
// what the server measured of the upload.
func inspect(get tree.Getter, sn, parent *tree.Snapshot, history []*tree.Snapshot, data jobData) []string {
	if parent == nil {
		return nil
	}
	var reasons []string
	dense := data.raw >= detectDenseBytes && float64(data.wire) >= 0.95*float64(data.raw)
	before := parent.Stats.Files
	if before >= detectMinFiles {
		d := compareSnapshots(get, parent, sn)
		if d.renamed >= 20 && (d.renamed*10 >= before || d.renamed >= 1000) {
			top, n := "", int64(0)
			for e, c := range d.endings {
				if c > n || (c == n && e < top) {
					top, n = e, c
				}
			}
			r := fmt.Sprintf("%s were replaced by files of the same name with a new ending", count(d.renamed, "file", "files"))
			if top != "" && n*2 >= d.renamed {
				r += fmt.Sprintf(", most often “%s”", top)
			}
			reasons = append(reasons, r+".")
		} else if d.locked >= 20 {
			reasons = append(reasons, fmt.Sprintf("%s have an ending that ransomware gives the files it encrypts.", count(d.locked, "new file", "new files")))
		}
		var notes []string
		for name, dirs := range d.notes {
			if dirs >= 5 {
				notes = append(notes, fmt.Sprintf("“%s” in %d folders", name, dirs))
			}
		}
		if len(notes) > 0 {
			sort.Strings(notes)
			if len(notes) > 3 {
				notes = notes[:3]
			}
			reasons = append(reasons, "A new file with a name like a ransom note's appeared in many folders: "+strings.Join(notes, ", ")+".")
		}
		touched := d.modified + d.removed
		switch {
		case touched >= detectMinFiles && touched*2 >= before:
			reasons = append(reasons, fmt.Sprintf("%d of the %d files in the backup before were changed or are gone.", touched, before))
		case touched >= detectMinFiles && touched*4 >= before && dense:
			reasons = append(reasons, fmt.Sprintf("%d of the %d files in the backup before were changed or are gone, and the new data does not compress, as encrypted files do not.", touched, before))
		}
		if now := sn.Stats.Files; now*2 < before && len(reasons) == 0 {
			reasons = append(reasons, fmt.Sprintf("The backup holds %d files; the one before held %d.", now, before))
		}
	}
	// A burst of data that does not compress: what an encrypted disk
	// or share looks like when it is backed up as an image, or when the
	// files kept their names.
	if len(reasons) == 0 && dense && len(history) >= 3 && data.raw >= detectBurstBytes {
		var usual []int64
		for _, h := range history {
			usual = append(usual, h.Stats.BytesUploaded)
		}
		slices.Sort(usual)
		usual = usual[:len(usual)-1] // the largest is the first, full backup
		med := usual[len(usual)/2]
		if med < 1 {
			med = 1
		}
		if data.raw >= 5*med {
			reasons = append(reasons, fmt.Sprintf("It stored %s of new data, %d times what a backup of this policy usually adds, and the data does not compress, as encrypted data does not.", byteSize(data.raw), data.raw/med))
		}
	}
	return reasons
}
