package restore

import (
	"strings"

	"github.com/ajthom90/better-backup/internal/tree"
)

// SystemState asks for a Windows system state to be put back over the
// running system it came from. Files in use are swapped in when Windows
// restarts, so the restore is complete only after a restart.
type SystemState struct {
	// Registry also replaces the registry hives at the restart.
	Registry bool `json:"registry,omitempty"`
	// AuthoritativeSYSVOL makes this domain controller's restored SYSVOL
	// the copy its replication partners take over. Without it the
	// restored SYSVOL is brought up to date from the partners.
	AuthoritativeSYSVOL bool `json:"authoritative_sysvol,omitempty"`
	// AuthoritativeSubtrees are distinguished names in Active Directory
	// whose restored objects win over the newer ones on other domain
	// controllers: the way to bring back deleted objects.
	AuthoritativeSubtrees []string `json:"authoritative_subtrees,omitempty"`
	// Restart restarts Windows when the restore is done, into normal
	// mode.
	Restart bool `json:"restart,omitempty"`
}

// machineHives are the registry hives every Windows has, by file name
// in System32\config, in the order they are replaced. SYSTEM is last:
// once it is replaced, nothing written to the live registry survives
// the restart.
var machineHives = []string{"SOFTWARE", "SAM", "SECURITY", "DEFAULT", "SYSTEM"}

const (
	fileOrdinary = iota
	fileHive     // a machine hive: staged and swapped in by the registry
	fileSkip     // a hive's log: belongs to the live hive, never restored
)

// classify says how a system state file bound for dest is handled.
// configDir is System32\config.
func classify(configDir, dest string) (kind int, hive string) {
	// These are Windows paths whatever system this runs on.
	i := strings.LastIndex(dest, `\`)
	if i < 0 || !strings.EqualFold(dest[:i], configDir) {
		return fileOrdinary, ""
	}
	name := strings.ToUpper(dest[i+1:])
	for _, h := range machineHives {
		if name == h {
			return fileHive, h
		}
		// SYSTEM.LOG1, SOFTWARE{guid}.TM.blf and the like.
		if strings.HasPrefix(name, h+".") || strings.HasPrefix(name, h+"{") {
			return fileSkip, ""
		}
	}
	return fileOrdinary, ""
}

const (
	keepTree  = iota // the key and everything under it
	keepMerge        // subkeys the backup lacks, and earlier start values
	keepValue        // one value
)

// parseKeep reads one entry of the registry's KeysNotToRestore lists:
// what, in the SYSTEM hive, a restore must leave as it is on the live
// system. A trailing backslash names a key, a trailing asterisk a key
// whose subkeys are merged, anything else a value.
func parseKeep(entry string) (kind int, key, value string, ok bool) {
	entry = strings.TrimSpace(entry)
	switch {
	case entry == "":
		return 0, "", "", false
	case strings.HasSuffix(entry, `*`):
		kind, key = keepMerge, strings.TrimRight(strings.TrimSuffix(entry, `*`), `\`)
	case strings.HasSuffix(entry, `\`):
		kind, key = keepTree, strings.TrimRight(entry, `\`)
	default:
		i := strings.LastIndex(entry, `\`)
		if i < 0 {
			return 0, "", "", false
		}
		kind, key, value = keepValue, entry[:i], entry[i+1:]
	}
	key = strings.TrimLeft(key, `\`)
	return kind, key, value, key != ""
}

// controlSet turns a path that starts at CurrentControlSet into one
// that starts at the named control set. A hive that is not the running
// one has no CurrentControlSet.
func controlSet(path, set string) string {
	const cur = "currentcontrolset"
	if len(path) >= len(cur) && strings.EqualFold(path[:len(cur)], cur) && (len(path) == len(cur) || path[len(cur)] == '\\') {
		return set + path[len(cur):]
	}
	return path
}

// adLog reports whether name is a file the Active Directory database
// engine keeps beside the database: a log, checkpoint or reserve file.
// Ones that are newer than a restored database must go, or the engine
// tries to replay them into it.
func adLog(name string) bool {
	n := strings.ToLower(name)
	return n == "edb.chk" || n == "temp.edb" || n == "edbtmp.log" ||
		strings.HasPrefix(n, "edb") && (strings.HasSuffix(n, ".log") || strings.HasSuffix(n, ".jrs"))
}

// systemState restores the system state entry over the running system.
func (r *restorer) systemState(e *tree.Entry, full bool) error {
	if err := r.sysBegin(); err != nil {
		return err
	}
	for _, p := range e.Subtree {
		drives, err := tree.ReadPage(r.get, p)
		if err != nil {
			return err
		}
		for i := range drives {
			d := &drives[i]
			if len(d.Name) != 1 || d.Type != tree.TypeDir {
				r.warn("system state: skipped %q, which is not a drive", d.Name)
				continue
			}
			orig := r.join(e.Name, d.Name)
			m := matchFull
			if !full {
				if m = r.match(orig); m == matchNone {
					continue
				}
			}
			if err := r.entry(d, orig, d.Name+`:\`, m == matchFull); err != nil {
				return err
			}
		}
	}
	return r.sysFinish()
}
