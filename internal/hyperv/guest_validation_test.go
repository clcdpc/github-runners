package hyperv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Execute the REAL embedded script with fake external tools, not a rewritten
// readiness algorithm. Coreutils timeout/sleep and Bash's wall clock are real.
// This runs without Docker, Linux guests, network access or Hyper-V privileges.
func guestBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		for _, dir := range []string{os.Getenv("ProgramFiles"), `C:\Program Files`} {
			path := filepath.Join(dir, "Git", "bin", "bash.exe")
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
		t.Skip("guest script regression tests require Git Bash (or Bash on Linux); no WSL/Hyper-V needed")
	}
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash unavailable for guest script regression tests")
	}
	return path
}

func TestGuestValidation(t *testing.T) {
	// Git Bash process creation is substantially slower than Linux, especially
	// under coverage/antivirus. Keep the real-clock timeout tests independent of
	// machine-wide process storms instead of lengthening production deadlines.
	slots := make(chan struct{}, 4)
	for _, scenario := range []string{"ready", "delayed", "startup-exit", "exit-after-query", "exit-during-host", "wrong-port", "wrong-sql-result", "host-timeout", "inside-timeout", "hung-query", "low-memory", "compose-unhealthy", "dotnet-test-failure", "publish-failure", "package-failure", "browser-dependency-failure", "browser-failure", "sql-exit-after-browser", "cleanup-failure", "early-exit", "omitted-sql", "omitted-browser", "smoke", "diagnostics", "home-unset-root", "home-unset-runner", "home-wrong-runner", "home-unset-full"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			bash := guestBash(t)
			dir := t.TempDir()
			write := func(name, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script, err := guest.ReadFile("guest/verify.sh")
			if err != nil {
				t.Fatal(err)
			}
			script = []byte(strings.ReplaceAll(string(script), "\r\n", "\n"))
			switch scenario {
			case "early-exit":
				script = []byte(strings.Replace(string(script), "stage_passed compose\n", "stage_passed compose\nexit 0\n", 1))
			case "omitted-sql":
				script = []byte(strings.Replace(string(script), "if test \"$mode\" = full; then\n  phase='SQL resource preflight'", "if false; then\n  phase='SQL resource preflight'", 1))
			case "omitted-browser":
				script = []byte(strings.Replace(string(script), "  stage_passed chromium-browser\n", "", 1))
			}
			write("verify.sh", string(script))
			// Only mock filesystem prerequisites belonging to the guest. All other
			// test conditions, traps, pipelines and retries execute unchanged.
			write("env.sh", `test() {
  case "$*" in '-w /usr/share/dotnet'|'-x /home/runner/bin/Runner.Listener') return 0 ;; esac
  builtin test "$@"
}
`)
			write("id", "#!/usr/bin/env bash\necho \"$FIXTURE_UID\"\n")
			for _, name := range []string{"docker", "dotnet", "node", "npm", "npx", "pwsh", "python3", "git", "getent", "curl", "openssl", "awk", "free", "df", "ss", "sudo"} {
				write(name, guestToolFixture)
			}
			mode := "full"
			if scenario == "smoke" {
				mode = "smoke"
			}
			if strings.HasPrefix(scenario, "home-") && scenario != "home-unset-full" {
				mode = "smoke"
			}
			priorTranscript := ""
			if scenario == "diagnostics" {
				mode = "diagnostics"
				priorTranscript = "prior guest failure evidence\n"
				if err := os.MkdirAll(filepath.Join(dir, "home", ".clc"), 0700); err != nil {
					t.Fatal(err)
				}
				write("home/.clc/validation.log", priorTranscript)
				write("credential", "Clc"+strings.Repeat("a", 48)+"Aa1!")
			}
			// Set paths inside Bash so Git's MSYS path translation matches Linux.
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			// Match production's SSH transport. Running a script file conceals
			// interactive children consuming the rest of a bash -s script.
			cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", `export FIXTURE_ROOT="$PWD" FIXTURE_ACCOUNT_HOME="$PWD/$FIXTURE_HOME_DIRECTORY" BASH_ENV="$PWD/env.sh" PATH="$PWD:$PATH"; mkdir -p "$FIXTURE_ACCOUNT_HOME"; export HOME="$FIXTURE_ACCOUNT_HOME"; case "$FIXTURE_SCENARIO" in home-unset-*) unset HOME ;; home-wrong-*) export HOME="$PWD/foreign-home" SUDO_USER=root ;; esac; exec bash -s -- "$FIXTURE_MODE"`)
			cmd.Stdin = strings.NewReader(strings.ReplaceAll(string(script), "\r\n", "\n"))
			cmd.Dir = dir
			sqlBudget := "6"
			if scenario == "host-timeout" || scenario == "delayed" {
				sqlBudget = "12"
			}
			uid := "1000"
			homeDirectory := "home"
			if scenario == "home-unset-root" {
				uid = "0"
				homeDirectory = "root-home"
			}
			cmd.Env = append(os.Environ(), "FIXTURE_UID="+uid, "FIXTURE_HOME_DIRECTORY="+homeDirectory, "FIXTURE_SCENARIO="+scenario, "FIXTURE_MODE="+mode, "CLC_SQL_READY_TIMEOUT_SECONDS="+sqlBudget, "CLC_SERVICE_READY_TIMEOUT_SECONDS=12", "MSYS_NO_PATHCONV=1")
			cmd.WaitDelay = 3 * time.Second
			start := time.Now()
			b, runErr := cmd.CombinedOutput()
			output := string(b)
			read := func(name string) string {
				t.Helper()
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("%s: %v\n%s", name, err, output)
				}
				return string(b)
			}
			calls := read("calls")
			success := scenario == "ready" || scenario == "delayed" || scenario == "smoke" || scenario == "diagnostics" || strings.HasPrefix(scenario, "home-")
			if (runErr == nil) != success || strings.Contains(output, "CLC_VALIDATION_PASSED") != (success && mode != "diagnostics") {
				t.Fatalf("scenario=%s exit=%v\n%s\nCalls:\n%s", scenario, runErr, output, calls)
			}
			transcript := read(homeDirectory + "/.clc/validation.log")
			if _, err := os.Stat(filepath.Join(dir, "foreign-home")); !os.IsNotExist(err) {
				t.Fatal("verification used inherited HOME/SUDO_USER instead of effective passwd home", err)
			}
			if strings.TrimSpace(transcript) != strings.TrimSpace(priorTranscript+output) {
				t.Fatalf("guest transcript was not drained/preserved\ntranscript: %s\noutput: %s", transcript, output)
			}
			for _, secret := range []string{"Clc" + strings.Repeat("a", 48) + "Aa1!", "ghp_FIXTURE_CREDENTIAL", "diagnostic-secret", "access-secret", "auth-secret"} {
				if strings.Contains(output+calls+transcript, secret) {
					t.Fatalf("credential leaked in %s", scenario)
				}
			}
			if strings.Contains(calls, "-P ") || strings.Contains(calls, "MSSQL_SA_PASSWORD=") {
				t.Fatal("credential passed on command line", calls)
			}
			if success && mode != "diagnostics" {
				stages, err := validationEvidence(output, mode == "full")
				if err != nil || strings.Join(stages, " ") != strings.Join(expectedValidationStages(mode == "full"), " ") {
					t.Fatal("guest did not prove the full mandatory sequence", stages, err, output)
				}
				if read("compose-stdin") != "" || !strings.Contains(calls, "exec -T --interactive=false") {
					t.Fatal("Compose can consume the streamed validation script", calls)
				}
				for _, check := range []string{"docker info", "docker compose version", "docker buildx version", "dotnet --list-sdks", "node --version", "npm --version", "pwsh -NoLogo", "python3 --version", "git --version", "getent hosts github.com", "curl -fsSI", "docker run --rm hello-world", "docker build -t clc-probe-image", "docker exec clc-probe grep -qx built /proof", "up -d --wait --wait-timeout 30", "exec -T --interactive=false test grep -qx built /proof"} {
					if !strings.Contains(calls, check) {
						t.Fatal("stage marker without execution of mandatory check", check, calls)
					}
				}
			}
			if !success || mode == "diagnostics" {
				for _, evidence := range []string{"Diagnostics: docker ps -a", "exit state and resources", "Docker logs", "Guest memory", "Guest disk", "Docker daemon journal", "OOM diagnostics"} {
					if !strings.Contains(output, evidence) {
						t.Fatalf("missing %q\n%s", evidence, output)
					}
				}
				if scenario != "cleanup-failure" && mode != "diagnostics" && strings.Index(calls, "docker ps -a") > strings.Index(calls, "docker rm -f") {
					t.Fatal("containers removed before diagnostic capture", calls)
				}
			}
			if strings.Contains(calls, "docker pull") && scenario != "cleanup-failure" && !success || mode == "diagnostics" {
				for _, evidence := range []string{"SQL published ports", "SQL resource usage", "SQL Server startup errorlog", "SQL startup fixture errorlog"} {
					if !strings.Contains(output, evidence) {
						t.Fatalf("missing SQL evidence %q\n%s", evidence, output)
					}
				}
			}
			switch scenario {
			case "diagnostics":
				if strings.Contains(calls, "docker rm") || strings.Contains(calls, "docker run") {
					t.Fatal("diagnostic recovery changed containers before host cleanup", calls)
				}
			case "delayed":
				if !strings.Contains(output, "SQL inside container ready after 2 attempt(s)") || !strings.Contains(output, "SQL Ubuntu localhost:1433 (127.0.0.1) ready after 2 attempt(s)") || !strings.Contains(output, "Docker daemon ready after 2 attempt(s)") || !strings.Contains(output, "Docker exec proof ready after 2 attempt(s)") {
					t.Fatal("delayed components were not retried", output)
				}
			case "startup-exit", "exit-after-query", "exit-during-host":
				if !strings.Contains(output, "exited or failed startup") || strings.Contains(output, "SQL inside container timed out") {
					t.Fatal("container exit confused with slow initialization", output)
				}
			case "wrong-port":
				if !strings.Contains(output, "Incorrect SQL networking") || strings.Contains(calls, "probe-host") {
					t.Fatal("wrong mapping accepted", output)
				}
			case "host-timeout":
				if strings.Count(calls, "probe-host") < 2 || !strings.Contains(output, "Internal SQL connectivity passed, but Ubuntu localhost:1433 failed") || !strings.Contains(output, "TCP fixture refused") {
					t.Fatal("network failure was not retried/diagnosed", output)
				}
			case "inside-timeout", "hung-query":
				if !strings.Contains(output, "SQL inside container timed out after 6s") || time.Since(start) > 35*time.Second {
					t.Fatal("query timeout was unbounded or undiagnosed", time.Since(start), output)
				}
			case "low-memory":
				if !strings.Contains(output, "requires at least 4 GiB") || strings.Contains(calls, "docker pull") {
					t.Fatal("insufficient memory was not rejected before SQL startup", output)
				}
			case "compose-unhealthy":
				if !strings.Contains(calls, "--wait --wait-timeout 30") || strings.Contains(calls, "docker pull") {
					t.Fatal("unhealthy Compose workload accepted", calls)
				}
			case "early-exit", "omitted-sql":
				if !strings.Contains(output, "Mandatory validation stage missing: sql-query-container") || !strings.Contains(output, "Mandatory validation stage missing: chromium-browser") {
					t.Fatal("early EOF/exit or skipped full block was not rejected", output)
				}
			case "wrong-sql-result":
				if !strings.Contains(output, "unexpected SQL result") {
					t.Fatal("wrong SQL result accepted", output)
				}
			}
			if success && mode == "full" || scenario == "browser-failure" || scenario == "cleanup-failure" {
				for _, step := range []string{"docker pull mcr.microsoft.com/mssql/server:2022-latest", "probe-inside", "probe-host", "ProductMajorVersion", "python3 -c", "dotnet run", "dotnet test", "dotnet publish", "zipfile.ZipFile", "npx playwright install chromium", "node probe.cjs dependencies", "node probe.cjs browser"} {
					if !strings.Contains(calls, step) {
						t.Fatalf("validation did not continue through %s\n%s", step, calls)
					}
				}
			} else if mode != "full" && strings.Contains(calls, "npx playwright") {
				t.Fatal("continued past a failed prerequisite", calls)
			}
			if mode != "diagnostics" && (!strings.Contains(calls, "docker image rm") || strings.Contains(calls, "docker pull") && !strings.Contains(calls, "docker rm -f clc-sql")) {
				t.Fatal("owned workload cleanup was skipped", calls)
			}
		})
	}
}

func TestEmbeddedGuestShellSyntax(t *testing.T) {
	bash := guestBash(t)
	entries, err := guest.ReadDir("guest")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sh") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			b, err := guest.ReadFile("guest/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bash, "-n")
			cmd.Stdin = strings.NewReader(string(b))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(err, string(output))
			}
		})
	}
}

const guestToolFixture = `#!/usr/bin/env bash
set -euo pipefail
tool=${0##*/}
printf '%s %s\n' "$tool" "$*" >> "$FIXTURE_ROOT/calls"
scenario=$FIXTURE_SCENARIO
count() {
  local path="$FIXTURE_ROOT/$1" n=0
  if test -f "$path"; then n=$(cat "$path"); fi
  n=$((n+1)); echo "$n" > "$path"; echo "$n"
}
case "$tool" in
openssl) printf '%048d\n' 0 | tr 0 a ;;
awk) echo 12582912 ;;
getent) if test "$1" = passwd; then test "$2" = "$FIXTURE_UID"; printf 'fixture:x:%s:1000::%s:/bin/bash\n' "$FIXTURE_UID" "$FIXTURE_ACCOUNT_HOME"; else echo '127.0.0.1 localhost'; fi ;;
free|df|ss|sudo) echo 'fixture resource/startup diagnostics' ;;
curl|pwsh|npm|npx|git) ;;
python3) if test "$scenario" = package-failure && [[ "$*" == *zipfile* ]]; then echo 'fixture corrupt package' >&2; exit 1; fi ;;
node)
  if test "$1" = --version; then echo v24.22.2
  else
    cp "$1" "$FIXTURE_ROOT/browser-probe.cjs"
    if test "$2" = dependencies && test "$scenario" = browser-dependency-failure; then echo 'libfixture.so => not found' >&2; exit 1; fi
    if test "$2" = browser && test "$scenario" = browser-failure; then echo 'fixture Chromium launch failed' >&2; exit 1; fi
    if test "$2" = browser && test "$scenario" = sql-exit-after-browser; then touch "$FIXTURE_ROOT/server-exited"; fi
  fi ;;
dotnet)
  test "$HOME" = "$FIXTURE_ACCOUNT_HOME"
  case "$1" in
  --list-sdks) echo '10.0.113 [/usr/share/dotnet/sdk]' ;;
  new) while test "$1" != -o; do shift; done; mkdir -p "$2" ;;
  publish) if test "$scenario" = publish-failure; then echo 'fixture publish failed' >&2; exit 1; fi; while test "$1" != --output; do shift; done; mkdir -p "$2"; touch "$2/sql-probe.exe" ;;
  run) test -n "$CLC_SQL_PASSWORD" ;;
  test) test -n "$CLC_SQL_PASSWORD"; cp "${2%/*}/SqlTests.cs" "$FIXTURE_ROOT/SqlTests.cs"; if test "$scenario" = dotnet-test-failure; then echo 'fixture SQL integration test failed' >&2; exit 1; fi ;;
  esac ;;
docker)
  case "$1" in
  info)
    if test "$#" -gt 1; then
      if test "$scenario" = low-memory; then echo 2147483648; else echo 17179869184; fi
    elif test "$scenario" = delayed && test "$(count daemon)" -eq 1; then exit 1
    else echo 'fixture daemon ready'; fi ;;
  build) cp "${@: -1}/Dockerfile" "$FIXTURE_ROOT/dockerfile" ;;
  run)
    if [[ " $* " == *' --name clc-sql '* ]]; then
      test -n "$MSSQL_SA_PASSWORD"
      test "${#MSSQL_SA_PASSWORD}" -ge 8
      [[ "$MSSQL_SA_PASSWORD" == *[A-Z]* && "$MSSQL_SA_PASSWORD" == *[a-z]* && "$MSSQL_SA_PASSWORD" == *[0-9]* && "$MSSQL_SA_PASSWORD" == *'!'* ]]
      printf '%s' "$MSSQL_SA_PASSWORD" > "$FIXTURE_ROOT/credential"
      [[ " $* " == *' --memory 4g --memory-swap 4g '* && " $* " == *' -e MSSQL_MEMORY_LIMIT_MB=3072 '* && " $* " == *' -p 127.0.0.1:1433:1433/tcp '* ]]
    fi
    if [[ " $* " == *' --name clc-sql-client '* ]]; then
      [[ " $* " == *' --network host '* ]]
    fi
    echo 'fixture container created' ;;
  inspect)
    if [[ "$*" == *'.State.Status}}|'* ]]; then
      container=${@: -1}
      if test "$container" = clc-sql && { test "$scenario" = startup-exit || test -f "$FIXTURE_ROOT/server-exited"; }; then
        echo 'exited|false|false|137|true|fixture process exited'
      else echo 'running|true|false|0|false|'; fi
    else echo 'State={"Status":"exited","ExitCode":137,"OOMKilled":true} Memory=4294967296 Ports=fixture'; fi ;;
  exec)
    if [[ " $* " == *' clc-probe '* ]]; then
      if test "$scenario" = delayed && test "$(count exec)" -eq 1; then exit 1; fi
      exit 0
    fi
    [[ " $* " == *' -e SQLCMDPASSWORD '* && " $* " == *' -S tcp:127.0.0.1,1433 '* && " $* " == *' -b -l 3 -t 5 '* ]]
    test "$SQLCMDPASSWORD" = "$(cat "$FIXTURE_ROOT/credential")"
    if [[ " $* " == *' clc-sql-client '* ]]; then side=host; else side=inside; fi
    echo "probe-$side" >> "$FIXTURE_ROOT/calls"
    n=$(count "$side")
    if test "$scenario" = exit-after-query; then touch "$FIXTURE_ROOT/server-exited"; fi
    if test "$scenario" = exit-during-host && test "$side" = host; then touch "$FIXTURE_ROOT/server-exited"; exit 1; fi
    if test "$scenario" = hung-query && test "$side" = inside; then sleep 30; fi
    if { test "$scenario" = delayed && test "$n" -eq 1; } || { test "$scenario" = inside-timeout && test "$side" = inside; } || { test "$scenario" = host-timeout && test "$side" = host; }; then
      echo 'TCP fixture refused' >&2; exit 1
    fi
    if test "$scenario" = wrong-sql-result; then echo 41; else echo 42; fi ;;
  port) if test "$scenario" = wrong-port; then echo '127.0.0.1:15433'; else echo '127.0.0.1:1433'; fi ;;
  logs)
    echo 'SQL Docker log fixture'
    if test -f "$FIXTURE_ROOT/credential"; then cat "$FIXTURE_ROOT/credential"; echo; fi
    echo 'token=diagnostic-secret'
    echo 'ACCESS_TOKEN=access-secret'
    echo 'AUTHORIZATION: Bearer auth-secret'
    echo 'ghp_FIXTURE_CREDENTIAL' ;;
  cp) echo 'SQL startup fixture errorlog' > "${@: -1}" ;;
  compose)
    if [[ " $* " == *' exec '* ]]; then
      # Compose exec is interactive by default even with -T. Drain stdin
      # like its client-side forwarding goroutine, including unread script.
      cat > "$FIXTURE_ROOT/compose-stdin"
    fi
    if [[ " $* " == *' up '* ]]; then
      [[ " $* " == *' --wait --wait-timeout 30 '* ]]
      if test "$scenario" = compose-unhealthy; then echo 'fixture unhealthy service'; exit 1; fi
    fi ;;
  rm) if test "$scenario" = cleanup-failure && test "${@: -1}" = clc-sql; then echo 'fixture container removal failed' >&2; exit 1; fi ;;
  esac ;;
esac
`
