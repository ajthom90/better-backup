//go:build windows

package vss

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSysFreeString = windows.NewLazySystemDLL("oleaut32.dll").NewProc("SysFreeString")

// Writers asks every VSS writer on this machine what it wants backed up.
// It takes no snapshot.
func Writers(timeout time.Duration) ([]Writer, error) {
	type result struct {
		w   []Writer
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) {
			ch <- result{nil, fmt.Errorf("initializing COM: %w", err)}
			return
		}
		defer windows.CoUninitialize()
		hr, _, _ := procCoInitSecu.Call(0, ^uintptr(0), 0, 0, 6, 2, 0, 0, 0)
		if int32(hr) < 0 && uint32(hr) != rpcTooLate {
			ch <- result{nil, check(hr, "security setup")}
			return
		}
		w, err := writers(time.Now().Add(timeout))
		ch <- result{w, err}
	}()
	r := <-ch
	return r.w, r.err
}

func writers(deadline time.Time) ([]Writer, error) {
	if err := procCreate.Find(); err != nil {
		return nil, fmt.Errorf("VSS is not available: %w", err)
	}
	var bc unsafe.Pointer
	hr, _, _ := procCreate.Call(uintptr(unsafe.Pointer(&bc)))
	if err := check(hr, "startup"); err != nil {
		return nil, err
	}
	defer call(bc, unkRelease)
	if err := check(call(bc, bcInitializeForBackup, 0), "initialize"); err != nil {
		return nil, err
	}
	var async unsafe.Pointer
	if err := check(call(bc, bcGatherWriterMetadata, uintptr(unsafe.Pointer(&async))), "gather writer metadata"); err != nil {
		return nil, err
	}
	if err := wait(async, "gather writer metadata", deadline); err != nil {
		return nil, err
	}
	defer call(bc, bcFreeWriterMetadata)
	var n uint32
	if err := check(call(bc, bcGetWriterMetadataCnt, uintptr(unsafe.Pointer(&n))), "count writers"); err != nil {
		return nil, err
	}
	var out []Writer
	for i := uint32(0); i < n; i++ {
		var instance windows.GUID
		var md unsafe.Pointer
		if err := check(call(bc, bcGetWriterMetadata, uintptr(i), uintptr(unsafe.Pointer(&instance)), uintptr(unsafe.Pointer(&md))), "read writer metadata"); err != nil {
			return nil, err
		}
		var bstr *uint16
		err := check(call(md, wmSaveAsXML, uintptr(unsafe.Pointer(&bstr))), "read writer metadata")
		call(md, unkRelease)
		if err != nil {
			return nil, err
		}
		doc := windows.UTF16PtrToString(bstr)
		procSysFreeString.Call(uintptr(unsafe.Pointer(bstr)))
		w, err := parseWriter(doc)
		if err != nil {
			return nil, fmt.Errorf("writer metadata: %w", err)
		}
		expand := func(fs []FileSpec) {
			for i := range fs {
				fs[i].Path = expandEnv(fs[i].Path)
			}
		}
		for c := range w.Components {
			expand(w.Components[c].Files)
		}
		expand(w.Excludes)
		out = append(out, w)
	}
	return out, nil
}

// expandEnv replaces %NAME% references, which writers use for folders
// such as %SystemRoot%.
func expandEnv(s string) string {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return s
	}
	buf := make([]uint16, 1024)
	for {
		n, err := windows.ExpandEnvironmentStrings(p, &buf[0], uint32(len(buf)))
		if err != nil || n == 0 {
			return s
		}
		if int(n) <= len(buf) {
			return windows.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n)
	}
}
