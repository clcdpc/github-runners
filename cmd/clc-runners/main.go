package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	githubapi "github.com/clcdpc/github-runners/internal/github"
	"github.com/clcdpc/github-runners/internal/hyperv"
	"github.com/clcdpc/github-runners/internal/restart"
	"github.com/clcdpc/github-runners/internal/secure"
	gh "github.com/google/go-github/v79/github"
	"github.com/google/uuid"
)

var version = "dev"
var processStartedAt = time.Now()

type runtimeStatus struct {
	PID              int                  `json:"pid"`
	Nonce            string               `json:"nonce"`
	StartedAt        time.Time            `json:"startedAt"`
	UpdatedAt        time.Time            `json:"updatedAt"`
	Status           string               `json:"status"`
	RunnerCount      int                  `json:"runnerCount"`
	ProcessAlive     bool                 `json:"processAlive"`
	ConfigHash       string               `json:"configHash"`
	PATHash          string               `json:"patHash"`
	BinaryHash       string               `json:"binaryHash"`
	ImageID          string               `json:"imageId"`
	RuntimeHash      string               `json:"runtimeHash"`
	Resources        *config.Resources    `json:"resources,omitempty"`
	Capacity         *controller.Capacity `json:"capacity,omitempty"`
	RestartSupported bool                 `json:"restartSupported"`
	Restart          *restart.State       `json:"restart,omitempty"`
}

func main() {
	r := &secure.Redactor{}
	if err := execute(r); err != nil {
		fmt.Fprintln(os.Stderr, r.Text(err.Error()))
		os.Exit(1)
	}
}
func execute(redactor *secure.Redactor) error {
	if len(os.Args) < 2 {
		return errors.New("usage: clc-runners <validate|check-auth|setup|run|status|jobs|cleanup|delete-scale-set|version> --config local/controller.json")
	}
	command := os.Args[1]
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	path := flags.String("config", "local/controller.json", "dedicated JSON configuration file")
	confirm := flags.String("confirm-name", "", "scale set name, required only for explicit deletion")
	quiet := flags.Bool("quiet", false, "write operational logs only to the configured log directory")
	check := flags.Bool("check", false, "read-only infrastructure check (setup only)")
	force := flags.Bool("force", false, "rebuild the immutable Ubuntu image")
	asJSON := flags.Bool("json", false, "structured output (jobs only)")
	controllerExecutable := flags.String("controller-executable", "", "controller binary to verify when using a separate jobs reader")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected command arguments")
	}
	switch command {
	case "validate", "check-auth", "setup", "run", "status", "jobs", "cleanup", "delete-scale-set", "check-hyperv", "image-status", "prepare-image", "test-hyperv", "probe-hyperv", "prune-images":
	default:
		return errors.New("unknown command")
	}
	if *check && command != "setup" {
		return errors.New("--check is supported only with setup")
	}
	if (*asJSON || *controllerExecutable != "") && command != "jobs" {
		return errors.New("--json and --controller-executable are supported only with jobs")
	}
	var loadedConfigHash, loadedPATHash string
	if command == "run" {
		var err error
		loadedConfigHash, err = fileHash(*path)
		if err != nil {
			return err
		}
	}
	c, err := config.Load(*path)
	if err != nil && (command == "status" || command == "jobs") {
		c, err = config.Load(*path + ".effective.json")
	}
	if err != nil {
		return err
	}
	if command == "status" || command == "jobs" {
		if effective, e := config.Load(*path + ".effective.json"); e == nil && (controller.Scope(effective) != controller.Scope(c) || effective.Controller.DataDirectory != c.Controller.DataDirectory) {
			executable, _ := os.Executable()
			if *controllerExecutable != "" {
				executable = *controllerExecutable
			}
			if live, _ := readRuntime(effective, executable, time.Now()); live.ProcessAlive {
				c = effective
			}
		}
	}
	if command == "jobs" {
		if *controllerExecutable == "" {
			*controllerExecutable, _ = os.Executable()
		}
		return writeJobs(context.Background(), os.Stdout, c, *controllerExecutable, *asJSON)
	}
	if command == "validate" {
		fmt.Println("Configuration valid.")
		return nil
	}
	if command == "image-status" {
		image, err := hyperv.LoadImage(c.HyperV.ImageDirectory)
		if err != nil {
			if os.IsNotExist(errors.Unwrap(err)) || os.IsNotExist(err) {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"image": nil, "current": false, "reason": "no published base image exists"})
			}
			return err
		}
		status, err := hyperv.CheckImageInputs(image, c.HyperV.ImageRecipe, c.HyperV.DiskGiB)
		if err != nil {
			return err
		}
		if status.Current {
			if err = hyperv.ValidateImage(image); err != nil {
				return err
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"image": image, "current": status.Current, "reason": status.Reason, "changedInputs": status.ChangedInputs})
	}
	if command == "check-hyperv" {
		ws := &hyperv.Client{Config: c.HyperV, DataDir: c.Controller.DataDirectory, Redactor: redactor}
		return ws.Check(context.Background())
	}
	if command == "prepare-image" || command == "test-hyperv" || command == "probe-hyperv" || command == "prune-images" {
		if err = os.MkdirAll(c.Controller.DataDirectory, 0700); err != nil {
			return err
		}
		unlock, err := secure.Lock(filepath.Join(c.Controller.DataDirectory, "controller.lock"))
		if err != nil {
			return err
		}
		defer unlock()
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		scopePath := scaleLockPath(cache, c)
		if err = os.MkdirAll(filepath.Dir(scopePath), 0700); err != nil {
			return err
		}
		unlockScope, err := secure.Lock(scopePath)
		if err != nil {
			return errors.New("this scale set is in use by another controller/management operation; drain it before offline VM/image validation")
		}
		defer unlockScope()
		s, err := controller.LoadState(filepath.Join(c.Controller.DataDirectory, "state.json"), c)
		if err != nil {
			return err
		}
		ws := &hyperv.Client{Config: c.HyperV, Owner: s.Owner, Scale: c.GitHub.ScaleSet, DataDir: c.Controller.DataDirectory, LogDir: c.Controller.LogDirectory, Redactor: redactor}
		ctx := context.Background()
		switch command {
		case "prepare-image":
			ws.Progress, ws.ProgressOperation = os.Stderr, "PrepareImage"
			m, err := ws.PrepareImage(ctx, *force)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(m)
		case "test-hyperv":
			ws.Progress = os.Stderr
			ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
			defer cancel()
			return ws.Integration(ctx)
		case "probe-hyperv":
			ws.Progress = os.Stderr
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			return ws.Probe(ctx)
		case "prune-images":
			return ws.PruneImages(ctx)
		}
	}
	statePath := filepath.Join(c.Controller.DataDirectory, "state.json")
	if command == "status" {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			fmt.Println(`{"initialized":false}`)
			return nil
		}
		s, err := controller.InspectState(statePath, c)
		if err != nil {
			return err
		}
		var rt runtimeStatus
		if b, err := os.ReadFile(filepath.Join(c.Controller.DataDirectory, "status.json")); err == nil {
			json.Unmarshal(b, &rt)
		}
		restartState := restart.Read(filepath.Join(c.Controller.DataDirectory, "restart.json"))
		rt.Restart = &restartState
		executable, _ := os.Executable()
		rt.ProcessAlive = secure.ProcessRunning(rt.PID, rt.StartedAt, executable)
		if !rt.ProcessAlive && rt.Status != "stopped" {
			rt.Status = "not-running"
		}
		fresh := rt.ProcessAlive && time.Since(rt.UpdatedAt) < time.Duration(max(60, 3*c.Controller.ReconcileIntervalSeconds))*time.Second
		if rt.Capacity != nil && !fresh {
			rt.Capacity.Advertising = false
			rt.Capacity.ListenerConnected = false
		}
		currentHash, err := fileHash(*path)
		configurationError := ""
		if err != nil {
			configurationError = "Desired configuration is unavailable; running effective configuration retained"
		}
		ws := &hyperv.Client{Config: c.HyperV, Owner: s.Owner, Scale: c.GitHub.ScaleSet, DataDir: c.Controller.DataDirectory, LogDir: c.Controller.LogDirectory, Redactor: redactor}
		items, err := ws.List(context.Background())
		inventoryError := ""
		if err != nil {
			inventoryError = redactor.Text(err.Error())
		}
		inventoryAvailable := err == nil
		var admission *hyperv.Admission
		admissionError := ""
		if a, err := ws.Capacity(context.Background()); err == nil {
			admission = &a
		} else {
			admissionError = redactor.Text(err.Error())
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Runtime              runtimeStatus     `json:"runtime"`
			State                *controller.State `json:"state"`
			VMs                  []hyperv.VM       `json:"vms"`
			InventoryAvailable   bool              `json:"inventoryAvailable"`
			InventoryError       string            `json:"inventoryError,omitempty"`
			ConfiguredResources  config.Resources  `json:"configuredResources"`
			ConfigurationCurrent bool              `json:"configurationCurrent"`
			RuntimeFresh         bool              `json:"runtimeFresh"`
			Admission            *hyperv.Admission `json:"admission,omitempty"`
			AdmissionError       string            `json:"admissionError,omitempty"`
			ConfigurationError   string            `json:"configurationError,omitempty"`
		}{rt, s, items, inventoryAvailable, inventoryError, c.Resources(), strings.EqualFold(currentHash, rt.ConfigHash), fresh, admission, admissionError, configurationError})
	}
	if command == "run" {
		if err = restartStartupGate(c); err != nil {
			return err
		}
		loadedPATHash, err = fileHash(c.Controller.PATFile)
		if err != nil {
			return errors.New("PAT file unavailable; run runners.ps1 SetToken under the controller account")
		}
	}
	pat, err := secure.ReadPAT(c.Controller.PATFile)
	if err != nil {
		return err
	}
	redactor.Add(pat)
	if command == "check-auth" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := githubapi.CheckAuthentication(ctx, githubapi.REST(pat), c); err != nil {
			return err
		}
		fmt.Println("GitHub organization authentication and runner write permission valid.")
		return nil
	}
	if !*check {
		if err = os.MkdirAll(c.Controller.DataDirectory, 0700); err != nil {
			return err
		}
		unlock, err := secure.Lock(filepath.Join(c.Controller.DataDirectory, "controller.lock"))
		if err != nil {
			return err
		}
		defer unlock()
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		// The data lock protects the journal. This second crash-released lock
		// fences the same permanent scale set across different configurations
		// and checkouts under this Windows account, including scheduled tasks.
		scopePath := scaleLockPath(cache, c)
		if err := os.MkdirAll(filepath.Dir(scopePath), 0700); err != nil {
			return err
		}
		unlockScope, err := secure.Lock(scopePath)
		if err != nil {
			return errors.New("this GitHub scale set is locked by another controller or management operation; use its original configuration to stop/drain it")
		}
		defer unlockScope()
	}
	logs := &secure.DailyLog{Directory: c.Controller.LogDirectory, RetentionDays: c.Controller.LogRetentionDays}
	defer logs.Close()
	var level slog.Level
	if err = level.UnmarshalText([]byte(c.Controller.LogLevel)); err != nil {
		return err
	}
	var output io.Writer = logs
	if !*quiet && !*check {
		output = io.MultiWriter(os.Stdout, logs)
	}
	logger := slog.New(slog.NewJSONHandler(&secure.Writer{Out: output, Redactor: redactor}, &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.String(a.Key, a.Value.Time().Local().Format(time.RFC3339Nano))
		}
		return a
	}}))
	api, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: c.GitHub.OrganizationURL, PersonalAccessToken: pat, SystemInfo: scaleset.SystemInfo{System: "clc-hyperv-runners", Version: version, Subsystem: "controller"}},
		scaleset.WithLogger(logger.WithGroup("github")), scaleset.WithRetry(scaleset.RetryConfig{Max: 3, WaitMin: time.Second, WaitMax: 10 * time.Second, MaxElapsed: 3 * time.Minute}))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if *check {
		savedID := 0
		if _, err := os.Stat(statePath); err == nil {
			s, err := controller.InspectState(statePath, c)
			if err != nil {
				return err
			}
			savedID = s.ScaleSetID
		} else if !os.IsNotExist(err) {
			return err
		}
		current, err := githubapi.InfrastructureCurrent(ctx, githubapi.REST(pat), api, c, savedID)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Current bool `json:"current"`
		}{current})
	}
	s, err := controller.LoadState(statePath, c)
	if err != nil {
		return err
	}
	logs.RunnerPrefix = c.GitHub.ScaleSet + "-" + s.Owner[:8] + "-"
	ws := &hyperv.Client{Config: c.HyperV, Owner: s.Owner, Scale: c.GitHub.ScaleSet, DataDir: c.Controller.DataDirectory, LogDir: c.Controller.LogDirectory, Redactor: redactor}
	engine := &controller.Controller{Config: c, State: s, API: githubapi.RunnerAPI{Client: api}, HyperV: ws, Logger: logger, Redactor: redactor, Save: func(s *controller.State) error { return controller.SaveState(statePath, s) }, Now: time.Now}
	if command == "setup" {
		group, err := githubapi.SetupGroup(ctx, githubapi.REST(pat), c)
		if err != nil {
			return err
		}
		if s.ScaleSetID != 0 {
			existing, err := api.GetRunnerScaleSet(ctx, int(group.GetID()), c.GitHub.ScaleSet)
			if err != nil {
				return err
			}
			if existing == nil || existing.ID != s.ScaleSetID {
				return errors.New("saved scale-set ID differs from GitHub; investigate before adopting a replacement")
			}
		}
		ss, err := githubapi.EnsureScaleSet(ctx, api, int(group.GetID()), c.GitHub.ScaleSet)
		if err != nil {
			return err
		}
		if s.ScaleSetID != 0 && s.ScaleSetID != ss.ID {
			return errors.New("saved scale-set ID differs from GitHub; investigate before adopting a replacement")
		}
		s.ScaleSetID = ss.ID
		if err = controller.SaveState(statePath, s); err != nil {
			return err
		}
		logger.Info("GitHub setup complete", "runnerGroup", c.GitHub.RunnerGroup, "scaleSet", ss.Name, "scaleSetId", ss.ID)
		return nil
	}
	if command == "delete-scale-set" {
		if *confirm != c.GitHub.ScaleSet {
			return errors.New("explicit deletion requires --confirm-name equal to the configured scale set name")
		}
		if engine.Count() != 0 {
			return errors.New("scale-set deletion requires an empty runner journal; run cleanup first")
		}
		items, err := ws.List(ctx)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			return errors.New("owned VMs remain; run cleanup first")
		}
		group, err := githubapi.FindGroup(ctx, githubapi.REST(pat), c)
		if err != nil {
			return err
		}
		if group == nil {
			return nil
		}
		ss, err := api.GetRunnerScaleSet(ctx, int(group.GetID()), c.GitHub.ScaleSet)
		if err != nil {
			return err
		}
		if ss == nil {
			return nil
		}
		if s.ScaleSetID != ss.ID {
			return errors.New("scale-set ID does not match journal; refusing deletion")
		}
		if ss.Statistics == nil || ss.Statistics.TotalAssignedJobs != 0 || ss.Statistics.TotalRegisteredRunners != 0 {
			return errors.New("scale set has jobs/runners or unknown statistics; refusing deletion")
		}
		if err = api.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
			return err
		}
		s.ScaleSetID = 0
		return controller.SaveState(statePath, s)
	}
	if command == "cleanup" {
		if err = ws.RecoverAuxiliary(ctx); err != nil {
			return err
		}
		engine.Drain()
		if err = engine.Tick(ctx); err != nil {
			return err
		}
		logger.Info("cleanup complete", "remainingRunners", engine.Count())
		return nil
	}
	if err = ws.RecoverAuxiliary(ctx); err != nil {
		return err
	}
	if err = ws.Check(ctx); err != nil {
		return err
	}
	image, err := hyperv.LoadImage(c.HyperV.ImageDirectory)
	if err != nil {
		return fmt.Errorf("runner image unavailable; run runners.ps1 BuildImage: %w", err)
	}
	if err = hyperv.ValidateImage(image); err != nil {
		return err
	}
	imageStatus, err := hyperv.CheckImageInputs(image, c.HyperV.ImageRecipe, c.HyperV.DiskGiB)
	if err != nil {
		return err
	}
	if !imageStatus.Current {
		return fmt.Errorf("Ubuntu image inputs changed (%s); run runners.ps1 Activate to prepare a validated image", imageStatus.Reason)
	}
	engine.ImageID = image.ID
	inputs, err := runtimeInputs(*path, c, image.ID)
	if err != nil {
		return err
	}
	if inputs.ConfigHash != loadedConfigHash || inputs.PATHash != loadedPATHash {
		return errors.New("configuration or PAT changed during controller startup; rerun Activate to load consistent inputs")
	}
	// Running jobs keep a fixed bridge implementation when source files change.
	bridge, err := snapshotRuntime(c.HyperV.Script, c.Controller.DataDirectory, inputs.RuntimeHash)
	if err != nil {
		return err
	}
	ws.Config.Script = bridge
	if err = controller.WriteJSON(*path+".effective.json", c); err != nil {
		logger.Warn("cannot persist effective configuration for read-only diagnostics", "error", err)
	}
	absolutePath, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	return run(ctx, cancel, absolutePath, c, api, githubapi.REST(pat), engine, logger, inputs)
}

func snapshotRuntime(source, data, expectedHash string) (string, error) {
	b, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(b))
	if actual != expectedHash {
		return "", errors.New("Hyper-V script changed during startup; rerun Activate")
	}
	dir := filepath.Join(data, "runtime")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, actual+".ps1")
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(b) {
			return "", errors.New("pinned runtime script was modified; inspect data/runtime before restart")
		}
		return path, nil
	}
	return path, os.WriteFile(path, b, 0600)
}

func fileHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func scaleLockPath(cache string, c config.Config) string {
	scope := strings.ToLower(c.Organization() + "|" + c.GitHub.RunnerGroup + "|" + c.GitHub.ScaleSet)
	return filepath.Join(cache, "clc-runners", "locks", fmt.Sprintf("%x.lock", sha256.Sum256([]byte(scope))))
}

func runtimeInputs(path string, c config.Config, imageID string) (runtimeStatus, error) {
	resources := c.Resources()
	rt := runtimeStatus{ImageID: imageID, Resources: &resources}
	executable, err := os.Executable()
	if err != nil {
		return rt, err
	}
	for file, target := range map[string]*string{path: &rt.ConfigHash, c.Controller.PATFile: &rt.PATHash, executable: &rt.BinaryHash} {
		*target, err = fileHash(file)
		if err != nil {
			return rt, err
		}
	}
	if c.HyperV.Script != "" {
		rt.RuntimeHash, err = fileHash(c.HyperV.Script)
		if err != nil {
			return rt, err
		}
	}
	return rt, nil
}

func run(ctx context.Context, cancel context.CancelFunc, path string, c config.Config, api *scaleset.Client, rest *gh.Client, engine *controller.Controller, logger *slog.Logger, rt runtimeStatus) error {
	statusPath := filepath.Join(c.Controller.DataDirectory, "status.json")
	stopPath := filepath.Join(c.Controller.DataDirectory, "stop.json")
	rt.PID, rt.Nonce, rt.StartedAt, rt.Status = os.Getpid(), uuid.NewString(), processStartedAt, "starting"
	rt.RestartSupported = true
	coordinator := newRestartCoordinator(path, c, rt, logger)
	if coordinator.state.State == "Draining" {
		engine.BeginDrain()
	}
	var statusMu sync.Mutex
	setStatus := func(value string) {
		statusMu.Lock()
		defer statusMu.Unlock()
		if value != "" {
			rt.Status = value
		}
		rt.UpdatedAt = time.Now()
		rt.ProcessAlive = rt.Status != "stopped"
		restartState := restart.Read(coordinator.path)
		rt.Restart = &restartState
		capacity := engine.Capacity()
		rt.Capacity = &capacity
		rt.RunnerCount = capacity.CurrentRunners
		if engine.IsDraining() && rt.Status != "stopped" {
			rt.Status = "draining"
		} else if rt.Status == "draining" && !engine.IsDraining() {
			if capacity.ListenerConnected {
				rt.Status = "listening"
			} else {
				rt.Status = "retrying"
			}
		}
		if err := controller.WriteJSON(statusPath, rt); err != nil {
			logger.Error("write runtime status failed", "error", err)
		}
	}
	setStatus("starting")
	defer setStatus("stopped")
	// Signal/Stop fences acquisition first. Listening continues at zero capacity
	// until accepted jobs finish, including work that still needs a VM.
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	listenerCtx, stopListener := context.WithCancel(workCtx)
	defer func() { stopListener() }()
	done := make(chan error, 1)
	startListener := func() {
		go func() {
			for {
				err := listen(listenerCtx, c, api, rest, engine, logger, setStatus)
				engine.Session(nil)
				if listenerCtx.Err() != nil {
					done <- err
					return
				}
				if engine.IsDraining() {
					setStatus("draining")
				} else {
					setStatus("retrying")
				}
				logger.Error("listener failed; retrying", "error", err)
				select {
				case <-listenerCtx.Done():
					done <- nil
					return
				case <-time.After(time.Duration(c.Controller.RetrySeconds) * time.Second):
				}
			}
		}()
	}
	startListener()
	control := time.NewTicker(time.Second)
	defer control.Stop()
	tick := time.NewTicker(time.Duration(c.Controller.ReconcileIntervalSeconds) * time.Second)
	defer tick.Stop()
	signalDone := ctx.Done()
	stopping := false
	for {
		select {
		case <-signalDone:
			stopping = true
			coordinator.cancelRequest()
			signalDone = nil
			engine.BeginDrain()
			setStatus("draining")
		case <-tick.C:
			if engine.IsDraining() {
				if err := engine.RefreshDrainDemand(workCtx); err != nil {
					logger.Error("refresh drain ownership failed", "error", err)
				}
			}
			if err := engine.Tick(workCtx); err != nil {
				logger.Error("runner reconciliation failed", "error", err)
			}
			setStatus("")
		case now := <-control.C:
			if b, err := os.ReadFile(stopPath); err == nil {
				var request struct {
					Nonce string `json:"nonce"`
				}
				if json.Unmarshal(b, &request) == nil && request.Nonce == rt.Nonce && !stopping {
					stopping = true
					coordinator.cancelRequest()
					engine.BeginDrain()
					setStatus("draining")
					cancel()
				}
			}
			ready := false
			if stopping {
				checkCtx, stop := context.WithTimeout(workCtx, 30*time.Second)
				ready = engine.CheckQuiescence(checkCtx).Ready
				stop()
			} else {
				ready = coordinator.poll(workCtx, engine, now)
			}
			if engine.IsDraining() {
				setStatus("draining")
			}
			if !ready {
				continue
			}
			// Wait for the official listener's final callback, ACK and session
			// close. Count==0 alone cannot establish this barrier.
			stopListener()
			listenerErr := <-done
			checkCtx, stop := context.WithTimeout(workCtx, 30*time.Second)
			q := engine.CheckQuiescence(checkCtx)
			stop()
			if q.Ready && !errors.Is(listenerErr, errSessionClose) {
				if stopping {
					return nil
				}
				if err := coordinator.handoff(ctx); err == nil {
					return nil
				} else if errors.Is(err, errRestartCancelled) || errors.Is(err, context.Canceled) {
					stopping = true
					coordinator.cancelRequest()
				} else {
					coordinator.fail(err)
					engine.CancelDrain()
				}
			}
			if errors.Is(listenerErr, errSessionClose) {
				logger.Error("restart blocked by unclosed listener session", "error", listenerErr)
			}
			listenerCtx, stopListener = context.WithCancel(workCtx)
			startListener()
		}
	}
}

var errSessionClose = errors.New("listener session close failed")

func listen(ctx context.Context, c config.Config, api *scaleset.Client, rest *gh.Client, engine *controller.Controller, logger *slog.Logger, setStatus func(string)) (result error) {
	group, err := githubapi.FindGroup(ctx, rest, c)
	if err != nil {
		return err
	}
	if group == nil {
		return errors.New("runner group does not exist; run runners.ps1 Setup")
	}
	s := engine.Snapshot()
	if s.ScaleSetID != 0 {
		existing, err := api.GetRunnerScaleSet(ctx, int(group.GetID()), c.GitHub.ScaleSet)
		if err != nil {
			return err
		}
		if existing == nil || existing.ID != s.ScaleSetID {
			return errors.New("scale-set ID differs from journal; refusing replacement")
		}
	}
	ss, err := githubapi.EnsureScaleSet(ctx, api, int(group.GetID()), c.GitHub.ScaleSet)
	if err != nil {
		return err
	}
	if err := engine.SetScaleSetID(ss.ID); err != nil {
		return err
	}
	api.SetSystemInfo(scaleset.SystemInfo{System: "clc-hyperv-runners", Version: version, ScaleSetID: ss.ID, Subsystem: "listener"})
	session, err := api.MessageSessionClient(ctx, ss.ID, "clc-hyperv-"+s.Owner)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := session.Close(closeCtx); err != nil {
			logger.Warn("listener session close failed", "error", err)
			result = errors.Join(result, errSessionClose, err)
		}
	}()
	engine.Session(session)
	l, err := listener.New(engine.ObserveSession(session), listener.Config{ScaleSetID: ss.ID, MaxRunners: c.Runners.Maximum, Logger: logger.WithGroup("listener")})
	if err != nil {
		return err
	}
	if engine.IsDraining() {
		setStatus("draining")
	} else {
		setStatus("listening")
	}
	logger.Info("listener connected", "scaleSet", ss.Name, "scaleSetId", ss.ID, "maximum", c.Runners.Maximum)
	return l.Run(ctx, engine)
}
