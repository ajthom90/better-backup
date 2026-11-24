package svc

import "golang.org/x/sys/windows/registry"

// safeBoot lets the service start when Windows runs in safe mode with
// networking, which Directory Services Restore Mode is a form of. A
// domain controller's Active Directory can only be restored there, so
// the agent must be able to run there.
func safeBoot(name string, on bool) error {
	const path = `SYSTEM\CurrentControlSet\Control\SafeBoot\Network\`
	if !on {
		err := registry.DeleteKey(registry.LOCAL_MACHINE, path+name)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, path+name, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("", "Service")
}
