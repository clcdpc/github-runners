//go:build windows

package restart

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/clcdpc/github-runners/internal/secure"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var isProcessInJob = kernel32.NewProc("IsProcessInJob")
var resumeThread = kernel32.NewProc("ResumeThread")

// StartDetached verifies complete Job Object escape while the helper is still
// suspended. Nested/restrictive jobs fail closed: no controller exit follows.
// Handles are not inherited, no console is attached, and the current token,
// environment and account are preserved (including S4U's logon token).
func startBreakaway(executable string, args []string, directory string) (int, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return 0, err
	}
	app, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	command := syscall.EscapeArg(path)
	for _, arg := range args {
		command += " " + syscall.EscapeArg(arg)
	}
	line, err := syscall.UTF16PtrFromString(command)
	if err != nil {
		return 0, err
	}
	dir, err := syscall.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	si := syscall.StartupInfo{Cb: uint32(unsafe.Sizeof(syscall.StartupInfo{})), Flags: 1, ShowWindow: 0}
	var pi syscall.ProcessInformation
	const flags = 0x01000000 | 0x08000000 | 0x00000004 | 0x00000200 // breakaway, no console, suspended, new process group
	if err = syscall.CreateProcess(app, line, nil, nil, false, flags, nil, dir, &si, &pi); err != nil {
		return 0, fmt.Errorf("independent helper creation failed (Job Object breakaway may be prohibited): %w", err)
	}
	defer syscall.CloseHandle(pi.Process)
	defer syscall.CloseHandle(pi.Thread)
	var inJob int32
	ok, _, checkErr := isProcessInJob.Call(uintptr(pi.Process), 0, uintptr(unsafe.Pointer(&inJob)))
	if ok == 0 || inJob != 0 {
		_ = syscall.TerminateProcess(pi.Process, 1) // suspended helper only; never a controller or runner
		if ok == 0 {
			return 0, fmt.Errorf("verify helper Job Object independence: %w", checkErr)
		}
		return 0, errors.New("helper remained in a Windows Job Object; restart blocked without stopping controller")
	}
	resumed, _, resumeErr := resumeThread.Call(uintptr(pi.Thread))
	if uint32(resumed) == 0xffffffff {
		_ = syscall.TerminateProcess(pi.Process, 1)
		return 0, resumeErr
	}
	return int(pi.ProcessId), nil
}

func CurrentAccountSID() (string, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}

// WMI's documented Win32_Process.Create broker does not inherit the caller's
// Job Object. It is the fallback for nested/restrictive jobs. Verify the actual
// owner SID, and let the helper validate DPAPI/Hyper-V/startup in its own token
// before it acknowledges the handoff. No new service or scheduled task is made.
func StartDetached(executable string, args []string, directory string) (int, error) {
	pid, directErr := startBreakaway(executable, args, directory)
	if directErr == nil {
		return pid, nil
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return 0, err
	}
	sid, err := CurrentAccountSID()
	if err != nil {
		return 0, err
	}
	line := syscall.EscapeArg(path)
	for _, arg := range args {
		line += " " + syscall.EscapeArg(arg)
	}
	const broker = `$ErrorActionPreference='Stop'
$inputData=[Console]::In.ReadToEnd() | ConvertFrom-Json
$startup=New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0;CreateFlags=[uint32]150994944}
$created=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$inputData.commandLine;CurrentDirectory=$inputData.directory;ProcessStartupInformation=$startup}
if ($created.ReturnValue -ne 0) { throw ('WMI process broker failed: '+$created.ReturnValue) }
$process=Get-CimInstance Win32_Process -Filter ('ProcessId='+[uint32]$created.ProcessId)
$owner=Invoke-CimMethod -InputObject $process -MethodName GetOwnerSid
if ($owner.ReturnValue -ne 0 -or $owner.Sid -ne $inputData.sid) { throw 'Restart broker account identity mismatch' }
@{pid=[int]$created.ProcessId} | ConvertTo-Json -Compress
`
	words := utf16.Encode([]rune(broker))
	encoded := make([]byte, 2*len(words))
	for i, v := range words {
		binary.LittleEndian.PutUint16(encoded[2*i:], v)
	}
	input, _ := json.Marshal(map[string]string{"commandLine": line, "directory": directory, "sid": sid})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	hide(cmd)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.Output()
	if err != nil {
		detail := ""
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			detail = (&secure.Redactor{}).Text(string(failure.Stderr))
		}
		return 0, fmt.Errorf("independent helper unavailable: native breakaway: %v; WMI broker: %w %s", directErr, err, detail)
	}
	var result struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(output, &result) != nil || result.PID < 1 {
		return 0, errors.New("independent process broker returned invalid identity")
	}
	return result.PID, nil
}

func hide(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
