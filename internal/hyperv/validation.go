package hyperv

import (
	"fmt"
	"strings"
)

// Keep the controller's contract independent of the streamed guest program:
// an early EOF, omitted full block, or old success-only script cannot pass.
func requiredValidationStages(full bool) []string {
	stages := []string{
		"docker-daemon", "docker-compose", "docker-buildx", "dotnet-sdk", "node", "npm",
		"powershell", "python", "git", "runner", "github-dns", "github-https",
		"docker-run", "docker-build", "docker-exec", "compose",
	}
	if full {
		stages = append(stages,
			"sql-resources", "sql-server", "sql-query-container", "sql-port", "sql-query-host",
			"sql-localhost-socket", "dotnet-sql", "dotnet-tests", "windows-publish", "windows-package",
			"chromium-dependencies", "chromium-browser", "sql-still-running",
		)
	}
	return stages
}

func validationEvidence(output string, full bool) (completed []string, err error) {
	completed = []string{}
	required := requiredValidationStages(full)
	mode := "smoke"
	if full {
		mode = "full"
	}
	complete, passed := false, false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if passed {
			return completed, fmt.Errorf("guest output continued after validation success")
		}
		switch {
		case strings.HasPrefix(line, "CLC_VALIDATION_STAGE_PASSED:"):
			stage := strings.TrimPrefix(line, "CLC_VALIDATION_STAGE_PASSED:")
			if complete || len(completed) >= len(required) || required[len(completed)] != stage {
				return completed, fmt.Errorf("unexpected, duplicate or out-of-order validation stage %q", stage)
			}
			completed = append(completed, stage)
		case strings.HasPrefix(line, "CLC_VALIDATION_COMPLETED:"):
			if complete || line != "CLC_VALIDATION_COMPLETED:"+mode || len(completed) != len(required) {
				return completed, fmt.Errorf("guest %s completion has wrong mode or missing mandatory stages: %v", mode, required[len(completed):])
			}
			complete = true
		case line == "CLC_VALIDATION_PASSED":
			if !complete {
				return completed, fmt.Errorf("guest claimed success without completing mandatory %s stages: %v", mode, required[len(completed):])
			}
			passed = true
		}
	}
	if !passed {
		return completed, fmt.Errorf("guest did not return complete %s validation evidence; missing stages: %v", mode, required[len(completed):])
	}
	return completed, nil
}
