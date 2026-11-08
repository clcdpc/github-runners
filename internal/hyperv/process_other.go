//go:build !windows

package hyperv

import "os/exec"

func hideWindow(*exec.Cmd) {}
