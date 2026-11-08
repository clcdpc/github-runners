package jobs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v79/github"
)

const LookupTimeout = 5 * time.Second

type WorkflowJobsAPI interface {
	ListWorkflowJobs(context.Context, string, string, int64, *gh.ListWorkflowJobsOptions) (*gh.Jobs, *gh.Response, error)
}

type runLookup struct {
	owner, repo string
	runID       int64
	rows        []int
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Enrich makes read-only, on-demand GETs grouped by repository/run. It does not
// cache or persist results. The caller supplies the existing authenticated REST
// client; failure or missing repository Actions read access leaves local data.
func Enrich(ctx context.Context, report *Report, api WorkflowJobsAPI) {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	groups := []runLookup{}
	byRun := map[string]int{}
	for i := range report.Runners {
		r := &report.Runners[i]
		if r.Phase != "busy" || r.WorkflowRunID < 1 || r.Repository == "" || r.RunnerID < 1 {
			continue
		}
		if !repositoryPattern.MatchString(r.Repository) {
			r.LookupError = "invalid assignment repository; lookup skipped"
			continue
		}
		key := fmt.Sprintf("%s/%d", strings.ToLower(r.Repository), r.WorkflowRunID)
		index, exists := byRun[key]
		if !exists {
			parts := strings.SplitN(r.Repository, "/", 2)
			index = len(groups)
			groups = append(groups, runLookup{owner: parts[0], repo: parts[1], runID: r.WorkflowRunID})
			byRun[key] = index
		}
		groups[index].rows = append(groups[index].rows, i)
	}
	work := make(chan runLookup, len(groups))
	for _, group := range groups {
		work <- group
	}
	close(work)
	var workers sync.WaitGroup
	for range min(3, len(groups)) {
		workers.Go(func() {
			for group := range work {
				items, err := workflowJobs(ctx, api, group)
				for _, i := range group.rows {
					r := &report.Runners[i]
					if err != nil {
						r.LookupError = lookupError(err)
						continue
					}
					var match *gh.WorkflowJob
					ambiguous := false
					for _, job := range items {
						if job != nil && job.GetID() > 0 && job.GetRunID() == r.WorkflowRunID &&
							job.GetRunnerID() == int64(r.RunnerID) && job.GetRunnerName() == r.Name {
							if match != nil && match.GetID() != job.GetID() {
								ambiguous = true
							}
							match = job
						}
					}
					if ambiguous || match == nil {
						r.LookupError = "workflow job not uniquely matched to runner ID and name"
						continue
					}
					applyWorkflowJob(r, match, report.GeneratedAt)
				}
			}
		})
	}
	workers.Wait()
}

func workflowJobs(ctx context.Context, api WorkflowJobsAPI, group runLookup) ([]*gh.WorkflowJob, error) {
	var result []*gh.WorkflowJob
	opts := &gh.ListWorkflowJobsOptions{Filter: "all", ListOptions: gh.ListOptions{PerPage: 100, Page: 1}}
	for range 5 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		jobs, response, err := api.ListWorkflowJobs(ctx, group.owner, group.repo, group.runID, opts)
		if err != nil {
			return nil, err
		}
		if jobs == nil {
			return nil, errors.New("empty workflow jobs response")
		}
		result = append(result, jobs.Jobs...)
		if response == nil || response.NextPage == 0 {
			return result, nil
		}
		if response.NextPage <= opts.Page {
			return nil, errors.New("invalid workflow jobs pagination")
		}
		opts.Page = response.NextPage
	}
	// An incomplete result cannot prove that a runner match is unique.
	return nil, errors.New("workflow jobs pagination limit reached")
}

func lookupError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "GitHub lookup timed out or was cancelled; local metadata retained"
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return fmt.Sprintf("GitHub lookup HTTP %d; local metadata retained", response.Response.StatusCode)
	}
	// Network/server text is untrusted and may contain request credentials.
	return "GitHub lookup unavailable; local metadata retained"
}

func applyWorkflowJob(r *Runner, job *gh.WorkflowJob, now time.Time) {
	if job.GetStatus() == "completed" {
		// Keep the occupied runner row, but never present its previous assignment
		// as active just because the journal's listener event was delayed.
		*r = Runner{Name: r.Name, ShortName: r.ShortName, RunnerID: r.RunnerID, VMID: r.VMID, Phase: r.Phase,
			State: "Completing", ExecutionState: "completed", ExecutionSource: "github-api", MetadataStatus: "completed"}
		return
	}
	r.WorkflowJobID, r.RunAttempt = job.GetID(), job.GetRunAttempt()
	r.WorkflowName = clean(job.GetWorkflowName())
	if job.GetName() != "" {
		r.JobDisplayName = clean(job.GetName())
	}
	r.JobURL, r.MetadataStatus = clean(job.GetHTMLURL()), "github-api"
	if started := job.StartedAt; started != nil {
		if value := Timestamp(started.UTC().Format(time.RFC3339Nano), now); value != nil {
			r.JobStartedAt, r.ElapsedSource = value, "github-api"
		}
	}
	switch job.GetStatus() {
	case "in_progress":
		r.State, r.ExecutionState, r.ExecutionSource = "Running", "running", "github-api"
	case "queued", "waiting", "pending", "requested":
		r.State, r.ExecutionState, r.ExecutionSource = "Assigned", "starting", "github-api"
	default:
		r.State, r.ExecutionState, r.ExecutionSource = "Unknown", "unknown", "github-api"
	}
	r.RecalculateElapsed(now)
}
