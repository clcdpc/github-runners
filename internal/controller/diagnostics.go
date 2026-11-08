package controller

import (
	"context"
	"errors"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/clcdpc/github-runners/internal/hyperv"
)

type ProvisioningIssue struct {
	At       time.Time `json:"at"`
	Decision string    `json:"decision"`
	Reason   string    `json:"reason"`
}

// Capacity keeps configured slots separate from measured resource admission and
// the total maximum last reported to GitHub. The protocol expects a TOTAL cap,
// not maximum minus the busy count, even when all slots are occupied.
type Capacity struct {
	Maximum               int                `json:"maximum"`
	Minimum               int                `json:"minimum"`
	CurrentRunners        int                `json:"currentRunners"`
	BusyRunners           int                `json:"busyRunners"`
	AvailableSlots        int                `json:"availableSlots"`
	AssignedJobs          int                `json:"assignedJobs"`
	DesiredRunners        int                `json:"desiredRunners"`
	StatisticsAt          time.Time          `json:"statisticsAt"`
	ListenerConnected     bool               `json:"listenerConnected"`
	ListenerMaximum       int                `json:"listenerMaximum"`
	AdvertisedMaximum     int                `json:"advertisedMaximum"`
	AdvertisedAt          time.Time          `json:"advertisedAt"`
	Advertising           bool               `json:"advertising"`
	ResourceBlocked       bool               `json:"resourceBlocked"`
	NextProvisionAttempt  time.Time          `json:"nextProvisionAttempt"`
	LastAdmission         *hyperv.Admission  `json:"lastAdmission,omitempty"`
	LastProvisioningIssue *ProvisioningIssue `json:"lastProvisioningIssue,omitempty"`
}

func (c *Controller) Capacity() Capacity {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := InventoryCapacity(c.State, c.Config.Runners.Maximum)
	d.Minimum = c.Config.Runners.Minimum
	d.DesiredRunners, d.StatisticsAt = Desired(c.Config.Runners.Minimum, c.Config.Runners.Maximum, c.statistics), c.statisticsAt
	d.ListenerConnected = c.session != nil
	d.AdvertisedMaximum, d.AdvertisedAt = c.advertisedMaximum, c.advertisedAt
	d.NextProvisionAttempt = c.retryAt
	if c.statistics != nil {
		d.AssignedJobs = c.statistics.TotalAssignedJobs
	}
	if d.ListenerConnected {
		d.ListenerMaximum = c.Config.Runners.Maximum
		if c.draining {
			d.ListenerMaximum = 0
		}
		d.Advertising = !c.draining && !d.AdvertisedAt.IsZero() && c.Now().Sub(d.AdvertisedAt) < 3*time.Minute
	}
	if c.lastAdmission != nil {
		copy := *c.lastAdmission
		d.LastAdmission = &copy
		d.ResourceBlocked = !copy.Ready && d.DesiredRunners > d.CurrentRunners
	}
	if c.State.LastProvisioningIssue != nil {
		copy := *c.State.LastProvisioningIssue
		d.LastProvisioningIssue = &copy
	}
	return d
}

// InventoryCapacity counts every journaled slot, including cleanup and partial
// provisioning. A removed runner is absent from the journal, not a phase filter.
func InventoryCapacity(s *State, maximum int) Capacity {
	d := Capacity{Maximum: maximum, AvailableSlots: maximum}
	if s == nil {
		return d
	}
	d.CurrentRunners = len(s.Runners)
	d.AvailableSlots = max(0, maximum-d.CurrentRunners)
	for _, r := range s.Runners {
		if r.Phase == "busy" {
			d.BusyRunners++
		}
	}
	return d
}

func (c *Controller) recordAdmission(a hyperv.Admission) {
	c.lastAdmission = &a
	c.Logger.Info("VM admission", "runnerLimit", c.Config.Runners.Maximum, "currentRunners", len(c.State.Runners),
		"availableSlots", max(0, c.Config.Runners.Maximum-len(c.State.Runners)), "decision", a.Decision, "reason", a.Reason,
		"requestedMemoryGiB", a.RequestedMemoryGiB, "availableMemoryGiB", float64(a.AvailableMemoryBytes)/(1<<30),
		"reserveMemoryGiB", a.ReserveMemoryGiB, "measurements", a)
}

func (c *Controller) provisioningProblem(decision string, err error) error {
	c.provisioningFailures++
	delay := time.Duration(max(30, c.Config.Controller.RetrySeconds)) * time.Second
	for i := 1; i < min(c.provisioningFailures, 5); i++ {
		delay *= 2
	}
	delay = min(delay, 5*time.Minute)
	c.retryAt = c.Now().Add(delay)
	c.State.LastProvisioningIssue = &ProvisioningIssue{At: c.Now(), Decision: decision, Reason: c.Redactor.Text(err.Error())}
	c.Logger.Warn("runner provisioning delayed", "decision", decision, "reason", err, "retryAt", c.retryAt,
		"runnerLimit", c.Config.Runners.Maximum, "currentRunners", len(c.State.Runners))
	saveErr := c.Save(c.State)
	if decision == "Deferred" {
		// Assigned demand remains in GitHub statistics; the reconciliation timer
		// retries it without discarding a healthy listener or creating JIT churn.
		return saveErr
	}
	return errors.Join(err, saveErr)
}

type observedSession struct {
	listener.Client
	controller *Controller
}

// ObserveSession records successful capacity reports made by the official
// listener, including idle long-poll responses. It does not change the cap.
func (c *Controller) ObserveSession(session listener.Client) listener.Client {
	return &observedSession{Client: session, controller: c}
}

func (s *observedSession) GetMessage(ctx context.Context, last, maximum int) (*scaleset.RunnerScaleSetMessage, error) {
	maximum = s.controller.PollCapacity(maximum)
	msg, err := s.Client.GetMessage(ctx, last, maximum)
	if err == nil {
		c := s.controller
		c.mu.Lock()
		if c.session != nil {
			c.advertisedMaximum, c.advertisedAt = maximum, c.Now()
		}
		c.mu.Unlock()
	}
	return msg, err
}
