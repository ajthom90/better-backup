//go:build !windows

package agent

import (
	"errors"
	"testing"
)

func TestCheckPkgSignature(t *testing.T) {
	signed := `Package "better-backup-agent_1.2.0_darwin_universal.pkg":
   Status: signed by a developer certificate issued by Apple for distribution
   Notarization: trusted by the Apple notary service
   Signed with a trusted timestamp on: 2026-11-25 16:00:00 +0000
   Certificate Chain:
    1. Developer ID Installer: Example Person (ABCDE12345)
`
	if err := checkPkgSignature("ABCDE12345", []byte(signed), nil); err != nil {
		t.Error(err)
	}
	if err := checkPkgSignature("ZZZZZ99999", []byte(signed), nil); err == nil {
		t.Error("a package signed by another team was accepted")
	}
	if err := checkPkgSignature("ABCDE12345", []byte("   Status: no signature\n"), errors.New("exit status 1")); err == nil {
		t.Error("an unsigned package was accepted by a signed agent")
	}
	if err := checkPkgSignature("ABCDE12345", []byte("   Status: signed by untrusted certificate\n    1. Fake (ABCDE12345)\n"), nil); err == nil {
		t.Error("a self-signed package was accepted")
	}
}
