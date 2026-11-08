//go:build !windows

package restart

import (
	"errors"
	"os/exec"
)

func StartDetached(string, []string, string) (int, error) {
	return 0, errors.New("independent controller restart requires Windows")
}
func hide(*exec.Cmd)                     {}
func CurrentAccountSID() (string, error) { return "", errors.New("Windows account required") }
