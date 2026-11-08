package controller

import (
	"context"
	"errors"
	"time"
)

// BeginDrain shares the acquisition/provisioning mutex. Once it returns, a
// callback already holding the mutex has finished and no later callback can
// acquire work. The listener stays alive for ownership/completion messages.
func (c *Controller) BeginDrain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.draining, c.gracefulDrain = true, true
	c.drainZeroSince = time.Time{}
	c.needStatistics = true
}

func (c *Controller) IsDraining() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.draining }

func (c *Controller) ownedDemand() int {
	n := len(c.State.Acquisitions)
	if c.statistics != nil {
		n = max(n, c.statistics.TotalAssignedJobs, c.statistics.TotalAcquiredJobs, c.statistics.TotalRunningJobs, c.statistics.TotalBusyRunners)
	}
	return n
}

type Quiescence struct {
	Ready             bool   `json:"ready"`
	ActiveAssignments int    `json:"activeAssignments"`
	CleanupPending    int    `json:"cleanupPending"`
	Blocker           string `json:"blocker,omitempty"`
}

// CheckQuiescence serializes behind all JIT, VM, registration and cleanup calls.
// A fresh remote fence, successful VM inventory and empty ownership journal are
// all required. Available (unacquired) GitHub jobs deliberately do not block it.
func (c *Controller) CheckQuiescence(ctx context.Context) (q Quiescence) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q.ActiveAssignments = c.ownedDemand()
	for _, runner := range c.State.Runners {
		if runner.Phase == "completed" || runner.Phase == "cleaning" || runner.Phase == "failed" {
			q.CleanupPending++
		}
	}
	if !c.gracefulDrain {
		q.Blocker = "Assignment acceptance is not fenced"
		return
	}
	stats, err := c.API.Statistics(ctx, c.State.ScaleSetID)
	if err != nil || stats == nil {
		c.drainZeroSince = time.Time{}
		q.Blocker = "GitHub ownership statistics unavailable"
		return
	}
	c.statistics, c.statisticsAt = stats, c.Now()
	// Remote zero reconciles missed cancellation/completion messages and a lost
	// acquisition reply. Never use an old cached zero to discard ownership.
	if c.reconcileAcquisitions() {
		if err = c.Save(c.State); err != nil {
			q.Blocker = "Cannot persist ownership fence"
			return
		}
	}
	q.ActiveAssignments = c.ownedDemand()
	if q.ActiveAssignments > 0 {
		q.Blocker = "Accepted jobs still running or waiting for a runner"
		return
	}
	if len(c.State.Runners) != 0 || len(c.pendingJIT) != 0 {
		q.Blocker = "Runner provisioning or cleanup in progress"
		return
	}
	vms, err := c.HyperV.List(ctx)
	if err != nil {
		q.Blocker = "Cannot verify owned VM cleanup"
		return
	}
	if len(vms) != 0 {
		q.CleanupPending = len(vms)
		q.Blocker = "Owned VM cleanup in progress"
		return
	}
	if stats.TotalRegisteredRunners != 0 {
		q.Blocker = "Runner deregistration in progress"
		return
	}
	q.Ready = true
	return
}

// PollCapacity is checked by every poll, including reconnects. A poll already
// on the wire at the drain transition may return offers; Scale declines those.
func (c *Controller) PollCapacity(maximum int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining {
		return 0
	}
	return maximum
}

// RefreshDrainDemand also reconciles cancellation when no completion event is
// delivered. Missing API evidence never makes a drain ready.
func (c *Controller) RefreshDrainDemand(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.gracefulDrain {
		return errors.New("not draining")
	}
	stats, err := c.API.Statistics(ctx, c.State.ScaleSetID)
	if err != nil {
		c.drainZeroSince = time.Time{}
		return err
	}
	if stats == nil {
		c.drainZeroSince = time.Time{}
		return errors.New("statistics unavailable")
	}
	c.statistics, c.statisticsAt = stats, c.Now()
	c.reconcileAcquisitions()
	c.needStatistics = false
	return c.Save(c.State)
}

// Never discard a durable acquisition intent on a single possibly lagging
// statistics response. Normal completion messages release exact request IDs;
// missed cancellation/completion and crash recovery require repeated fresh
// remote zero observations separated by a stabilization window.
func (c *Controller) reconcileAcquisitions() bool {
	stats := c.statistics
	if stats == nil || stats.TotalAssignedJobs != 0 || stats.TotalAcquiredJobs != 0 || stats.TotalRunningJobs != 0 || stats.TotalBusyRunners != 0 {
		c.drainZeroSince = time.Time{}
		return false
	}
	if len(c.State.Acquisitions) == 0 {
		return false
	}
	if c.drainZeroSince.IsZero() {
		c.drainZeroSince = c.Now()
		return false
	}
	if c.Now().Sub(c.drainZeroSince) < max(15*time.Second, 2*time.Duration(c.Config.Controller.ReconcileIntervalSeconds)*time.Second) {
		return false
	}
	c.State.Acquisitions = nil
	return true
}

func (c *Controller) CancelDrain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.draining, c.gracefulDrain = false, false
	c.needStatistics = true
}
