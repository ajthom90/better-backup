// Package all links every hypervisor driver into the program.
package all

import (
	_ "github.com/ajthom90/better-backup/internal/hyperv"
	_ "github.com/ajthom90/better-backup/internal/libvirt"
	_ "github.com/ajthom90/better-backup/internal/proxmox"
)
