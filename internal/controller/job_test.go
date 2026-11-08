package controller

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/clcdpc/github-runners/internal/hyperv"
)

func TestJobAssignmentsAndCompletionAreIsolatedAcrossThreeRunners(t *testing.T) {
	f := newFixture(t)
	f.c.Config.Runners.Maximum, f.api.assigned = 3, 3
	f.scale(t, 3)
	names := make([]string, 0, 3)
	for name := range f.c.State.Runners {
		names = append(names, name)
	}
	slices.Sort(names)
	message := &scaleset.RunnerScaleSetMessage{}
	for i, name := range names {
		r := f.c.State.Runners[name]
		message.JobStartedMessages = append(message.JobStartedMessages, &scaleset.JobStarted{RunnerID: r.RunnerID, RunnerName: name,
			JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: int64(200 + i), OwnerName: "clcdpc", RepositoryName: "same-repository",
				JobDisplayName: "Same display name", JobID: "opaque-" + name, WorkflowRunID: int64(300 + i), JobWorkflowRef: "clcdpc/same-repository/.github/workflows/ci.yml@refs/heads/master", RunnerAssignTime: f.now}})
		f.ws.life[name] = hyperv.Lifecycle{Configured: true, Listening: true, Started: true, StartedAt: f.now.Add(-time.Duration(i+1) * time.Minute)}
	}
	if err := f.c.Scale(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		r := f.c.State.Runners[name]
		if r.Job == nil || r.Job.Repository != "clcdpc/same-repository" || r.Job.WorkflowRunID != int64(300+i) || r.RequestID != int64(200+i) ||
			r.Job.AssignmentJobID != "opaque-"+name || r.Job.StartedAt != stamp(f.now.Add(-time.Duration(i+1)*time.Minute)) || r.ExecutionObservedAt == "" {
			t.Fatal("assignments were mixed", r)
		}
	}
	before, _ := json.Marshal(f.c.State.Runners[names[1]])
	message = &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerID: f.c.State.Runners[names[0]].RunnerID, RunnerName: names[0], JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 200}, Result: "canceled"}}}
	if err := f.c.Scale(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	completed := f.c.State.Runners[names[0]]
	if completed.Job != nil || completed.RequestID != 0 || completed.ExecutionObservedAt != "" || completed.Phase != "completed" {
		t.Fatal("completed assignment not cleared", completed)
	}
	after, _ := json.Marshal(f.c.State.Runners[names[1]])
	if string(before) != string(after) {
		t.Fatal("one completion altered another runner", string(before), string(after))
	}
	// Duplicate starts after completion must never resurrect an assignment.
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: names[0]}}}); err != nil {
		t.Fatal(err)
	}
	if completed.Job != nil || completed.Phase != "completed" {
		t.Fatal("replayed start resurrected completed job")
	}
	f.exit(names[0])
	f.api.assigned = 2
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.State.Runners[names[0]] != nil || f.c.State.Runners[names[1]].Job == nil || f.c.State.Runners[names[2]].Job == nil {
		t.Fatal("retirement cleared an unrelated assignment")
	}
}

func TestStaleOrForeignJobEventsCannotClearCurrentAssignment(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	r := f.c.State.Runners[name]
	r.RequestID = 50
	r.Job = &JobAssignment{Repository: "clcdpc/current", AssignmentJobID: "current-job"}
	r.Phase = "busy"
	for _, event := range []*scaleset.JobCompleted{
		{RunnerName: name, RunnerID: r.RunnerID + 1, JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 50}},
		{RunnerName: name, RunnerID: r.RunnerID, JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 49}},
		{RunnerName: name, RunnerID: r.RunnerID, JobMessageBase: scaleset.JobMessageBase{JobID: "previous-job"}},
		{RunnerName: "another-runner", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 50}},
	} {
		if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{event}}); err != nil {
			t.Fatal(err)
		}
		if r.Job == nil || r.Job.AssignmentJobID != "current-job" || r.RequestID != 50 {
			t.Fatal("stale completion cleared current assignment")
		}
	}
	f.c.jobStarted(&scaleset.JobStarted{RunnerName: name, RunnerID: r.RunnerID, JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 49, JobDisplayName: "old job"}})
	if r.Job.DisplayName != "" {
		t.Fatal("stale start replaced current assignment")
	}
}

func TestReplayedStartDoesNotConfirmExecutionAfterRestart(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	r := f.c.State.Runners[f.name()]
	r.Phase, r.RequestID = "busy", 50
	r.Job = &JobAssignment{Repository: "clcdpc/current", AssignmentJobID: "current-job"}
	f.c.jobStarted(&scaleset.JobStarted{RunnerName: r.Name, RunnerID: r.RunnerID,
		JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 50, JobID: "current-job"}})
	if r.ExecutionObservedAt != "" || r.RequestID != 50 {
		t.Fatal("replayed start confirmed stale execution")
	}
	f.c.jobStarted(&scaleset.JobStarted{RunnerName: r.Name, RunnerID: r.RunnerID})
	if r.RequestID != 50 {
		t.Fatal("incomplete replay erased stable request identity")
	}
}

func TestJobMetadataRecoveryRequiresFreshEvidenceAndClearsOnGuestCompletion(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	r := f.c.State.Runners[name]
	r.Phase = "busy"
	r.RequestID = 50
	r.ExecutionObservedAt = stamp(f.now)
	r.Job = &JobAssignment{Repository: "clcdpc/current", DisplayName: "Run tests", WorkflowRunID: 123, StartedAt: stamp(f.now.Add(-time.Minute)), StartSource: "runner-hook"}
	if err := SaveState(f.path, f.c.State); err != nil {
		t.Fatal(err)
	}
	inspected, err := InspectState(f.path, f.c.Config)
	if err != nil || inspected.Runners[name].ExecutionObservedAt == "" {
		t.Fatal("inspection mutated observation", err)
	}
	restored, err := LoadState(f.path, f.c.Config)
	if err != nil || restored.Runners[name].ExecutionObservedAt != "" || restored.Runners[name].Job.DisplayName != "Run tests" {
		t.Fatal("restart trusted old busy metadata or lost identifiers", err)
	}
	f.c.State = restored
	f.c.Session(nil)
	f.ws.life[name] = hyperv.Lifecycle{Configured: true, Listening: true, Started: true, StartedAt: f.now.Add(-time.Minute)}
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restored.Runners[name].ExecutionObservedAt == "" || restored.Runners[name].Job.WorkflowRunID != 123 {
		t.Fatal("recovery did not reconcile guest execution")
	}
	f.ws.failure = "probe"
	if err := f.c.Tick(context.Background()); err == nil {
		t.Fatal("probe failure expected")
	}
	if restored.Runners[name].ExecutionObservedAt != "" {
		t.Fatal("failed probe retained live execution confirmation")
	}
	f.ws.failure = ""
	f.ws.life[name] = hyperv.Lifecycle{Started: true, Completed: true}
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restored.Runners[name].Job != nil || restored.Runners[name].RequestID != 0 {
		t.Fatal("completion marker retained assignment")
	}
}

func TestOldStateAndMalformedOptionalTelemetryRemainCompatible(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	var legacy map[string]any
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	row := legacy["runners"].(map[string]any)[name].(map[string]any)
	delete(row, "phaseSince")
	delete(row, "executionObservedAt")
	delete(row, "job")
	for _, malformed := range []bool{false, true} {
		if malformed {
			row["phaseSince"] = "invalid"
			row["executionObservedAt"] = "invalid"
			row["job"] = map[string]any{"startedAt": "not a date"}
		}
		data, _ = json.Marshal(legacy)
		if err = os.WriteFile(f.path, data, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := InspectState(f.path, f.c.Config)
		if err != nil || loaded.Version != 2 || loaded.Runners[name].RunnerID < 1 {
			t.Fatal("optional telemetry broke v2 compatibility", err)
		}
	}
}

func TestSnapshotsDoNotShareMutableJobMetadata(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	f.c.State.Runners[name].Job = &JobAssignment{DisplayName: "original"}
	copy := f.c.Snapshot()
	copy.Runners[name].Job.DisplayName = "modified"
	if f.c.State.Runners[name].Job.DisplayName != "original" {
		t.Fatal("snapshot shares assignment pointer")
	}
}

func TestCleanupFailureAndFailedProvisioningRemainVisible(t *testing.T) {
	f := newFixture(t)
	f.api.failJIT = true
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err == nil {
		t.Fatal("expected provisioning failure")
	}
	if f.c.State.Runners[f.name()].Phase != "failed" {
		t.Fatal("failed provisioning slot hidden")
	}
	g := newFixture(t)
	g.scale(t, 1)
	name := g.name()
	g.c.State.Runners[name].Job = &JobAssignment{DisplayName: "old job"}
	g.exit(name)
	g.ws.failure = "archive"
	if err := g.c.Tick(context.Background()); err == nil {
		t.Fatal("expected archive failure")
	}
	if r := g.c.State.Runners[name]; r == nil || r.Phase != "cleaning" || r.Job != nil || r.PhaseSince == "" {
		t.Fatal("cleanup slot or metadata incorrect", r)
	}
}

func TestGuestCompletionIsDurableBeforeRemoteCleanupFailure(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	r := f.c.State.Runners[name]
	r.Phase, r.ExecutionObservedAt = "busy", stamp(f.now)
	r.Job = &JobAssignment{Repository: "clcdpc/current", DisplayName: "Old job"}
	if err := SaveState(f.path, f.c.State); err != nil {
		t.Fatal(err)
	}
	f.ws.life[name] = hyperv.Lifecycle{Started: true, Completed: true, Exited: true}
	f.api.findError = true
	if err := f.c.Tick(context.Background()); err == nil {
		t.Fatal("expected remote lookup outage")
	}
	saved, err := InspectState(f.path, f.c.Config)
	if err != nil {
		t.Fatal(err)
	}
	if r := saved.Runners[name]; r == nil || r.Job != nil || r.ExecutionObservedAt != "" || r.Phase != "completed" {
		t.Fatal("cleanup outage left old running assignment in journal", r)
	}
}

func TestVMTerminationAndInventoryOutageCannotLeaveFreshRunningMetadata(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "inventory outage", true: "stopped VM"}[stopped], func(t *testing.T) {
			f := newFixture(t)
			f.scale(t, 1)
			name := f.name()
			r := f.c.State.Runners[name]
			r.Phase, r.ExecutionObservedAt = "busy", stamp(f.now)
			r.Job = &JobAssignment{Repository: "clcdpc/current", DisplayName: "Old job"}
			if stopped {
				f.exit(name)
				f.api.findError = true
			} else {
				f.ws.failure = "list"
			}
			if err := f.c.Tick(context.Background()); err == nil {
				t.Fatal("expected unavailable inventory/cleanup")
			}
			saved, err := InspectState(f.path, f.c.Config)
			if err != nil {
				t.Fatal(err)
			}
			if r := saved.Runners[name]; r == nil || r.ExecutionObservedAt != "" || (stopped && r.Job != nil) {
				t.Fatal("old execution confirmation survived authoritative VM failure", r)
			}
		})
	}
}
