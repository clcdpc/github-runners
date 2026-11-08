package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	githubapi "github.com/clcdpc/github-runners/internal/github"
	"github.com/clcdpc/github-runners/internal/jobs"
	"github.com/clcdpc/github-runners/internal/restart"
	"github.com/clcdpc/github-runners/internal/secure"
)

func readRuntime(c config.Config, executable string, now time.Time) (runtimeStatus, bool) {
	var rt runtimeStatus
	if b, err := os.ReadFile(filepath.Join(c.Controller.DataDirectory, "status.json")); err == nil {
		json.Unmarshal(b, &rt)
	}
	rt.ProcessAlive = secure.ProcessRunning(rt.PID, rt.StartedAt, executable)
	if !rt.ProcessAlive && rt.Status != "stopped" {
		rt.Status = "not-running"
	}
	fresh := rt.ProcessAlive && now.Sub(rt.UpdatedAt) >= 0 && now.Sub(rt.UpdatedAt) < time.Duration(max(60, 3*c.Controller.ReconcileIntervalSeconds))*time.Second
	return rt, fresh
}

func jobsReport(c config.Config, executable string, now time.Time) (jobs.Report, error) {
	s, err := controller.InspectState(filepath.Join(c.Controller.DataDirectory, "state.json"), c)
	if err != nil && !os.IsNotExist(err) {
		return jobs.Report{}, err
	}
	rt, fresh := readRuntime(c, executable, now)
	if s == nil {
		rt.Status = "uninitialized"
	}
	status := jobs.ControllerStatus{Status: rt.Status, ProcessAlive: rt.ProcessAlive, Fresh: fresh,
		StartedAt: jobs.Timestamp(rt.StartedAt.Format(time.RFC3339Nano), now), UpdatedAt: jobs.Timestamp(rt.UpdatedAt.Format(time.RFC3339Nano), now)}
	restartState := restart.Read(filepath.Join(c.Controller.DataDirectory, "restart.json"))
	status.Restart = &restartState
	return jobs.Build(s, status, c.Runners.Maximum, now, time.Duration(max(60, 3*c.Controller.ReconcileIntervalSeconds))*time.Second), nil
}

func writeJobs(ctx context.Context, w io.Writer, c config.Config, executable string, asJSON bool) error {
	report, err := jobsReport(c, executable, time.Now())
	if err != nil {
		return err
	}
	if report.LookupTargets() {
		pat, err := secure.ReadPAT(c.Controller.PATFile)
		if err != nil {
			for i := range report.Runners {
				if report.Runners[i].Phase == "busy" && report.Runners[i].WorkflowRunID > 0 {
					report.Runners[i].LookupError = "saved credentials unavailable; local metadata retained"
				}
			}
		} else {
			jobs.Enrich(ctx, &report, githubapi.REST(pat).Actions)
		}
	}
	if asJSON {
		return json.NewEncoder(w).Encode(report)
	}
	return jobs.WriteTable(w, report)
}
