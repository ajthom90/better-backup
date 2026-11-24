package main

import "golang.org/x/sys/windows/registry"

// settings are the choices made in the Windows installer, which keeps
// them in the registry so that an upgrade finds them again.
type settings struct{ repo, port string }

func installed() (s settings) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\better-backup\Server`, registry.QUERY_VALUE)
	if err != nil {
		return s
	}
	defer k.Close()
	s.repo, _, _ = k.GetStringValue("Repo")
	s.port, _, _ = k.GetStringValue("Port")
	return s
}
