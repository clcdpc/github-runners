package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/restart"
	"github.com/google/uuid"
)

func coordinatorFixture(t *testing.T, busy bool) (*restartCoordinator, *controller.Controller, time.Time) {
	t.Helper()
	c, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c.Controller.DataDirectory = filepath.Join(dir, "data")
	c.Controller.PATFile = filepath.Join(dir, "credential.dpapi")
	c.Controller.LogDirectory = filepath.Join(dir, "logs")
	c.HyperV.Script = filepath.Join(dir, "HyperV.ps1")
	c.HyperV.ImageDirectory = filepath.Join(dir, "images")
	c.HyperV.ImageRecipe = filepath.Join(dir, "recipe.json")
	for _, path := range []string{c.Controller.PATFile, c.HyperV.Script, c.HyperV.ImageRecipe} {
		if err = os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "controller.json")
	if err = controller.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()
	r := newRestartCoordinator(path, c, runtimeStatus{PID: os.Getpid(), Nonce: uuid.NewString(), StartedAt: processStartedAt}, logger)
	target, _ := r.watcher.Capture(now)
	r.watcher.Applied = target
	engine := &controller.Controller{Config: c, State: &controller.State{Runners: map[string]*controller.Runner{}}, Now: func() time.Time { return now }}
	if busy {
		engine.State.Runners["owned-fixture"] = &controller.Runner{Name: "owned-fixture", Phase: "busy", CreatedAt: now}
	}
	return r, engine, now
}
func requestRestart(t *testing.T, r *restartCoordinator, engine *controller.Controller, nonce string) {
	t.Helper()
	if err := controller.WriteJSON(filepath.Join(engine.Config.Controller.DataDirectory, "restart-request.json"), map[string]string{"nonce": nonce, "requestId": uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
}

func TestManualRestartIsNonceProtectedPromptAndIdempotent(t *testing.T) {
	for _, busy := range []bool{false, true} {
		r, engine, now := coordinatorFixture(t, busy)
		requestRestart(t, r, engine, uuid.NewString())
		r.poll(context.Background(), engine, now)
		if r.state.Pending || engine.IsDraining() {
			t.Fatal("stale nonce requested restart")
		}
		requestRestart(t, r, engine, r.rt.Nonce)
		r.poll(context.Background(), engine, now.Add(time.Second))
		if !r.state.Pending || r.state.Automatic || r.state.State != "Draining" || !engine.IsDraining() || r.state.Blocker == "" {
			t.Fatal("manual restart did not fence assignments/report blocker", r.state)
		}
		if busy && r.state.ActiveAssignments != 1 {
			t.Fatal("busy manual restart lost assignment visibility")
		}
		requestID := r.state.RequestID
		requestRestart(t, r, engine, r.rt.Nonce)
		r.poll(context.Background(), engine, now.Add(2*time.Second))
		if r.state.RequestID != requestID {
			t.Fatal("duplicate request queued another helper")
		}
		r.fail(errors.New("fixture launch failure"))
		requestRestart(t, r, engine, r.rt.Nonce)
		r.poll(context.Background(), engine, now.Add(3*time.Second))
		if r.state.State == "Failed" || r.state.RequestID == requestID {
			t.Fatal("explicit retry did not recover failed request")
		}
	}
}

func TestIdleControllerDoesNotRequestRestartAndFailedIdentityDoesNotLoop(t *testing.T) {
	r, engine, now := coordinatorFixture(t, false)
	for i := 0; i < 5; i++ {
		r.poll(context.Background(), engine, now.Add(time.Duration(i)*10*time.Second))
	}
	if r.state.Pending || engine.IsDraining() {
		t.Fatal("idleness triggered restart")
	}
	target, _ := r.watcher.Capture(now)
	r.state = restart.State{Version: 1, State: "Failed", Pending: true, Target: target, RequestID: "failed", LastFailure: "fixture"}
	for i := 5; i < 10; i++ {
		r.poll(context.Background(), engine, now.Add(time.Duration(i)*10*time.Second))
	}
	if r.state.State != "Failed" || r.state.RequestID != "failed" {
		t.Fatal("failed target retried without a new request/input")
	}
}

func TestAutomaticChangeReversionClearsPendingDrain(t *testing.T) {
	r, engine, now := coordinatorFixture(t, false)
	original, _ := os.ReadFile(r.watcher.ConfigPath)
	changed := engine.Config
	changed.Controller.LogLevel = "debug"
	_ = controller.WriteJSON(r.watcher.ConfigPath, changed)
	r.poll(context.Background(), engine, now)
	r.poll(context.Background(), engine, now.Add(6*time.Second))
	if !r.state.Pending || !r.state.Automatic {
		t.Fatal("stable change not detected")
	}
	engine.BeginDrain()
	r.state.State = "Draining"
	_ = os.WriteFile(r.watcher.ConfigPath, original, 0600)
	r.poll(context.Background(), engine, now.Add(12*time.Second))
	if r.state.Pending || engine.IsDraining() {
		t.Fatal("reverted automatic update left controller unavailable")
	}
}

func TestCrashRecoveryRebindsPendingRequestToNewInstance(t *testing.T) {
	r, engine, _ := coordinatorFixture(t, true)
	s := restart.State{Version: 1, State: "Draining", Pending: true, ControllerNonce: uuid.NewString(), RequestID: uuid.NewString(), PID: 123, DrainingAt: time.Now().UTC()}
	if err := restart.Write(r.path, s); err != nil {
		t.Fatal(err)
	}
	recovered := newRestartCoordinator(r.watcher.ConfigPath, engine.Config, r.rt, r.logger)
	if recovered.state.ControllerNonce != r.rt.Nonce || recovered.state.PID != r.rt.PID || recovered.state.RequestID == s.RequestID || recovered.state.State != "Draining" {
		t.Fatal("crash left stale restart ownership", recovered.state)
	}
	s.State = "ReadyToRestart"
	_ = restart.Write(r.path, s)
	recovered = newRestartCoordinator(r.watcher.ConfigPath, engine.Config, r.rt, r.logger)
	if recovered.state.State != "Failed" || recovered.state.LastFailure == "" {
		t.Fatal("interrupted handoff was silently retried")
	}
}

func TestStartupGateChecksHelperProcessIdentityAndAllowsCrashRecovery(t *testing.T) {
	r, engine, _ := coordinatorFixture(t, false)
	exe, _ := os.Executable()
	engine.Config.HyperV.PowerShell = exe
	s := restart.State{Version: 1, State: "Restarting", RequestID: uuid.NewString()}
	_ = restart.Write(r.path, s)
	ack := map[string]any{"requestId": s.RequestID, "helperPid": os.Getpid(), "startedAt": processStartedAt}
	ackPath := filepath.Join(engine.Config.Controller.DataDirectory, "restart-ack.json")
	_ = controller.WriteJSON(ackPath, ack)
	if restartStartupGate(engine.Config) == nil {
		t.Fatal("startup raced active helper installation")
	}
	ack["startedAt"] = time.Now().Add(-time.Hour)
	_ = controller.WriteJSON(ackPath, ack)
	if err := restartStartupGate(engine.Config); err != nil {
		t.Fatal("stale/reused PID blocked normal startup recovery", err)
	}
	s.StartAllowed = true
	_ = restart.Write(r.path, s)
	if err := restartStartupGate(engine.Config); err != nil {
		t.Fatal(err)
	}
}
