// Package jobs builds read-only views of the controller journal. It never probes
// guests, acquires management locks, or changes the journal or GitHub resources.
package jobs

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/restart"
	"github.com/clcdpc/github-runners/internal/secure"
)

type ControllerStatus struct {
	Status       string         `json:"status"`
	ProcessAlive bool           `json:"processAlive"`
	Fresh        bool           `json:"fresh"`
	StartedAt    *time.Time     `json:"startedAt"`
	UpdatedAt    *time.Time     `json:"updatedAt"`
	Restart      *restart.State `json:"restart,omitempty"`
}

// Report is schema v1. Unknown identifiers are null/zero/empty, never guesses.
// ElapsedSeconds is null when there is no trustworthy timestamp.
type Report struct {
	SchemaVersion      int              `json:"schemaVersion"`
	GeneratedAt        time.Time        `json:"generatedAt"`
	Controller         ControllerStatus `json:"controller"`
	MaximumConcurrency int              `json:"maximumConcurrency"`
	OccupiedCapacity   int              `json:"occupiedCapacity"`
	AvailableCapacity  int              `json:"availableCapacity"`
	Runners            []Runner         `json:"runners"`
}

type Runner struct {
	Name            string     `json:"name"`
	ShortName       string     `json:"shortName"`
	RunnerID        int        `json:"runnerId"`
	VMID            string     `json:"vmId"`
	Phase           string     `json:"phase"`
	State           string     `json:"state"`
	ExecutionState  string     `json:"executionState"`
	ExecutionSource string     `json:"executionSource"`
	PhaseSince      *time.Time `json:"phaseSince"`
	Repository      string     `json:"repository"`
	WorkflowName    string     `json:"workflowName"`
	WorkflowRef     string     `json:"workflowRef"`
	YAMLJobID       string     `json:"yamlJobId"`
	JobDisplayName  string     `json:"jobDisplayName"`
	WorkflowRunID   int64      `json:"workflowRunId"`
	WorkflowJobID   int64      `json:"workflowJobId"`
	AssignmentJobID string     `json:"assignmentJobId"`
	RequestID       int64      `json:"requestId"`
	RunAttempt      int64      `json:"runAttempt"`
	JobURL          string     `json:"jobUrl"`
	AssignedAt      *time.Time `json:"assignedAt"`
	JobStartedAt    *time.Time `json:"jobStartedAt"`
	ElapsedSeconds  *int64     `json:"elapsedSeconds"`
	ElapsedSource   string     `json:"elapsedSource"`
	MetadataStatus  string     `json:"metadataStatus"`
	LookupError     string     `json:"lookupError"`
}

func Timestamp(value string, now time.Time) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.IsZero() || t.After(now) {
		return nil
	}
	t = t.UTC()
	return &t
}

func Build(s *controller.State, runtime ControllerStatus, maximum int, now time.Time, freshness time.Duration) Report {
	capacity := controller.InventoryCapacity(s, maximum)
	report := Report{SchemaVersion: 1, GeneratedAt: now.UTC(), Controller: runtime,
		MaximumConcurrency: maximum, OccupiedCapacity: capacity.CurrentRunners, AvailableCapacity: capacity.AvailableSlots,
		Runners: []Runner{}}
	if s == nil {
		return report
	}
	names := make([]string, 0, len(s.Runners))
	for name := range s.Runners {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		r := s.Runners[name]
		row := Runner{Name: name, ShortName: shortName(name), RunnerID: r.RunnerID, VMID: r.VMID, Phase: r.Phase,
			PhaseSince: Timestamp(r.PhaseSince, now), ExecutionState: "none", MetadataStatus: "not-applicable"}
		switch r.Phase {
		case "provisioning", "starting":
			row.State = "Provisioning"
		case "online":
			row.State = "Idle"
		case "busy":
			row.State, row.ExecutionState, row.MetadataStatus = "Unknown", "unknown", "missing"
			observed := Timestamp(r.ExecutionObservedAt, now)
			if runtime.ProcessAlive && runtime.Fresh && observed != nil && now.Sub(*observed) <= freshness &&
				runtime.StartedAt != nil && !observed.Before(*runtime.StartedAt) {
				row.State, row.ExecutionState, row.ExecutionSource = "Running", "running", "controller"
			}
			if r.Job != nil {
				row.Repository, row.JobDisplayName, row.WorkflowRef = clean(r.Job.Repository), clean(r.Job.DisplayName), clean(r.Job.WorkflowRef)
				row.WorkflowRunID, row.AssignmentJobID, row.RequestID = r.Job.WorkflowRunID, clean(r.Job.AssignmentJobID), r.RequestID
				row.AssignedAt, row.JobStartedAt = Timestamp(r.Job.AssignedAt, now), Timestamp(r.Job.StartedAt, now)
				if row.Repository != "" || row.JobDisplayName != "" || row.WorkflowRunID > 0 {
					row.MetadataStatus = "assignment"
				}
				if row.JobStartedAt != nil {
					row.ElapsedSource = r.Job.StartSource
				}
			}
		case "completed":
			row.State, row.ExecutionState = "Completing", "completed"
		case "cleaning":
			row.State, row.ExecutionState = "Cleaning up", "completed"
		case "failed":
			row.State, row.ExecutionState = "Failed", "unknown"
		default:
			row.State, row.ExecutionState = "Unknown", "unknown"
		}
		row.RecalculateElapsed(now)
		report.Runners = append(report.Runners, row)
	}
	return report
}

func (r *Runner) RecalculateElapsed(now time.Time) {
	r.ElapsedSeconds = nil
	var start *time.Time
	if r.ExecutionState == "running" {
		start = r.JobStartedAt
	} else if r.State == "Provisioning" || r.State == "Completing" || r.State == "Cleaning up" || r.State == "Failed" {
		start = r.PhaseSince
		if start != nil {
			r.ElapsedSource = "phase"
		}
	}
	if start != nil && !start.After(now) {
		seconds := int64(now.Sub(*start) / time.Second)
		r.ElapsedSeconds = &seconds
	} else {
		r.ElapsedSource = ""
	}
}

// LookupTargets is deliberately limited to runs identified by actual assignment
// events. Jobs never searches queued/running workflows to infer an association.
func (r Report) LookupTargets() bool {
	for _, row := range r.Runners {
		if row.Phase == "busy" && row.Repository != "" && row.WorkflowRunID > 0 && row.RunnerID > 0 {
			return true
		}
	}
	return false
}

func shortName(name string) string {
	if len(name) > 12 {
		return name[len(name)-12:]
	}
	return name
}

func clean(value string) string {
	value = (&secure.Redactor{}).Text(value)
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) }), " ")
}

func cell(value string, width int) string {
	value = clean(value)
	if value == "" {
		value = "-"
	}
	used := displayWidth(value)
	if used > width {
		var truncated strings.Builder
		used = 0
		for _, r := range value {
			if used+runeWidth(r) > width-3 {
				break
			}
			truncated.WriteRune(r)
			used += runeWidth(r)
		}
		value = truncated.String() + "..."
		used += 3
	}
	return value + strings.Repeat(" ", width-used)
}

func displayWidth(value string) int {
	width := 0
	for _, r := range value {
		width += runeWidth(r)
	}
	return width
}

func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	// Conservatively budget two cells for wide CJK/Hangul and emoji glyphs.
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe6f) ||
		(r >= 0xff01 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

func Duration(seconds int64) string {
	seconds = max(0, seconds)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%dh %dm", seconds/3600, seconds%3600/60)
}

// WriteTable stays within 80 columns. Full, untruncated values remain in JSON.
func WriteTable(w io.Writer, report Report) error {
	var b strings.Builder
	b.WriteString("CLC GitHub Runners - Active Jobs\n\n")
	if len(report.Runners) == 0 {
		b.WriteString("No active controller-managed runners.\n")
	} else {
		fmt.Fprintf(&b, "%s %s %s %s %s\n", cell("Runner", 12), cell("State", 12), cell("Repository", 22), cell("Job", 20), "Elapsed")
		for _, r := range report.Runners {
			elapsed := "-"
			if r.ElapsedSeconds != nil {
				elapsed = Duration(*r.ElapsedSeconds)
				if r.ElapsedSource == "phase" {
					elapsed += "*"
				}
			}
			fmt.Fprintf(&b, "%s %s %s %s %s\n", cell(r.ShortName, 12), cell(r.State, 12), cell(r.Repository, 22), cell(r.JobDisplayName, 20), elapsed)
		}
	}
	counts := map[string]int{}
	phaseElapsed, unavailable := false, false
	for _, r := range report.Runners {
		counts[r.State]++
		phaseElapsed = phaseElapsed || r.ElapsedSource == "phase"
		unavailable = unavailable || r.LookupError != "" || r.MetadataStatus == "missing"
	}
	var summary []string
	for _, state := range []string{"Running", "Assigned", "Provisioning", "Idle", "Completing", "Cleaning up", "Failed", "Unknown"} {
		if counts[state] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", counts[state], strings.ToLower(state)))
		}
	}
	summary = append(summary, fmt.Sprintf("%d/%d capacity", report.OccupiedCapacity, report.MaximumConcurrency))
	fmt.Fprintf(&b, "\n%s\n", strings.Join(summary, " | "))
	if r := report.Controller.Restart; r != nil && r.Pending {
		prefix := "Controller restart " + strings.TrimRight(cell(strings.ToLower(clean(r.State)), 16), " ")
		b.WriteString(prefix)
		if r.Blocker != "" {
			fmt.Fprintf(&b, " - %s", strings.TrimRight(cell(r.Blocker, max(1, 79-displayWidth(prefix)-3)), " "))
		}
		b.WriteByte('\n')
	}
	if !report.Controller.Fresh {
		fmt.Fprintf(&b, "Controller: %s; unconfirmed execution is Unknown.\n", clean(report.Controller.Status))
	}
	if phaseElapsed {
		b.WriteString("* Time in lifecycle phase; other elapsed values use job execution start.\n")
	}
	if unavailable {
		b.WriteString("Some job metadata is unavailable; see Jobs -Json for details.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
