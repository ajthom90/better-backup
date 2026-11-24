package agentctl

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"

	"golang.org/x/sys/windows"
)

// On Windows the control channel is a TCP port on the loopback address,
// chosen by the system, and a secret every request must carry. Both are
// in control.json in the agent's state directory, so whoever may read
// that directory (administrators, as installed) may manage the agent.
const defaultSocket = ""

// Unix sockets would be simpler, but Windows Server 2016 has none.

type channelFile struct {
	Addr   string `json:"addr"`
	Secret string `json:"secret"`
}

func listen(path string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	b := make([]byte, 32)
	rand.Read(b)
	secret := hex.EncodeToString(b)
	data, _ := json.Marshal(channelFile{ln.Addr().String(), secret})
	// Write beside, then rename: a reader never sees half a file.
	if err = os.WriteFile(path+".tmp", data, 0o600); err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, secret, nil
}

func cleanup(path string) { os.Remove(path) }

func endpoint(path string) (network, addr, secret string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	var f channelFile
	if json.Unmarshal(data, &f) != nil || f.Addr == "" || f.Secret == "" {
		return "", "", "", os.ErrNotExist // being written, or damaged: as good as absent
	}
	return "tcp", f.Addr, f.Secret, nil
}

func isRefused(err error) bool { return errors.Is(err, windows.WSAECONNREFUSED) }
