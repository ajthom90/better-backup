// Package keys holds the repository's cryptographic primitives: chunk
// identifiers, key derivation, authenticated encryption and passphrase
// wrapping of the master key.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"lukechampine.com/blake3"
)

// ID identifies a chunk: the keyed BLAKE3 hash of its plaintext.
type ID [32]byte

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Short returns an abbreviated form for logs.
func (id ID) Short() string { return hex.EncodeToString(id[:6]) }

func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *ID) UnmarshalText(b []byte) error {
	if hex.DecodedLen(len(b)) != len(id) {
		return fmt.Errorf("invalid chunk id length %d", len(b))
	}
	_, err := hex.Decode(id[:], b)
	return err
}

// ParseID parses a hex chunk id.
func ParseID(s string) (ID, error) {
	var id ID
	err := id.UnmarshalText([]byte(s))
	return id, err
}

// Keys are the working keys derived from the repository master key.
type Keys struct {
	Enc [32]byte // root of all encryption subkeys
	ID  [32]byte // keys the chunk-id hash
}

// Derive expands a master key into the working keys.
func Derive(master []byte) Keys {
	var k Keys
	expand(master, "better-backup/v1/enc", k.Enc[:])
	expand(master, "better-backup/v1/id", k.ID[:])
	return k
}

func expand(secret []byte, info string, out []byte) {
	r := hkdf.New(sha256.New, secret, nil, []byte(info))
	if _, err := io.ReadFull(r, out); err != nil {
		panic(err)
	}
}

// ChunkID computes the id of a plaintext chunk.
func ChunkID(idKey *[32]byte, data []byte) ID {
	h := blake3.New(32, idKey[:])
	h.Write(data)
	var id ID
	h.Sum(id[:0])
	return id
}

func aead(encKey *[32]byte, context []byte) cipher.AEAD {
	// One subkey per context (a chunk id, a pack trailer, a snapshot), so
	// random 96-bit nonces never repeat under one key in practice.
	m := hmac.New(sha256.New, encKey[:])
	m.Write(context)
	block, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
}

// Overhead is the number of bytes Seal adds to a plaintext.
const Overhead = 12 + 16

// Seal encrypts plaintext under a subkey bound to context. The context is
// also authenticated, so a blob cannot be replayed under another context.
func Seal(encKey *[32]byte, context, plaintext []byte) []byte {
	g := aead(encKey, context)
	out := make([]byte, 12, 12+len(plaintext)+16)
	if _, err := rand.Read(out); err != nil {
		panic(err)
	}
	return g.Seal(out, out[:12], plaintext, context)
}

// Open reverses Seal.
func Open(encKey *[32]byte, context, sealed []byte) ([]byte, error) {
	if len(sealed) < Overhead {
		return nil, errors.New("sealed data too short")
	}
	g := aead(encKey, context)
	pt, err := g.Open(nil, sealed[:12], sealed[12:], context)
	if err != nil {
		return nil, errors.New("decryption failed: data is corrupt or the key is wrong")
	}
	return pt, nil
}

// Wrapped is a master key encrypted under a passphrase.
type Wrapped struct {
	KDF     string `json:"kdf"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
	Salt    []byte `json:"salt"`
	Data    []byte `json:"data"`
}

func kek(passphrase []byte, w *Wrapped) *[32]byte {
	var k [32]byte
	copy(k[:], argon2.IDKey(passphrase, w.Salt, w.Time, w.Memory, w.Threads, 32))
	return &k
}

// Wrap encrypts master under passphrase using Argon2id.
func Wrap(passphrase, master []byte) *Wrapped {
	w := &Wrapped{KDF: "argon2id", Time: 3, Memory: 64 * 1024, Threads: 4, Salt: make([]byte, 16)}
	if _, err := rand.Read(w.Salt); err != nil {
		panic(err)
	}
	w.Data = Seal(kek(passphrase, w), []byte("master"), master)
	return w
}

// Unwrap recovers the master key.
func Unwrap(passphrase []byte, w *Wrapped) ([]byte, error) {
	if w.KDF != "argon2id" {
		return nil, fmt.Errorf("unsupported kdf %q", w.KDF)
	}
	m, err := Open(kek(passphrase, w), []byte("master"), w.Data)
	if err != nil {
		return nil, errors.New("wrong repository passphrase")
	}
	return m, nil
}

// Random returns n random bytes.
func Random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// RandomHex returns a random hex string of n bytes.
func RandomHex(n int) string { return hex.EncodeToString(Random(n)) }
