//go:build windows

package restart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var createJob = kernel32.NewProc("CreateJobObjectW")
var setJob = kernel32.NewProc("SetInformationJobObject")
var assignJob = kernel32.NewProc("AssignProcessToJobObject")

type jobBasic struct {
	ProcessTime, JobTime int64
	Flags                uint32
	Minimum, Maximum     uintptr
	Processes            uint32
	Affinity             uintptr
	Priority, Scheduling uint32
}
type ioCounters struct{ Read, Write, Other, ReadBytes, WriteBytes, OtherBytes uint64 }
type jobLimits struct {
	Basic                                          jobBasic
	IO                                             ioCounters
	ProcessMemory, JobMemory, PeakProcess, PeakJob uintptr
}

// This mode runs only in a disposable test subprocess. It exercises both a
// breakaway-enabled kill-on-close host and a host that forbids breakaway.
func TestDetachedParentFixture(t *testing.T) {
	root := os.Getenv("CLC_RESTART_PROCESS_FIXTURE")
	if root == "" {
		return
	}
	mode := os.Getenv("CLC_RESTART_PROCESS_MODE")
	if mode != "normal" {
		job, _, err := createJob.Call(0, 0)
		if job == 0 {
			t.Fatal(err)
		}
		limits := jobLimits{}
		limits.Basic.Flags = 0x2000 // KILL_ON_JOB_CLOSE
		if mode == "breakaway" {
			limits.Basic.Flags |= 0x800
		}
		ok, _, err := setJob.Call(job, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
		if ok == 0 {
			t.Fatal(err)
		}
		self, _ := syscall.GetCurrentProcess()
		ok, _, err = assignJob.Call(job, uintptr(self))
		if ok == 0 {
			t.Fatal(err)
		}
		// Deliberately retain the handle until process exit. Closing it terminates
		// the disposable parent and any children that failed to escape.
	}
	script := filepath.Join(root, "helper.ps1")
	_, err := StartDetached("pwsh.exe", []string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-File", script, "-Parent", fmt.Sprint(os.Getpid()), "-Result", filepath.Join(root, "survived.txt")}, root)
	if mode == "restricted" {
		// StartDetached must use the independent broker when native breakaway
		// is refused by a hosting job. Survival below proves that fallback.
	}
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestDetachedHelperSurvivesControllerExitAndJobClosure(t *testing.T) {
	for _, mode := range []string{"normal", "breakaway", "restricted"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			script := `param([int]$Parent,[string]$Result)
$ErrorActionPreference='Stop'
$deadline=[DateTime]::UtcNow.AddSeconds(15)
while ((Get-Process -Id $Parent -ErrorAction SilentlyContinue) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 100 }
if (Get-Process -Id $Parent -ErrorAction SilentlyContinue) { exit 2 }
[IO.File]::WriteAllText($Result,'independent helper survived original process exit')
`
			writeFile(t, filepath.Join(root, "helper.ps1"), script)
			cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedParentFixture$")
			cmd.Env = append(os.Environ(), "CLC_RESTART_PROCESS_FIXTURE="+root, "CLC_RESTART_PROCESS_MODE="+mode)
			hide(cmd)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fixture: %v %s", err, out)
			}
			result := "survived.txt"
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if b, e := os.ReadFile(filepath.Join(root, result)); e == nil && len(b) > 0 {
					t.Logf("%s: %s", mode, strings.TrimSpace(string(b)))
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatal("independent process handoff did not produce", result)
		})
	}
}
