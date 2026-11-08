package hyperv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Prefer the guest's command/exit record, including the verifier's more specific
// record when the build harness subsequently reports its own failed invocation.
// Legacy builds have no record: preserve Bash's actual error in that case.
func provisioningFailure(output string) string {
	var records, shellErrors []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CLC_PROVISIONING_FAILED ") || strings.HasPrefix(line, "CLC_VERIFICATION_FAILED ") {
			records = append(records, line)
		} else if strings.Contains(line, ": unbound variable") || (strings.Contains(line, ".sh: line ") && (strings.Contains(line, "command not found") || strings.Contains(line, "Permission denied"))) {
			shellErrors = append(shellErrors, line)
		}
	}
	unique := []string{}
	seen := map[string]bool{}
	for _, record := range append(shellErrors, records...) {
		if !seen[record] && len(unique) < 4 {
			unique = append(unique, record)
			seen[record] = true
		}
	}
	result := strings.Join(unique, "; ")
	if len(result) > 4096 {
		result = result[:4096]
	}
	return result
}

// Evidence lives outside disposable VM directories. Capture even partial SSH
// output and propagate write/flush errors; cleanup must still run afterward.
func (c *Client) archiveBuild(ctx context.Context, r Record, log string) (string, error) {
	executor := &Client{Config: c.Config, Redactor: c.Redactor, Command: c.Command}
	b, captureErr := executor.ssh(ctx, r, nil, `for file in /var/log/clc-image-failure /var/log/clc-image-build.log /var/log/cloud-init-output.log /var/log/cloud-init.log /var/log/clc-image-packages.txt /home/runner/.clc/validation.log; do
  if sudo -n test -f "$file"; then
    limit=1048576
    case "$file" in
      /var/log/clc-image-build.log) limit=3145728 ;;
      /var/log/clc-image-packages.txt) limit=524288 ;;
      /var/log/clc-image-failure) limit=65536 ;;
    esac
    printf '\n--- %s ---\n' "$file"
    sudo -n tail -c "$limit" "$file" || exit 1
  fi
done
printf '\n--- cloud-init status ---\n'
sudo -n cloud-init status --long 2>&1 || true`)
	redacted := c.Redactor.Text(string(b))
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return provisioningFailure(redacted), errors.Join(captureErr, err)
	}
	_, writeErr := fmt.Fprintln(f, redacted)
	return provisioningFailure(redacted), errors.Join(captureErr, writeErr, f.Sync(), f.Close())
}

// Shared by PrepareImage, controller startup and offline cleanup. All callers
// hold the controller lock; auxiliary VMs can never contain a runner job.
func (c *Client) recoverAuxiliaryVM(r Record) error {
	logDir := filepath.Join(c.LogDir, "images")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	recoveryLog := filepath.Join(logDir, r.Name+"-recovery.log")
	c.progress("Recovering interrupted %s VM %s; diagnostics: %s", r.Purpose, r.Name, recoveryLog)
	captureCtx, captureCancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, captureErr := c.archiveBuild(captureCtx, r, recoveryLog)
	captureCancel()
	if captureErr != nil {
		// Already-off/missing VMs cannot answer SSH. Preserve partial evidence
		// and the capture error, then permit safe, owned-resource cleanup.
		message := "Interrupted guest diagnostic capture unavailable: " + c.Redactor.Text(captureErr.Error())
		f, openErr := os.OpenFile(recoveryLog, os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return errors.Join(captureErr, openErr)
		}
		_, writeErr := fmt.Fprintln(f, message)
		if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
			return err
		}
		c.progress("%s; retained at %s", message, recoveryLog)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	if err := c.destroy(cleanupCtx, r.Name); err != nil {
		return fmt.Errorf("recover interrupted image VM (diagnostics: %s): %w", recoveryLog, err)
	}
	return nil
}
