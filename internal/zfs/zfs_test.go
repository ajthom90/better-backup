//go:build unix

package zfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake is a stand-in for the zfs command: it lists the datasets in
// $FAKE/datasets, makes a snapshot by copying the dataset's folder into
// its .zfs folder, and logs every call.
const fake = `#!/bin/sh
echo "$*" >> "$FAKE/log"
case "$1" in
list) cat "$FAKE/datasets" ;;
get) cat "$FAKE/marked" 2>/dev/null ;;
snapshot)
  [ -e "$FAKE/fail-snapshot" ] && { echo "cannot create snapshots: out of space" >&2; exit 1; }
  shift 3
  for s in "$@"; do
    ds=${s%@*}; tag=${s#*@}
    mp=$(awk -F'\t' -v d="$ds" '$1==d && $2=="filesystem" {print $3}' "$FAKE/datasets")
    [ -d "$mp" ] && [ ! -e "$FAKE/unmounted-$(basename "$ds")" ] || continue
    mkdir -p "$mp/.zfs/snapshot/$tag"
    for f in "$mp"/*; do [ -f "$f" ] && cp "$f" "$mp/.zfs/snapshot/$tag/"; done
  done ;;
destroy) ;;
*) exit 1 ;;
esac
exit 0
`

func TestSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKE", dir)
	bin := filepath.Join(dir, "zfs")
	os.WriteFile(bin, []byte(fake), 0o755)
	Command = bin
	defer func() { Command = "zfs" }()

	tank := filepath.Join(dir, "mnt", "tank")
	docs, photos, locked := filepath.Join(tank, "docs"), filepath.Join(tank, "docs", "photos"), filepath.Join(tank, "docs", "locked")
	for _, d := range []string{photos, locked, filepath.Join(tank, "other")} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(docs, "a.txt"), []byte("before"), 0o644)
	os.WriteFile(filepath.Join(photos, "b.txt"), []byte("before"), 0o644)
	os.WriteFile(filepath.Join(dir, "datasets"), []byte(strings.Join([]string{
		"tank\tfilesystem\t" + tank,
		"tank/docs\tfilesystem\t" + docs,
		"tank/docs/photos\tfilesystem\t" + photos,
		"tank/docs/locked\tfilesystem\t" + locked,
		"tank/other\tfilesystem\t" + filepath.Join(tank, "other"),
		"tank/legacy\tfilesystem\tlegacy",
		"tank/vm\tvolume\t-",
	}, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "unmounted-locked"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "marked"), []byte("tank/docs@old\tagent1\ntank/docs@theirs\tagent2\ntank/vm-better-backup-old\tagent1\n"), 0o644)

	// Everything here is on one file system, so the pool's root dataset
	// counts as a possible holder of the path too. On real ZFS each
	// dataset is its own file system; TestZFS in the server package
	// covers that.
	s, warnings, err := Begin(context.Background(), Options{Agent: "agent1", Tag: "job1", Paths: []string{docs}, Volumes: []string{"/dev/sda1"}})
	if err != nil || len(warnings) != 0 {
		t.Fatal(err, warnings)
	}
	os.WriteFile(filepath.Join(docs, "a.txt"), []byte("after"), 0o644)
	read := func(p string) string { b, _ := os.ReadFile(s.Path(p)); return string(b) }
	if read(filepath.Join(docs, "a.txt")) != "before" || read(filepath.Join(photos, "b.txt")) != "before" {
		t.Fatalf("paths are not read from the snapshots: %s", s.Path(filepath.Join(photos, "b.txt")))
	}
	// A dataset that is not mounted has no snapshot folder; its empty
	// mount point is read from its parent's snapshot.
	if got, want := s.Path(filepath.Join(locked, "x")), filepath.Join(docs, ".zfs", "snapshot", "job1", "locked", "x"); got != want {
		t.Fatalf("path in an unmounted dataset: %s", got)
	}
	if name, dev, zvol, ok := s.Volume("/dev/sda1"); name != "/dev/sda1" || dev != name || zvol || !ok {
		t.Fatal("a device that is not a ZFS volume was changed")
	}
	if got := strings.Join(s.Excludes(), " "); !strings.Contains(got, filepath.Join(photos, ".zfs")) || strings.Contains(got, "other") {
		t.Fatalf("excludes: %s", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	for _, want := range []string{
		"destroy tank/docs@old\n", "destroy tank/vm-better-backup-old\n", // leftovers of this agent
		"snapshot -o " + Property + "=agent1 tank@job1 tank/docs@job1 tank/docs/locked@job1 tank/docs/photos@job1\n",
		"destroy tank/docs/photos@job1\n",
	} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("missing %q in:\n%s", want, log)
		}
	}
	if strings.Contains(string(log), "theirs") || strings.Contains(string(log), "tank/other@") {
		t.Fatalf("touched what it should not:\n%s", log)
	}

	// Live: nothing is frozen, but .zfs folders are still left out.
	os.Remove(filepath.Join(dir, "log"))
	s, _, _ = Begin(context.Background(), Options{Agent: "agent1", Tag: "job2", Paths: []string{docs}, Live: true})
	if len(s.Snapshots()) != 0 || len(s.Excludes()) != 3 || s.Path(docs) != docs {
		t.Fatalf("live session: %v %v", s.Snapshots(), s.Excludes())
	}

	// A failed snapshot is a warning, and files are read live.
	os.WriteFile(filepath.Join(dir, "fail-snapshot"), nil, 0o644)
	s, warnings, err = Begin(context.Background(), Options{Agent: "agent1", Tag: "job3", Paths: []string{docs}, Volumes: []string{"/dev/zvol/tank/vm"}})
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "out of space") || s.Path(docs) != docs {
		t.Fatal(err, warnings)
	}
	if _, _, zvol, ok := s.Volume("/dev/zvol/tank/vm"); !zvol || ok {
		t.Fatal("a volume without a snapshot must not be backed up")
	}
}
