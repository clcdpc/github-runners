package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/hyperv"
	"github.com/clcdpc/github-runners/internal/secure"
	"github.com/google/uuid"
)

type API interface {
	JIT(context.Context, string, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	Find(context.Context, string) (*scaleset.RunnerReference, error)
	Remove(context.Context, int) error
	Statistics(context.Context, int) (*scaleset.RunnerScaleSetStatistic, error)
}
type Provisioner interface {
	Capacity(context.Context) (hyperv.Admission, error)
	List(context.Context) ([]hyperv.VM, error)
	Create(context.Context, string, string, int) (string, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Deliver(context.Context, string, string) error
	Lifecycle(context.Context, string) (hyperv.Lifecycle, error)
	Archive(context.Context, string, bool) error
	Remove(context.Context, string) error
	ForgetDiagnostics(string) error
}
type Acquirer interface {
	AcquireJobs(context.Context, []int64) ([]int64, error)
}

type Controller struct {
	mu                   sync.Mutex
	Config               config.Config
	State                *State
	API                  API
	HyperV               Provisioner
	ImageID              string
	Logger               *slog.Logger
	Redactor             *secure.Redactor
	Save                 func(*State) error
	Now                  func() time.Time
	session              Acquirer
	statistics           *scaleset.RunnerScaleSetStatistic
	statisticsAt         time.Time
	draining             bool
	gracefulDrain        bool
	drainZeroSince       time.Time
	needStatistics       bool
	pendingJIT           map[string]string
	lastAdmission        *hyperv.Admission
	retryAt              time.Time
	provisioningFailures int
	advertisedMaximum    int
	advertisedAt         time.Time
}

func Desired(minimum, maximum int, stats *scaleset.RunnerScaleSetStatistic) int {
	if stats == nil {
		return minimum
	}
	// Saturate without overflowing on malformed remote statistics.
	return min(maximum, minimum+min(maximum, max(0, stats.TotalAssignedJobs)))
}
func (c *Controller) Session(session Acquirer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = session
	c.statistics = nil
	c.drainZeroSince = time.Time{}
	c.advertisedMaximum = 0
	c.advertisedAt = time.Time{}
}
func (c *Controller) Drain()     { c.mu.Lock(); defer c.mu.Unlock(); c.draining = true; c.session = nil }
func (c *Controller) Count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.State.Runners) }
func (c *Controller) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := *c.State
	s.Acquisitions = map[int64]bool{}
	for id, accepted := range c.State.Acquisitions {
		s.Acquisitions[id] = accepted
	}
	if c.State.LastProvisioningIssue != nil {
		issue := *c.State.LastProvisioningIssue
		s.LastProvisioningIssue = &issue
	}
	s.Runners = map[string]*Runner{}
	for name, r := range c.State.Runners {
		copy := *r
		if r.Job != nil {
			job := *r.Job
			copy.Job = &job
		}
		s.Runners[name] = &copy
	}
	return s
}
func (c *Controller) SetScaleSetID(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.State.ScaleSetID = id
	return c.Save(c.State)
}
func (c *Controller) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg != nil {
		if len(msg.JobAvailableMessages) > 0 && !c.draining {
			if c.session == nil {
				return errors.New("no active listener session")
			}
			ids := []int64{}
			seen := map[int64]bool{}
			for _, job := range msg.JobAvailableMessages {
				if job != nil && job.RunnerRequestID > 0 && !seen[job.RunnerRequestID] {
					ids = append(ids, job.RunnerRequestID)
					seen[job.RunnerRequestID] = true
				}
			}
			if c.State.Acquisitions == nil {
				c.State.Acquisitions = map[int64]bool{}
			}
			c.drainZeroSince = time.Time{}
			for _, id := range ids {
				if _, exists := c.State.Acquisitions[id]; !exists {
					c.State.Acquisitions[id] = false
				}
			}
			if err := c.Save(c.State); err != nil {
				return err
			}
			acquired, err := c.session.AcquireJobs(ctx, ids)
			if err != nil {
				return fmt.Errorf("acquire jobs: %w", err)
			}
			accepted := map[int64]bool{}
			for _, id := range acquired {
				accepted[id] = true
				c.State.Acquisitions[id] = true
			}
			for _, id := range ids {
				if !accepted[id] && !c.State.Acquisitions[id] {
					delete(c.State.Acquisitions, id)
				}
			}
		}
		for _, job := range msg.JobStartedMessages {
			if job == nil {
				continue
			}
			c.jobStarted(job)
			c.Logger.Info("job started", "runner", job.RunnerName, "requestId", job.RunnerRequestID)
		}
		for _, job := range msg.JobCompletedMessages {
			if job == nil {
				continue
			}
			c.jobCompleted(job)
			delete(c.State.Acquisitions, job.RunnerRequestID)
			c.Logger.Info("job completed", "runner", job.RunnerName, "requestId", job.RunnerRequestID, "result", job.Result)
		}
		if msg.Statistics != nil {
			copy := *msg.Statistics
			c.statistics = &copy
			if copy.TotalAssignedJobs > 0 || copy.TotalAcquiredJobs > 0 || copy.TotalRunningJobs > 0 || copy.TotalBusyRunners > 0 {
				c.drainZeroSince = time.Time{}
			}
			c.statisticsAt = c.Now()
			c.needStatistics = false
		}
		if err := c.Save(c.State); err != nil {
			return err
		}
	}
	return c.tick(ctx)
}
func (c *Controller) Tick(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick(ctx)
}

func (c *Controller) tick(ctx context.Context) error {
	VMs, err := c.HyperV.List(ctx)
	if err != nil {
		for _, r := range c.State.Runners {
			r.ExecutionObservedAt = ""
		}
		return errors.Join(err, c.Save(c.State))
	}
	present := map[string]hyperv.VM{}
	for _, v := range VMs {
		if !OwnedName(v.Name, c.Config.GitHub.ScaleSet, c.State.Owner) {
			return errors.New("owned VM has an invalid runner name")
		}
		present[v.Name] = v
		if c.State.Runners[v.Name] == nil {
			id := v.RunnerID
			if id < 1 {
				return errors.New("owned VM is missing its runner registration ID")
			}
			c.State.Runners[v.Name] = &Runner{Name: v.Name, VMID: v.ID, RunnerID: id, Phase: "recovering", CreatedAt: v.Created}
			c.Logger.Info("recovered owned VM", "runner", v.Name)
		}
	}
	if err = c.Save(c.State); err != nil {
		return err
	}
	names := make([]string, 0, len(c.State.Runners))
	for name := range c.State.Runners {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		r := c.State.Runners[name]
		v, exists := present[name]
		if !exists || !v.Running {
			// A stopped/missing VM cannot still execute its old assignment, even
			// when GitHub or diagnostic cleanup is temporarily unavailable.
			r.Job, r.RequestID, r.ExecutionObservedAt = nil, 0, ""
			if err = c.Save(c.State); err != nil {
				return err
			}
			if exists {
				c.Logger.Info("VM terminated", "runner", name, "status", v.Status)
			}
			if err = c.cleanup(ctx, r, exists, false); err != nil {
				return err
			}
			continue
		}
		r.VMID = v.ID
		life, err := c.HyperV.Lifecycle(ctx, name)
		if err != nil {
			r.ExecutionObservedAt = ""
			if saveErr := c.Save(c.State); saveErr != nil {
				return saveErr
			}
			expired := c.Now().Sub(r.CreatedAt) >= time.Duration(c.Config.Runners.StartupTimeoutSeconds)*time.Second ||
				(r.Phase == "completed" && !r.CompletedAt.IsZero() && c.Now().Sub(r.CompletedAt) >= time.Duration(c.Config.Runners.CompletionGraceSeconds)*time.Second) ||
				(c.draining && !c.gracefulDrain && c.Now().Sub(r.CreatedAt) >= time.Duration(c.Config.Runners.IdleGraceSeconds)*time.Second)
			if expired {
				c.Logger.Warn("guest unreachable; attempting recovery through GitHub busy-runner fence", "runner", name, "error", err)
				life.Failed = true // Cleanup still has to pass GitHub's busy-runner fence.
			} else {
				return err
			}
		} // A failed probe is never evidence that a runner is idle.
		if life.Configured {
			delete(c.pendingJIT, name)
		}
		if !life.Started || life.Completed || life.Failed || life.Exited {
			r.ExecutionObservedAt = ""
		}
		if life.Completed && r.Phase != "cleaning" {
			c.complete(r)
		} else if life.Started && !life.Exited && !life.Failed && r.Phase != "completed" && r.Phase != "cleaning" {
			c.phase(r, "busy")
			r.ExecutionObservedAt = stamp(c.Now())
			if r.Job == nil {
				r.Job = &JobAssignment{}
			}
			if !life.StartedAt.IsZero() {
				r.Job.StartedAt, r.Job.StartSource = stamp(life.StartedAt), "runner-hook"
			}
		} else if (r.Phase == "recovering" || r.Phase == "starting" || r.Phase == "provisioning") && life.Configured && life.Listening {
			c.phase(r, "online")
			delete(c.pendingJIT, name)
		} else if (r.Phase == "provisioning" || r.Phase == "starting" || r.Phase == "recovering") && life.Ready && !life.Failed && !life.Exited && c.pendingJIT[name] != "" {
			if err = c.HyperV.Deliver(ctx, name, c.pendingJIT[name]); err != nil {
				return fmt.Errorf("deliver runner JIT over SSH stdin: %w", err)
			}
		}
		if life.Exited || life.Failed {
			if r.Phase != "completed" && r.Phase != "cleaning" {
				c.phase(r, "failed")
			}
		}
		if life.Completed || life.Exited || life.Failed {
			// Retain terminal evidence even if the subsequent remote cleanup
			// fails, so an old journal cannot still advertise an active job.
			if err = c.Save(c.State); err != nil {
				return err
			}
		}
		retire := false
		switch r.Phase {
		case "provisioning", "starting", "recovering":
			retire = life.Failed || (c.draining && !c.gracefulDrain) || c.Now().Sub(r.CreatedAt) >= time.Duration(c.Config.Runners.StartupTimeoutSeconds)*time.Second
		case "completed":
			retire = c.Now().Sub(r.CompletedAt) >= time.Duration(c.Config.Runners.CompletionGraceSeconds)*time.Second
		case "cleaning", "failed":
			retire = true
		case "online":
			if (c.draining && (!c.gracefulDrain || c.ownedDemand() == 0)) || (!c.gracefulDrain && c.Now().Sub(r.CreatedAt) >= time.Duration(c.Config.Runners.StartupTimeoutSeconds)*time.Second) {
				retire = true
			} else if c.session != nil && c.statistics != nil && c.Now().Sub(c.statisticsAt) < 3*time.Minute && len(c.State.Runners) > Desired(c.Config.Runners.Minimum, c.Config.Runners.Maximum, c.statistics) {
				if r.IdleSince.IsZero() {
					r.IdleSince = c.Now()
				}
				retire = c.Now().Sub(r.IdleSince) >= time.Duration(c.Config.Runners.IdleGraceSeconds)*time.Second
			} else {
				r.IdleSince = time.Time{}
			}
		}
		if life.Exited || life.Failed {
			retire = true
		}
		if retire {
			if err = c.cleanup(ctx, r, true, true); err != nil {
				if errors.Is(err, scaleset.JobStillRunningError) {
					c.phase(r, "busy")
					// The busy removal fence prevents deletion; it is not proof
					// that old assignment metadata is still executing.
					r.ExecutionObservedAt = ""
					continue
				}
				return err
			}
		}
	}
	if err = c.Save(c.State); err != nil {
		return err
	}
	if (c.draining && !c.gracefulDrain) || c.session == nil {
		return nil
	}
	if c.statistics == nil || c.Now().Sub(c.statisticsAt) > 3*time.Minute {
		c.needStatistics = true
	}
	if c.needStatistics {
		stats, err := c.API.Statistics(ctx, c.State.ScaleSetID)
		if err != nil {
			c.statistics = nil
			return err
		}
		if stats == nil {
			c.statistics = nil
			return errors.New("scale-set statistics are unavailable")
		}
		c.statistics = stats
		c.statisticsAt = c.Now()
		c.needStatistics = false
	}
	if c.statistics == nil || c.Now().Sub(c.statisticsAt) > 3*time.Minute {
		return nil
	}
	desired := Desired(c.Config.Runners.Minimum, c.Config.Runners.Maximum, c.statistics)
	if c.gracefulDrain {
		desired = min(c.Config.Runners.Maximum, c.ownedDemand())
	}
	if c.Now().Before(c.retryAt) {
		return nil
	}
	if desired > len(c.State.Runners) {
		c.Logger.Info("scaling runners", "current", len(c.State.Runners), "desired", desired, "assignedJobs", c.statistics.TotalAssignedJobs, "maximum", c.Config.Runners.Maximum)
	}
	for len(c.State.Runners) < desired {
		admission, err := c.HyperV.Capacity(ctx)
		if err != nil {
			c.lastAdmission = nil
			return c.provisioningProblem("Failed", fmt.Errorf("measure VM admission: %w", err))
		}
		c.recordAdmission(admission)
		if !admission.Ready {
			return c.provisioningProblem("Deferred", errors.New(admission.Reason))
		}
		if err := c.provision(ctx); err != nil {
			var shortage *hyperv.CapacityError
			if errors.As(err, &shortage) {
				c.recordAdmission(shortage.Admission)
				return c.provisioningProblem("Deferred", err)
			}
			return c.provisioningProblem("Failed", err)
		}
		c.retryAt = time.Time{}
		c.provisioningFailures = 0
	}
	return nil
}

func (c *Controller) provision(ctx context.Context) (err error) {
	name := c.Config.GitHub.ScaleSet + "-" + c.State.Owner[:8] + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	r := &Runner{Name: name, Phase: "provisioning", PhaseSince: stamp(c.Now()), CreatedAt: c.Now()}
	c.State.Runners[name] = r
	defer func() {
		if err != nil {
			c.phase(r, "failed")
		}
	}()
	// Journal the deterministic name before any remote side effect, including JIT.
	if err := c.Save(c.State); err != nil {
		return err
	}
	jit, err := c.API.JIT(ctx, name, c.State.ScaleSetID)
	if err != nil {
		return fmt.Errorf("generate runner JIT: %w", err)
	}
	if jit == nil || jit.Runner == nil || jit.Runner.ID < 1 || jit.Runner.Name != name || jit.Runner.RunnerScaleSetID != c.State.ScaleSetID || jit.EncodedJITConfig == "" {
		return errors.New("invalid JIT response")
	}
	c.Redactor.Set(name, jit.EncodedJITConfig)
	r.RunnerID = jit.Runner.ID
	if err = c.Save(c.State); err != nil {
		return err
	}
	if c.pendingJIT == nil {
		c.pendingJIT = map[string]string{}
	}
	c.pendingJIT[name] = jit.EncodedJITConfig
	r.VMID, err = c.HyperV.Create(ctx, name, c.ImageID, r.RunnerID)
	if err != nil {
		return fmt.Errorf("create runner VM: %w", err)
	}
	c.phase(r, "starting")
	if err = c.Save(c.State); err != nil {
		return err
	}
	if err = c.HyperV.Start(ctx, name); err != nil {
		return fmt.Errorf("start runner VM: %w", err)
	}
	if err = c.Save(c.State); err != nil {
		return err
	}
	c.Logger.Info("runner provisioned", "runner", name, "VMId", r.VMID)
	return nil
}

func (c *Controller) cleanup(ctx context.Context, r *Runner, present, running bool) error {
	previousPhase := r.Phase
	// Find by exact journaled name even when a JIT response was lost. Check scale
	// membership before deleting. GitHub rejects removal while a job is running;
	// that server-side fence also closes the idle-check/job-start race.
	remote, err := c.API.Find(ctx, r.Name)
	if err != nil {
		return err
	}
	if remote != nil {
		if remote.Name != r.Name || remote.RunnerScaleSetID != c.State.ScaleSetID {
			return errors.New("runner identity mismatch; refusing deletion")
		}
		if err = c.API.Remove(ctx, remote.ID); err != nil {
			return fmt.Errorf("remove runner registration: %w", err)
		}
	}
	c.phase(r, "cleaning")
	r.Job, r.RequestID, r.ExecutionObservedAt = nil, 0, ""
	if err = c.Save(c.State); err != nil {
		return err
	}
	if err = c.HyperV.Archive(ctx, r.Name, present); err != nil {
		return fmt.Errorf("preserve runner diagnostics: %w", err)
	}
	if present && running {
		if err = c.HyperV.Stop(ctx, r.Name); err != nil {
			return err
		}
	}
	// Remove also handles durable partial creates with no registered VM.
	if err = c.HyperV.Remove(ctx, r.Name); err != nil {
		return err
	}
	if err = c.HyperV.ForgetDiagnostics(r.Name); err != nil {
		return err
	}
	delete(c.State.Runners, r.Name)
	delete(c.pendingJIT, r.Name)
	c.needStatistics = true
	// Releasing a VM can remove the resource shortage. Recheck immediately rather
	// than making a waiting job sit out the remaining admission backoff.
	if previousPhase == "busy" || previousPhase == "completed" || previousPhase == "online" || previousPhase == "recovering" || previousPhase == "cleaning" {
		c.retryAt = time.Time{}
		c.provisioningFailures = 0
	}
	if err = c.Save(c.State); err != nil {
		return err
	}
	c.Logger.Info("runner cleaned up", "runner", r.Name)
	c.Redactor.Forget(r.Name)
	return nil
}
