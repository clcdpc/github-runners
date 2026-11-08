//go:build windows

package hyperv

import (
	"fmt"
	"strings"
	"syscall"
)

func validateVHDXAttributes(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := syscall.GetFileAttributes(p)
	if err != nil {
		return err
	}
	forbidden := []string{}
	for _, flag := range []struct {
		mask uint32
		name string
	}{{0x200, "SparseFile"}, {0x800, "Compressed"}, {0x4000, "Encrypted"}} {
		if attributes&flag.mask != 0 {
			forbidden = append(forbidden, flag.name)
		}
	}
	if len(forbidden) != 0 {
		return fmt.Errorf("VHDX '%s' has forbidden NTFS attributes: %s; Hyper-V requires a non-sparse, uncompressed, unencrypted file. Use an uncompressed/unencrypted imageDirectory and dataDirectory, then run runners.ps1 PrepareImage -ForceRebuild; never modify an active base image", path, strings.Join(forbidden, ", "))
	}
	return nil
}
