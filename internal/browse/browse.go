// Package browse lists what is on the machine an agent runs on: its
// folders, the volumes and disks that can be imaged, and the virtual
// machines of its hypervisor. The web UI shows these when a policy is
// written, so that nobody has to look them up on the machine.
package browse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ajthom90/better-backup/internal/api"
	"github.com/ajthom90/better-backup/internal/hypervisor"
	"github.com/ajthom90/better-backup/internal/logring"
)

// maxEntries bounds one answer. A folder with more files than this is
// cut short, folders first, so the way further down is never lost.
const maxEntries = 2000

// Run answers one request. It never fails: what went wrong is in the
// result, for the person looking at the page.
func Run(ctx context.Context, req *api.BrowseRequest) *api.BrowseResult {
	var (
		res *api.BrowseResult
		err error
	)
	switch req.Kind {
	case api.BrowseFolders:
		res, err = Folders(req.Path)
	case api.BrowseVolumes:
		res = new(api.BrowseResult)
		res.Entries, err = volumes()
	case api.BrowseDisks:
		res = new(api.BrowseResult)
		res.Entries, err = disks()
	case api.BrowseVMs:
		res = new(api.BrowseResult)
		res.Entries, err = machines(ctx, req)
	case api.BrowseLog:
		res = &api.BrowseResult{Text: logring.Text()}
	default:
		err = fmt.Errorf("this agent version cannot list %q; update the agent", req.Kind)
	}
	if err != nil {
		return &api.BrowseResult{Path: req.Path, Entries: []api.BrowseEntry{}, Error: err.Error()}
	}
	if res.Entries == nil {
		res.Entries = []api.BrowseEntry{}
	}
	return res
}

// Folders lists a folder. An empty path asks where to start: the drives
// on Windows, the root folder elsewhere.
func Folders(path string) (*api.BrowseResult, error) {
	if path == "" {
		if r := roots(); r != nil {
			return &api.BrowseResult{Entries: r}, nil
		}
		path = string(filepath.Separator)
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s is not a full path", path)
	}
	path = filepath.Clean(path)
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	res := &api.BrowseResult{Path: path}
	if up := filepath.Dir(path); up != path {
		res.Up = &up
	} else if roots() != nil {
		// Above a drive is the list of drives.
		top := ""
		res.Up = &top
	}
	for _, e := range entries {
		be := api.BrowseEntry{Name: e.Name(), Value: filepath.Join(path, e.Name()), Dir: e.IsDir()}
		if e.Type()&os.ModeSymlink != 0 {
			// A link to a folder can be opened like one.
			if st, err := os.Stat(be.Value); err == nil && st.IsDir() {
				be.Dir = true
			}
			be.Detail = "link"
		} else if !e.IsDir() && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				be.Size = info.Size()
			}
		}
		res.Entries = append(res.Entries, be)
	}
	sort.SliceStable(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	if len(res.Entries) > maxEntries {
		res.Entries, res.Truncated = res.Entries[:maxEntries], true
	}
	return res, nil
}

// machines names the virtual machines of the hypervisor a policy would
// use.
func machines(ctx context.Context, req *api.BrowseRequest) ([]api.BrowseEntry, error) {
	drv, err := hypervisor.For(req.Hypervisor)
	if err != nil {
		return nil, err
	}
	names, err := drv.List(ctx, req.Connection)
	if err != nil {
		return nil, err
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	out := make([]api.BrowseEntry, 0, len(names))
	for _, n := range names {
		out = append(out, api.BrowseEntry{Name: n, Value: n, Detail: drv.Label()})
	}
	return out, nil
}
