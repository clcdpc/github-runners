package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/restart"
	"github.com/clcdpc/github-runners/internal/secure"
	"github.com/google/uuid"
)

type validationResult struct {
	target restart.Target
	err    error
}

var errRestartCancelled = errors.New("Restart cancelled by Stop")

func (r *restartCoordinator) cancelRequest() {
	if r.state.Pending {
		r.state.State = "None"
		r.state.Pending = false
		r.state.Reason = errRestartCancelled.Error()
		r.state.Blocker = ""
		r.save()
	}
}

type restartCoordinator struct {
	path       string
	watcher    *restart.Watcher
	state      restart.State
	rt         runtimeStatus
	logger     *slog.Logger
	validation chan validationResult
	validating bool
	validated  string
	manual     string
	nextPoll   time.Time
}

func newRestartCoordinator(path string, c config.Config, rt runtimeStatus, logger *slog.Logger) *restartCoordinator {
	executable, _ := os.Executable()
	root := filepath.Dir(filepath.Dir(executable))
	w := &restart.Watcher{ConfigPath: path, ManifestPath: filepath.Join(root, "bin", "restart-runtime.json"), Executable: executable, Running: c}
	applied, blocker := w.Capture(time.Now())
	if !strings.EqualFold(applied.ConfigHash, rt.ConfigHash) || !strings.EqualFold(applied.BinaryHash, rt.BinaryHash) || !strings.EqualFold(applied.PATHash, rt.PATHash) || !strings.EqualFold(applied.RuntimeHash, rt.RuntimeHash) || applied.ImageID != rt.ImageID || blocker != "" {
		applied.Identity = "running:" + rt.BinaryHash + rt.ConfigHash + rt.RuntimeHash + rt.ImageID
	}
	w.Applied = applied
	statePath := filepath.Join(c.Controller.DataDirectory, "restart.json")
	s := restart.Read(statePath)
	if s.Pending && (s.State == "Pending" || s.State == "Draining") && s.ControllerNonce != rt.Nonce {
		s.ControllerNonce, s.PID, s.StartedAt, s.RequestID = rt.Nonce, rt.PID, rt.StartedAt, uuid.NewString()
		s.AccountSID, _ = restart.CurrentAccountSID()
		s.Blocker = "Recovering graceful restart after controller interruption"
		_ = restart.Write(statePath, s)
	}
	if s.State == "ReadyToRestart" || (s.State == "Restarting" && !s.StartAllowed) {
		s.State, s.LastFailure, s.Blocker = "Failed", "Previous handoff was interrupted; inspect diagnostics and request RestartWhenIdle to retry", "Previous handoff was interrupted; inspect diagnostics and request RestartWhenIdle to retry"
		_ = restart.Write(statePath, s)
	}
	return &restartCoordinator{path: statePath, watcher: w, state: s, rt: rt, logger: logger, validation: make(chan validationResult, 1)}
}

func (r *restartCoordinator) save() {
	if err := restart.Write(r.path, r.state); err != nil {
		r.logger.Error("persist restart state failed", "error", err)
	}
}
func (r *restartCoordinator) fail(err error) {
	r.state.State = "Failed"
	r.state.Pending = true
	r.state.Blocker = err.Error()
	r.state.LastFailure = err.Error()
	r.save()
	r.logger.Error("controller restart failed", "error", err)
}

func (r *restartCoordinator) poll(ctx context.Context, engine *controller.Controller, now time.Time) bool {
	// The replacement leaves result ownership with its helper until readiness is
	// confirmed. A failed verification remains visible and is never auto-retried.
	if r.state.State == "Restarting" && r.state.StartAllowed {
		r.state = restart.Read(r.path)
		if r.state.State == "Restarting" {
			if now.Sub(r.state.LastAttempt) > 15*time.Minute {
				r.fail(errors.New("Restart helper did not confirm readiness; inspect helper diagnostics before retrying"))
			}
			return false
		}
	}
	var manual struct {
		Nonce     string `json:"nonce"`
		RequestID string `json:"requestId"`
	}
	b, _ := os.ReadFile(filepath.Join(engine.Config.Controller.DataDirectory, "restart-request.json"))
	_ = json.Unmarshal(b, &manual)
	requested := manual.Nonce == r.rt.Nonce && manual.RequestID != "" && manual.RequestID != r.manual
	if !requested && now.Before(r.nextPoll) {
		return false
	}
	r.nextPoll = now.Add(5 * time.Second)
	target, blocker, changed := r.watcher.Poll(now)
	if requested {
		r.manual = manual.RequestID
		if r.state.Pending && r.state.State != "Failed" {
			r.state.Automatic = false
			r.state.Reason = "RestartWhenIdle requested (update also pending)"
			if strings.HasPrefix(r.validated, "blocked:") {
				r.validated = ""
			}
			r.save()
		}
	}
	active := r.state.Pending && r.state.State != "Failed"
	reverted := active && r.state.Target.Identity != target.Identity && target.Identity == r.watcher.Applied.Identity
	if reverted && r.state.Automatic {
		r.state.State = "None"
		r.state.Pending = false
		r.state.Blocker = ""
		r.state.Reason = "Desired inputs reverted to the running identity"
		r.validated = ""
		if engine.IsDraining() {
			engine.CancelDrain()
		}
		r.save()
		return false
	}
	if (changed && (r.state.Target.Identity != target.Identity || !r.state.Pending)) || (requested && !active) || reverted {
		// Completed requests for the running identity never create an idle restart.
		if changed || requested || reverted {
			previous := r.state
			r.state = restart.State{
				Version: 1, State: "Pending", Pending: true, Automatic: !requested,
				Reason: "Configuration/runtime update detected", RequestID: uuid.NewString(),
				ControllerNonce: r.rt.Nonce, PID: r.rt.PID, StartedAt: r.rt.StartedAt,
				Config: r.watcher.ConfigPath, Root: filepath.Dir(filepath.Dir(r.watcher.Executable)), Executable: r.watcher.Executable,
				Target: target, DetectedAt: now, LastSuccess: previous.LastSuccess, LastAttempt: previous.LastAttempt,
				LastFailure: previous.LastFailure, DrainingAt: previous.DrainingAt,
			}
			if requested {
				r.state.Reason = "RestartWhenIdle requested"
			} else if active && !previous.Automatic {
				r.state.Automatic = false
				r.state.Reason = "RestartWhenIdle requested (desired inputs changed)"
			}
			if !active {
				r.state.DrainingAt = time.Time{}
			}
			if engine.IsDraining() {
				r.state.State = "Draining"
			}
			r.validated = ""
			r.state.AccountSID, _ = restart.CurrentAccountSID()
			r.save()
		}
	}
	if !r.state.Pending || r.state.State == "Failed" {
		return false
	}
	if target.Identity == r.state.Target.Identity && target.ManifestHash != r.state.Target.ManifestHash {
		r.state.Target = target
		r.validated = ""
		r.save()
	}
	if requested && !engine.IsDraining() {
		engine.BeginDrain()
		r.state.State = "Draining"
		r.state.DrainingAt = now
		r.save()
	}
	capacity := engine.Capacity()
	snapshot := engine.Snapshot()
	assignments := max(capacity.AssignedJobs, capacity.BusyRunners, len(snapshot.Acquisitions))
	cleanup := 0
	for _, runner := range snapshot.Runners {
		if runner.Phase == "completed" || runner.Phase == "cleaning" || runner.Phase == "failed" {
			cleanup++
		}
	}
	if r.state.ActiveAssignments != assignments || r.state.CleanupPending != cleanup {
		r.state.ActiveAssignments, r.state.CleanupPending = assignments, cleanup
		r.save()
	}
	if r.state.Target.Identity != target.Identity || (!changed && target.Identity != r.watcher.Applied.Identity) {
		return false
	} // debounce additional edits
	select {
	case result := <-r.validation:
		r.validating = false
		if result.target.Identity == r.state.Target.Identity && result.target.ManifestHash == r.state.Target.ManifestHash {
			if result.err != nil {
				r.state.Blocker = result.err.Error()
				r.save()
				r.validated = "blocked:" + result.target.Identity
			} else {
				r.validated = result.target.Identity
				r.state.Blocker = ""
			}
		}
	default:
	}
	if blocker != "" {
		if r.state.Blocker != blocker {
			r.state.Blocker = blocker
			r.save()
		}
		return false
	}
	if r.validated != target.Identity {
		if !r.validating && r.validated != "blocked:"+target.Identity {
			manifest, err := restart.ValidateManifest(target.ManifestPath, target.ManifestHash)
			if err != nil {
				r.state.Blocker = err.Error()
				r.validated = "blocked:" + target.Identity
				r.save()
				return false
			}
			r.validating = true
			r.state.Blocker = "Validating desired installation and unattended startup"
			r.save()
			requestPath := r.path + ".check.json"
			if err = restart.Write(requestPath, r.state); err != nil {
				r.validating = false
				r.fail(err)
				return false
			}
			// Test-RunnerRestartPlan verifies the canonical record path, not this
			// immutable preflight copy; the script supplies it explicitly.
			go func() {
				checkCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
				err := restart.Preflight(checkCtx, engine.Config.HyperV.PowerShell, manifest, requestPath)
				r.validation <- validationResult{target, err}
			}()
		}
		return false
	}
	if !engine.IsDraining() {
		engine.BeginDrain()
		r.state.State = "Draining"
		r.state.DrainingAt = now
		r.logger.Info("controller restart draining", "reason", r.state.Reason)
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q := engine.CheckQuiescence(qctx)
	r.state.ActiveAssignments, r.state.CleanupPending, r.state.Blocker = q.ActiveAssignments, q.CleanupPending, q.Blocker
	r.save()
	return q.Ready
}

func (r *restartCoordinator) handoff(ctx context.Context) error {
	target, blocker := r.watcher.Capture(time.Now())
	if blocker != "" || target.Identity != r.state.Target.Identity {
		return errors.New("Desired update changed at handoff; wait for validation of the latest inputs")
	}
	manifest, err := restart.ValidateManifest(target.ManifestPath, target.ManifestHash)
	if err != nil {
		return err
	}
	r.state.State = "ReadyToRestart"
	r.state.Target = target
	r.state.LastAttempt = time.Now()
	r.state.Blocker = "Waiting for independent helper acknowledgment"
	if err = restart.Write(r.path, r.state); err != nil {
		return err
	}
	helper, err := restart.StartDetached(r.watcher.Running.HyperV.PowerShell, restart.HelperArguments(manifest, r.path, false), r.state.Root)
	if err != nil {
		if failed := restart.Read(r.path); failed.RequestID == r.state.RequestID && failed.State == "Failed" && failed.LastFailure != "" {
			return errors.New(failed.LastFailure)
		}
		return err
	}
	r.state.HelperPID = helper
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Independent restart helper did not acknowledge; controller preserved")
		case <-poll.C:
			var stop struct {
				Nonce string `json:"nonce"`
			}
			stopBytes, _ := os.ReadFile(filepath.Join(r.watcher.Running.Controller.DataDirectory, "stop.json"))
			if json.Unmarshal(stopBytes, &stop) == nil && stop.Nonce == r.rt.Nonce {
				return errRestartCancelled
			}
			if state := restart.Read(r.path); state.State == "Failed" && state.RequestID == r.state.RequestID {
				return errors.New(state.LastFailure)
			}
			var ack struct {
				RequestID string    `json:"requestId"`
				Nonce     string    `json:"nonce"`
				HelperPID int       `json:"helperPid"`
				StartedAt time.Time `json:"startedAt"`
			}
			b, _ := os.ReadFile(filepath.Join(r.watcher.Running.Controller.DataDirectory, "restart-ack.json"))
			if json.Unmarshal(b, &ack) == nil && ack.RequestID == r.state.RequestID && ack.Nonce == r.rt.Nonce && ack.HelperPID == helper && secure.ProcessRunning(helper, ack.StartedAt, resolveExecutable(r.watcher.Running.HyperV.PowerShell)) {
				latest, blocker := r.watcher.Capture(time.Now())
				if blocker != "" || latest.Identity != r.state.Target.Identity {
					return errors.New("Desired update changed during helper validation; original controller preserved")
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.state.State = "Restarting"
				r.state.Blocker = "Independent helper applying validated update after controller exit"
				return restart.Write(r.path, r.state)
			}
		}
	}
}

func resolveExecutable(path string) string {
	resolved, err := exec.LookPath(path)
	if err == nil {
		return resolved
	}
	return path
}

func restartStartupGate(c config.Config) error {
	s := restart.Read(filepath.Join(c.Controller.DataDirectory, "restart.json"))
	if s.State != "Restarting" && s.State != "ReadyToRestart" {
		return nil
	}
	if s.StartAllowed {
		return nil
	}
	ackPath := filepath.Join(c.Controller.DataDirectory, "restart-ack.json")
	var ack struct {
		RequestID string    `json:"requestId"`
		HelperPID int       `json:"helperPid"`
		StartedAt time.Time `json:"startedAt"`
	}
	b, _ := os.ReadFile(ackPath)
	if json.Unmarshal(b, &ack) == nil && ack.RequestID == s.RequestID && secure.ProcessRunning(ack.HelperPID, ack.StartedAt, resolveExecutable(c.HyperV.PowerShell)) {
		return fmt.Errorf("restart helper owns the handoff; wait for readiness or inspect restart.json")
	}
	return nil // crashed helper: normal startup recovery remains available
}
