package restart

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
)

func HelperArguments(m Manifest, request string, check bool) []string {
	args := []string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-File", filepath.Join(m.Bundle, "scripts", "Restart-Controller.ps1"), "-RequestPath", request}
	if check {
		args = append(args, "-Check")
	}
	return args
}

func Preflight(ctx context.Context, powershell string, m Manifest, request string) error {
	cmd := exec.CommandContext(ctx, powershell, HelperArguments(m, request, true)...)
	hide(cmd)
	output, err := cmd.Output()
	if err != nil {
		return errors.New("Restart validation failed; inspect restart-check.json and run Activate or TestUnattended as indicated")
	}
	var result struct {
		Ready   bool   `json:"ready"`
		Blocker string `json:"blocker"`
	}
	if json.Unmarshal(output, &result) != nil {
		return errors.New("restart preflight returned invalid diagnostics")
	}
	if !result.Ready {
		return errors.New(result.Blocker)
	}
	return nil
}
