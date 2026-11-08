package hyperv

import (
	"reflect"
	"strings"
	"testing"
)

// This acceptance list is deliberately independent of production's contract.
// Removing a required stage from both producer and consumer must break a test.
func expectedValidationStages(full bool) []string {
	stages := strings.Fields("docker-daemon docker-compose docker-buildx dotnet-sdk node npm powershell python git runner github-dns github-https docker-run docker-build docker-exec compose")
	if full {
		stages = append(stages, strings.Fields("sql-resources sql-server sql-query-container sql-port sql-query-host sql-localhost-socket dotnet-sql dotnet-tests windows-publish windows-package chromium-dependencies chromium-browser sql-still-running")...)
	}
	return stages
}

func successfulValidationOutput(full bool) string {
	var output strings.Builder
	for _, stage := range expectedValidationStages(full) {
		output.WriteString("CLC_VALIDATION_STAGE_PASSED:" + stage + "\n")
	}
	mode := "smoke"
	if full {
		mode = "full"
	}
	output.WriteString("CLC_VALIDATION_COMPLETED:" + mode + "\nCLC_VALIDATION_PASSED\n")
	return output.String()
}

func TestValidationEvidence(t *testing.T) {
	for _, full := range []bool{false, true} {
		if !reflect.DeepEqual(requiredValidationStages(full), expectedValidationStages(full)) {
			t.Fatal("required platform acceptance stages changed", requiredValidationStages(full))
		}
		output := successfulValidationOutput(full)
		completed, err := validationEvidence(strings.ReplaceAll(output, "\n", "\r\n"), full)
		if err != nil || !reflect.DeepEqual(completed, expectedValidationStages(full)) {
			t.Fatal(completed, err)
		}
		for _, stage := range expectedValidationStages(full) {
			t.Run(stage+"-omitted-"+map[bool]string{true: "full", false: "smoke"}[full], func(t *testing.T) {
				missing := strings.ReplaceAll(output, "CLC_VALIDATION_STAGE_PASSED:"+stage+"\n", "")
				if _, err := validationEvidence(missing, full); err == nil {
					t.Fatal("omitted mandatory stage accepted", stage)
				}
			})
		}
	}
	fullOutput := successfulValidationOutput(true)
	for name, output := range map[string]string{
		"bare-marker":       "CLC_VALIDATION_PASSED\n",
		"substring-marker":  "error: CLC_VALIDATION_PASSED was never emitted\n",
		"smoke-for-full":    successfulValidationOutput(false),
		"wrong-mode":        strings.ReplaceAll(fullOutput, "COMPLETED:full", "COMPLETED:smoke"),
		"duplicate-stage":   "CLC_VALIDATION_STAGE_PASSED:docker-daemon\n" + fullOutput,
		"unexpected-stage":  "CLC_VALIDATION_STAGE_PASSED:optional\n" + fullOutput,
		"truncated":         strings.ReplaceAll(fullOutput, "CLC_VALIDATION_PASSED\n", ""),
		"post-pass-error":   fullOutput + "cleanup failed\n",
		"duplicate-success": fullOutput + "CLC_VALIDATION_PASSED\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validationEvidence(output, true); err == nil {
				t.Fatal("incomplete or inconsistent evidence accepted", output)
			}
		})
	}
}
