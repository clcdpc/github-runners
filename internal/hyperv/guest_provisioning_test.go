package hyperv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuestBuildHarnessWithoutLoginEnvironment(t *testing.T) {
	for _, scenario := range []string{"ready", "installation-failure", "verification-failure", "unbound-variable", "logger-failure"} {
		t.Run(scenario, func(t *testing.T) {
			bash := guestBash(t)
			dir := t.TempDir()
			for _, path := range []string{"logs", "runner/.clc", "opt"} {
				if err := os.MkdirAll(filepath.Join(dir, path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script, err := guest.ReadFile("guest/prepare.sh")
			if err != nil {
				t.Fatal(err)
			}
			// Remap only guest filesystem locations; traps, shell options, logger
			// draining, user switching and exit status handling execute unchanged.
			script = []byte(strings.NewReplacer("/var/log/", `"${FIXTURE_ROOT}/logs"/`, "/home/runner/", `"${FIXTURE_ROOT}/runner"/`, "/opt/clc/", `"${FIXTURE_ROOT}/opt"/`).Replace(string(script)))
			for path, content := range map[string]string{
				"prepare.sh":      string(script),
				"id":              "#!/usr/bin/env bash\necho 0\n",
				"fixture-install": "#!/usr/bin/env bash\nexit 37\n",
				"tee": `#!/usr/bin/env bash
if test "$FIXTURE_SCENARIO" = logger-failure && [[ "$1" == */clc-image-build.log ]]; then cat >/dev/null; exit 23; fi
exec /usr/bin/tee "$@"
`,
				"runuser": `#!/usr/bin/env bash
set -euo pipefail
test "$1 $2 $3" = '-u runner --'
shift 3
export FIXTURE_RUN_AS=runner
unset HOME USER LOGNAME
exec "$@"
`,
				"opt/provision.sh": `#!/usr/bin/env bash
set -euo pipefail
test "$HOME" = /root
test "$DEBIAN_FRONTEND" = noninteractive
test "$(umask)" = 0022
echo 'fixture root installation'
case "$FIXTURE_SCENARIO" in
installation-failure) fixture-install ;;
unbound-variable) echo "$CLC_FIXTURE_UNSET" ;;
esac
`,
				"opt/verify.sh": `#!/usr/bin/env bash
set -euo pipefail
test "$FIXTURE_RUN_AS" = runner
test "${HOME-unset}" = unset
test "$1" = smoke
if test "$FIXTURE_SCENARIO" = verification-failure; then
  echo 'CLC_VERIFICATION_FAILED exit=43 line=250 phase=Docker command=fixture-verify'
  exit 43
fi
echo CLC_VALIDATION_PASSED
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", `export FIXTURE_ROOT="$PWD" PATH="$PWD:$PATH"; unset HOME USER LOGNAME SUDO_USER; exec bash ./prepare.sh`)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "FIXTURE_SCENARIO="+scenario, "MSYS_NO_PATHCONV=1")
			cmd.WaitDelay = 2 * time.Second
			output, runErr := cmd.CombinedOutput()
			ready := scenario == "ready"
			if (runErr == nil) != ready {
				t.Fatal(scenario, runErr, string(output))
			}
			_, readyErr := os.Stat(filepath.Join(dir, "runner/.clc/build-ready"))
			_, failedErr := os.Stat(filepath.Join(dir, "runner/.clc/failed"))
			if (readyErr == nil) != ready || (failedErr == nil) == ready {
				t.Fatal("incorrect build lifecycle markers", scenario, readyErr, failedErr, string(output))
			}
			log, err := os.ReadFile(filepath.Join(dir, "logs/clc-image-build.log"))
			if scenario != "logger-failure" && (err != nil || !strings.Contains(string(log), "fixture root installation")) {
				t.Fatal("lost build transcript", err, string(log))
			}
			if ready {
				if !strings.Contains(string(log), "CLC_VALIDATION_PASSED") {
					t.Fatal("ready preceded drained verification transcript", string(log))
				}
				return
			}
			failure, err := os.ReadFile(filepath.Join(dir, "logs/clc-image-failure"))
			if err != nil || !strings.Contains(string(failure), "CLC_PROVISIONING_FAILED exit=") || !strings.Contains(string(failure), "command=") {
				t.Fatal("failed command/exit not retained", err, string(failure), string(output))
			}
			exit := "exit=1"
			if scenario == "installation-failure" {
				exit = "exit=37"
			}
			if scenario == "verification-failure" {
				exit = "exit=43"
			}
			if scenario == "logger-failure" {
				exit = "exit=23"
			}
			if !strings.Contains(string(failure), exit) {
				t.Fatal("original exit status lost", string(failure), string(output))
			}
			if scenario == "installation-failure" && !strings.Contains(string(failure), "command=fixture-install") {
				t.Fatal("actual failed command lost", string(failure))
			}
			if scenario == "verification-failure" && !strings.Contains(string(log), "CLC_VERIFICATION_FAILED exit=43") {
				t.Fatal("verification failure not drained", string(log))
			}
			if scenario == "unbound-variable" && !strings.Contains(string(log), "CLC_FIXTURE_UNSET: unbound variable") {
				t.Fatal("nounset shell error lost", string(log))
			}
		})
	}
}

func TestGuestVerificationRejectsInvalidEffectiveHome(t *testing.T) {
	for _, scenario := range []string{"missing-account", "uid-mismatch", "relative-home", "empty-home", "missing-home"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			for name, source := range map[string]string{
				"id": "#!/usr/bin/env bash\necho 1000\n",
				"getent": `#!/usr/bin/env bash
test "$1 $2" = 'passwd 1000' || exit 9
uid=1000
home="$PWD/missing"
case "$FIXTURE_SCENARIO" in
missing-account) exit 2 ;;
uid-mismatch) uid=1001 ;;
relative-home) home=relative ;;
empty-home) home='' ;;
esac
printf 'runner:x:%s:1000::%s:/bin/bash\n' "$uid" "$home"
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script, err := guest.ReadFile("guest/verify.sh")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, guestBash(t), "--noprofile", "--norc", "-c", `export PATH="$PWD:$PATH" HOME="$PWD/inherited" SUDO_USER=root; exec bash -s -- smoke`)
			cmd.Dir, cmd.Stdin = dir, strings.NewReader(string(script))
			cmd.Env = append(os.Environ(), "FIXTURE_SCENARIO="+scenario)
			output, runErr := cmd.CombinedOutput()
			if runErr == nil || !strings.Contains(string(output), "account") || strings.Contains(string(output), "CLC_VALIDATION_PASSED") {
				t.Fatal("invalid effective account was accepted", scenario, runErr, string(output))
			}
			if _, err := os.Stat(filepath.Join(dir, "inherited")); !os.IsNotExist(err) {
				t.Fatal("invalid account fell back to inherited HOME", err)
			}
		})
	}
}
