package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// An enrollment code is everything a machine needs to enroll, in one
// string to copy and paste: the server address, a token and the server's
// fingerprint.
const enrollCodePrefix = "bb1."

type enrollCode struct {
	Server      string `json:"s"`
	Token       string `json:"t"`
	Fingerprint string `json:"f"`
}

// EncodeEnrollCode packs a server address, an enrollment token and the
// server's fingerprint into one code.
func EncodeEnrollCode(server, token, fingerprint string) string {
	b, _ := json.Marshal(enrollCode{strings.TrimRight(server, "/"), token, fingerprint})
	return enrollCodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// IsEnrollCode reports whether s looks like an enrollment code.
func IsEnrollCode(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), enrollCodePrefix)
}

// DecodeEnrollCode unpacks a code made by EncodeEnrollCode.
func DecodeEnrollCode(code string) (server, token, fingerprint string, err error) {
	// Codes are pasted from web pages and chat: drop any white space.
	code = strings.Join(strings.Fields(code), "")
	bad := errors.New("this is not an enrollment code; copy the whole code from the server's \"Add an agent\" dialog")
	rest, ok := strings.CutPrefix(code, enrollCodePrefix)
	if !ok {
		return "", "", "", bad
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(rest, "="))
	if err != nil {
		return "", "", "", bad
	}
	var c enrollCode
	if json.Unmarshal(b, &c) != nil || c.Server == "" || c.Token == "" || c.Fingerprint == "" {
		return "", "", "", bad
	}
	return c.Server, c.Token, c.Fingerprint, nil
}
