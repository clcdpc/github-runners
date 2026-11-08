//go:build windows

package secure

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type blob struct {
	Size uint32
	Data *byte
}

var crypt = syscall.NewLazyDLL("crypt32.dll").NewProc("CryptUnprotectData")
var localFree = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
var queryProcessImage = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")

// A PID alone can be reused. Status must match creation time and executable.
func ProcessRunning(pid int, started time.Time, executable string) bool {
	if pid < 1 || started.IsZero() {
		return false
	}
	h, err := syscall.OpenProcess(0x1000, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil || exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return false
	}
	creation := time.Unix(0, created.Nanoseconds())
	age := started.Sub(creation)
	if age < 0 || age > time.Minute {
		return false
	}
	var buffer [32768]uint16
	length := uint32(len(buffer))
	ok, _, _ := queryProcessImage.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&length)))
	return ok != 0 && strings.EqualFold(filepath.Clean(syscall.UTF16ToString(buffer[:length])), filepath.Clean(executable))
}

func ReadPAT(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("PAT file unavailable; run scripts/Set-RunnerToken.ps1 under the controller account")
	}
	if len(b) == 0 {
		return "", errors.New("empty PAT file")
	}
	in := blob{uint32(len(b)), &b[0]}
	var out blob
	ok, _, _ := crypt.Call(uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 1, uintptr(unsafe.Pointer(&out)))
	if ok == 0 {
		return "", errors.New("cannot decrypt PAT: use the same Windows account and machine that configured it")
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.Data)))
	data := unsafe.Slice(out.Data, out.Size)
	token := strings.TrimSpace(string(data))
	clear(data)
	clear(b)
	if !strings.HasPrefix(token, "github_pat_") {
		return "", errors.New("an organization-scoped fine-grained PAT (github_pat_) is required")
	}
	return token, nil
}

// Windows sharing mode zero gives a crash-released, cross-process lock, including
// separate logon sessions. A stale PID file never blocks recovery or kills a PID.
func Lock(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, errors.New("controller data directory is locked by another controller or management operation")
	}
	return func() { syscall.CloseHandle(h) }, nil
}
