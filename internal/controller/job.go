package controller

import (
	"strings"
	"time"

	"github.com/actions/scaleset"
)

// JobAssignment contains only nonsecret assignment evidence. AssignmentJobID is
// the scale-set protocol's opaque ID, NOT the REST API's numeric workflow job ID.
// The protocol does not expose the workflow name, YAML job key or run attempt.
type JobAssignment struct {
	Repository      string `json:"repository,omitempty"`
	WorkflowRef     string `json:"workflowRef,omitempty"`
	DisplayName     string `json:"displayName,omitempty"`
	WorkflowRunID   int64  `json:"workflowRunId,omitempty"`
	AssignmentJobID string `json:"assignmentJobId,omitempty"`
	AssignedAt      string `json:"assignedAt,omitempty"`
	StartedAt       string `json:"startedAt,omitempty"`
	StartSource     string `json:"startSource,omitempty"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (c *Controller) phase(r *Runner, phase string) {
	if r.Phase != phase {
		r.Phase = phase
		r.PhaseSince = stamp(c.Now())
	}
}

func (c *Controller) jobStarted(job *scaleset.JobStarted) {
	r := c.State.Runners[job.RunnerName]
	if !matchesAssignment(r, job.RunnerID, job.RunnerRequestID, job.JobID) || r.Phase == "completed" || r.Phase == "cleaning" {
		return
	}
	c.phase(r, "busy")
	// Listener messages can be replayed after restart. The normal lifecycle
	// probe (or a read-only API lookup) confirms that execution is still active.
	if job.RunnerRequestID > 0 {
		r.RequestID = job.RunnerRequestID
	}
	if r.Job == nil {
		r.Job = &JobAssignment{}
	}
	// Replayed events may fill delayed fields, but never reset an execution start.
	if job.OwnerName != "" && job.RepositoryName != "" {
		r.Job.Repository = strings.TrimRight(job.OwnerName, "/") + "/" + job.RepositoryName
	}
	if job.JobWorkflowRef != "" {
		r.Job.WorkflowRef = job.JobWorkflowRef
	}
	if job.JobDisplayName != "" {
		r.Job.DisplayName = job.JobDisplayName
	}
	if job.WorkflowRunID > 0 {
		r.Job.WorkflowRunID = job.WorkflowRunID
	}
	if job.JobID != "" {
		r.Job.AssignmentJobID = job.JobID
	}
	if !job.RunnerAssignTime.IsZero() {
		r.Job.AssignedAt = stamp(job.RunnerAssignTime)
	}
}

func (c *Controller) jobCompleted(job *scaleset.JobCompleted) {
	r := c.State.Runners[job.RunnerName]
	if !matchesAssignment(r, job.RunnerID, job.RunnerRequestID, job.JobID) || r.Phase == "cleaning" {
		return
	}
	c.complete(r)
}

func matchesAssignment(r *Runner, runnerID int, requestID int64, jobID string) bool {
	return r != nil && (runnerID == 0 || runnerID == r.RunnerID) &&
		(requestID == 0 || r.RequestID == 0 || requestID == r.RequestID) &&
		(jobID == "" || r.Job == nil || r.Job.AssignmentJobID == "" || jobID == r.Job.AssignmentJobID)
}

func (c *Controller) complete(r *Runner) {
	c.phase(r, "completed")
	if r.CompletedAt.IsZero() {
		r.CompletedAt = c.Now()
	}
	r.Job = nil
	r.RequestID = 0
	r.ExecutionObservedAt = ""
}
