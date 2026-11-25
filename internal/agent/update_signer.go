package agent

import (
	"errors"
	"strings"
)

// checkAuthenticode reads the two lines "Status|Subject" that PowerShell
// printed for this program and for the installer.
func checkAuthenticode(out string, err error) error {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\r", "")), "\n")
	if err != nil || len(lines) != 2 {
		// Without PowerShell there is no telling whether this program is
		// signed. Do not install blind if it might be.
		return errors.New("could not read the signatures of this agent and the installer, so the installer was not installed")
	}
	selfStatus, selfSigner, _ := strings.Cut(lines[0], "|")
	newStatus, newSigner, _ := strings.Cut(lines[1], "|")
	if selfStatus != "Valid" {
		return nil // this agent is not signed
	}
	if newStatus != "Valid" {
		return errors.New("the installer has no valid signature (" + newStatus + "), but this agent is signed by " + selfSigner + "; it was not installed")
	}
	if newSigner != selfSigner {
		return errors.New("the installer is signed by " + newSigner + ", not by the signer of this agent (" + selfSigner + "); it was not installed")
	}
	return nil
}
