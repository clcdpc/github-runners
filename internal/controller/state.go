package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/google/uuid"
)

type Runner struct {
	Name        string    `json:"name"`
	VMID        string    `json:"vmId,omitempty"`
	RunnerID    int       `json:"runnerId,omitempty"`
	Phase       string    `json:"phase"`
	CreatedAt   time.Time `json:"createdAt"`
	IdleSince   time.Time `json:"idleSince,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
	RequestID   int64     `json:"requestId,omitempty"`
	// Optional observation fields extend v2 without changing existing properties.
	// Telemetry timestamps are UTC RFC3339 strings so a malformed optional value
	// cannot make the ownership/cleanup journal unreadable.
	PhaseSince          string         `json:"phaseSince,omitempty"`
	ExecutionObservedAt string         `json:"executionObservedAt,omitempty"`
	Job                 *JobAssignment `json:"job,omitempty"`
}
type State struct {
	Version               int                `json:"version"`
	Owner                 string             `json:"owner"`
	Scope                 string             `json:"scope"`
	ScaleSetID            int                `json:"scaleSetId"`
	Runners               map[string]*Runner `json:"runners"`
	MigrationRequired     bool               `json:"migrationRequired,omitempty"`
	LastProvisioningIssue *ProvisioningIssue `json:"lastProvisioningIssue,omitempty"`
	// Acquisition intents cover accepted work that has no runner yet, including
	// a lost AcquireJobs response. Runner lifecycle data stays in Runners.
	Acquisitions map[int64]bool `json:"acquisitions,omitempty"`
}

func Scope(c config.Config) string {
	return strings.TrimRight(c.GitHub.OrganizationURL, "/") + "|" + c.GitHub.RunnerGroup + "|" + c.GitHub.ScaleSet + "|hyperv|" + c.HyperV.SwitchName + "|" + c.HyperV.Subnet
}
func OwnedName(name, scale, owner string) bool {
	return len(owner) >= 8 && strings.HasPrefix(name, scale+"-"+owner[:8]+"-") && regexp.MustCompile(`^[A-Za-z0-9_.-]+-[a-f0-9]{12}$`).MatchString(name)
}
func LoadState(path string, c config.Config) (*State, error) {
	return readState(path, c, true)
}

// InspectState never writes a legacy journal during a concurrent controller
// startup. Mutating callers must hold the data lock and use LoadState instead.
func InspectState(path string, c config.Config) (*State, error) { return readState(path, c, false) }
func readState(path string, c config.Config, migrate bool) (*State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if !migrate {
			return nil, err
		}
		s := &State{Version: 2, Owner: uuid.NewString(), Scope: Scope(c), Runners: map[string]*Runner{}}
		if err := SaveState(path, s); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return nil, errors.New("state is corrupt; restore the original configuration/state")
	}
	migrated := false
	if s.Version == 1 && len(s.Runners) == 0 && strings.HasPrefix(s.Scope, strings.TrimRight(c.GitHub.OrganizationURL, "/")+"|"+c.GitHub.RunnerGroup+"|"+c.GitHub.ScaleSet+"|") && len(strings.Split(s.Scope, "|")) == 4 {
		s.Version = 2
		s.Scope = Scope(c)
		migrated = migrate
		s.MigrationRequired = !migrate
	}
	if s.Version != 2 || s.Scope != Scope(c) || s.Runners == nil || s.ScaleSetID < 0 {
		return nil, errors.New("state scope differs or legacy runner journal is nonempty; drain and clean application-owned WSLC runners using the previous controller before migration")
	}
	if _, err = uuid.Parse(s.Owner); err != nil {
		return nil, errors.New("invalid state ownership identity; refusing resource cleanup")
	}
	for id := range s.Acquisitions {
		if id <= 0 {
			return nil, errors.New("invalid acquisition ownership identity; refusing restart")
		}
	}
	for name, r := range s.Runners {
		if r == nil || r.Name != name || !OwnedName(name, c.GitHub.ScaleSet, s.Owner) || r.RunnerID < 0 || r.CreatedAt.IsZero() {
			return nil, errors.New("invalid runner state; refusing resource cleanup")
		}
		if migrate {
			// A persisted busy phase is not evidence of execution after restart.
			// A new listener event or successful guest probe must confirm it.
			r.ExecutionObservedAt = ""
			if r.Phase == "completed" || r.Phase == "cleaning" {
				r.Job, r.RequestID = nil, 0
			}
		}
	}
	if migrated {
		backup := path + ".wslc-v1.bak"
		f, e := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			_, e = f.Write(b)
			closeErr := f.Close()
			if e != nil {
				return nil, e
			}
			if closeErr != nil {
				return nil, closeErr
			}
		} else if !os.IsExist(e) {
			return nil, e
		}
		if err = SaveState(path, &s); err != nil {
			return nil, err
		}
	}
	return &s, nil
}
func SaveState(path string, s *State) error { return WriteJSON(path, s) }
func WriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temp, path); err != nil {
		return fmt.Errorf("atomically persist state: %w", err)
	}
	return nil
}
