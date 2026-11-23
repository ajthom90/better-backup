package hyperv

import (
	"errors"
	"testing"
)

// TestScriptsParse checks every PowerShell script for syntax errors. It
// cannot run them: that takes a Hyper-V host.
func TestScriptsParse(t *testing.T) {
	const parse = `
$errs = $null
[void][System.Management.Automation.Language.Parser]::ParseInput($env:BB_SCRIPT, [ref]$null, [ref]$errs)
if ($errs.Count -gt 0) { throw (($errs | ForEach-Object { "line $($_.Extent.StartLineNumber): $($_.Message)" }) -join '; ') }
'ok'
`
	for name, script := range map[string]string{"list": listScript, "config": configScript, "checkpoint": checkpointScript, "release": releaseScript, "create": createScript} {
		out, err := run(parse, map[string]string{"BB_SCRIPT": script})
		if err != nil || string(out) != "ok" {
			t.Errorf("%s script: %q %v", name, out, err)
		}
	}
	if _, err := run(parse, map[string]string{"BB_SCRIPT": "if ($x { 'broken'"}); err == nil {
		t.Error("a broken script passed the syntax check")
	}
}

// TestListWithoutHyperV checks the error on a machine without Hyper-V.
// On a host with Hyper-V the list simply works.
func TestListWithoutHyperV(t *testing.T) {
	names, err := List()
	if err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("virtual machines: %v, error: %v", names, err)
}
