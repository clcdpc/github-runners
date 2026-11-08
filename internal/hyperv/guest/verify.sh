#!/usr/bin/env bash
# Sent over pinned SSH stdin too: validate immutable images with current checks.
set +x
set -Eeuo pipefail
umask 077
mode=${1:-smoke}
case "$mode" in smoke|full|diagnostics) ;; *) echo 'Expected smoke, full or diagnostics validation' >&2; exit 2 ;; esac
readonly mode
# cloud-final is a system service, not a login shell. HOME may be absent (or
# inherited from sudo). Resolve the effective account, never SUDO_USER or /home
# guesses: root's build checks and runner's SSH checks need separate caches/logs.
validation_uid=$(id -u)
if ! validation_account=$(getent passwd "$validation_uid"); then
  echo "Cannot resolve the effective validation account (uid $validation_uid)" >&2; exit 2
fi
IFS=: read -r validation_user _ validation_account_uid _ _ validation_home _ <<< "$validation_account"
if test "$validation_account_uid" != "$validation_uid" || test -z "$validation_user"; then
  echo 'Cannot resolve the effective validation account' >&2; exit 2
fi
case "$validation_home" in /*) ;; *) echo 'Validation account has no absolute home directory' >&2; exit 2 ;; esac
if ! test -d "$validation_home"; then
  echo "Validation account home is unavailable: $validation_home" >&2; exit 2
fi
export HOME="$validation_home"
readonly validation_home
# Declare the complete contract BEFORE any child can consume streamed script
# input. EXIT must fail closed even if bash reaches an unexpected early EOF.
required_stages=(docker-daemon docker-compose docker-buildx dotnet-sdk node npm powershell python git runner github-dns github-https docker-run docker-build docker-exec compose)
if test "$mode" = full; then
  required_stages+=(sql-resources sql-server sql-query-container sql-port sql-query-host sql-localhost-socket dotnet-sql dotnet-tests windows-publish windows-package chromium-dependencies chromium-browser sql-still-running)
fi
readonly -a required_stages
declare -A completed_stages=()
sequence_completed=false
work=''
phase=initialization
password=''
probe_created=false
sql_created=false
client_created=false
compose_created=false
image_created=false
progress() { printf '[TestHyperV +%ss] %s\n' "$SECONDS" "$*"; }
stage_passed() {
  local stage=$1
  if test "${required_stages[${#completed_stages[@]}]:-}" != "$stage"; then
    progress "Unexpected or out-of-order validation stage: $stage"; return 1
  fi
  completed_stages[$stage]=true
  printf 'CLC_VALIDATION_STAGE_PASSED:%s\n' "$stage"
}

# Inspect only selected non-secret fields. Filter the ENTIRE transcript,
# including unexpected errors from tools, diagnostics and cleanup.
redact_output() {
  local line
  while IFS= read -r line || test -n "$line"; do
    if test -n "$password"; then line=${line//"$password"/[REDACTED]}; fi
    printf '%s\n' "$line"
  done | sed -u -E \
    -e 's/((MSSQL_SA_PASSWORD|SQLCMDPASSWORD|CLC_SQL_PASSWORD|password|authorization|access_?token|refresh_?token|token|client_?secret|encodedJITConfig)[" ]*[:=][ ]*)[^,;]*/\1[REDACTED]/gI' \
    -e 's/(github_pat_|gh[pousr]_)[A-Za-z0-9_]+/[REDACTED]/g' \
    -e 's/Clc[a-f0-9]{48}Aa1!/[REDACTED]/g' \
    -e 's/eyJ[A-Za-z0-9_.-]+/[REDACTED]/g'
}

bounded() {
  local label=$1 limit=$2 result
  shift 2
  phase=$label
  progress "$label (limit ${limit}s)"
  # bash -s owns stdin. In particular Compose exec -T still forwards stdin
  # unless interactive mode is explicitly disabled. No probe needs input.
  if timeout --signal=TERM --kill-after=2 "${limit}s" "$@" < /dev/null; then
    progress "$label passed"
  else
    result=$?
    progress "$label failed (exit $result)"
    return "$result"
  fi
}

diagnostic() {
  local label=$1 result
  shift
  progress "Diagnostics: $label"
  if timeout --signal=TERM --kill-after=2 5s "$@" < /dev/null; then return; else result=$?; fi
  progress "Diagnostic unavailable: $label (exit $result)"
}

diagnostics() {
  progress "Failure in $phase; collecting diagnostics BEFORE removing containers"
  diagnostic 'docker ps -a' docker ps -a --no-trunc
  diagnostic 'Docker resources' docker info --format 'MemoryBytes={{.MemTotal}} CPUs={{.NCPU}} OS={{.OperatingSystem}}'
  diagnostic 'Guest memory' free -m
  diagnostic 'Guest disk' df -h
  diagnostic 'Guest CPU' getconf _NPROCESSORS_ONLN
  diagnostic 'Guest cgroup memory' sh -c 'for f in /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.events; do if test -r "$f"; then echo "$f"; cat "$f"; fi; done'
  diagnostic 'localhost resolution' getent ahosts localhost
  diagnostic 'TCP listeners' ss -lnt
  if test -f "$work/readiness.log"; then diagnostic 'Last readiness probe output' tail -c 16384 "$work/readiness.log"; fi
  local container
  for container in clc-probe clc-sql clc-sql-client; do
    diagnostic "$container exit state and resources" docker inspect --format 'State={{json .State}} Memory={{.HostConfig.Memory}} MemorySwap={{.HostConfig.MemorySwap}} NanoCPUs={{.HostConfig.NanoCpus}} NetworkMode={{.HostConfig.NetworkMode}} Ports={{json .NetworkSettings.Ports}} Bindings={{json .HostConfig.PortBindings}} Image={{.Image}}' "$container"
    diagnostic "$container Docker logs" docker logs --timestamps --tail 150 "$container"
  done
  if test "$sql_created" = true || test "$mode" = diagnostics; then
    diagnostic 'SQL published ports' docker port clc-sql
    diagnostic 'SQL resource usage' docker stats --no-stream clc-sql
    # docker cp works after process exit; docker exec does not.
    if timeout --signal=TERM --kill-after=2 5s docker cp clc-sql:/var/opt/mssql/log/errorlog "$work/sql-errorlog" < /dev/null; then
      diagnostic 'SQL Server startup errorlog' tail -c 262144 "$work/sql-errorlog"
    else
      progress 'SQL Server startup errorlog unavailable (startup may have failed before creating it)'
    fi
  fi
  if test "$compose_created" = true; then
    diagnostic 'Compose state' docker compose -f "$work/compose.yml" ps -a
    diagnostic 'Compose logs' docker compose -f "$work/compose.yml" logs --no-color --tail 100
  fi
  diagnostic 'Docker daemon journal' sudo -n journalctl -u docker.service --no-pager -n 100
  diagnostic 'Kernel startup / OOM diagnostics' sudo -n journalctl -k --no-pager -n 100
}

finish() {
  local result=$? cleanup_result=0 logger_result=0 stage
  trap - EXIT HUP INT TERM
  set +e
  if test "$mode" != diagnostics; then
    for stage in "${required_stages[@]}"; do
      if test "${completed_stages[$stage]:-false}" != true; then
        progress "Mandatory validation stage missing: $stage"; if test "$result" -eq 0; then result=1; fi
      fi
    done
    if test "$sequence_completed" != true; then progress 'Validation sequence did not reach its end'; if test "$result" -eq 0; then result=1; fi; fi
  fi
  if test "$result" -ne 0; then diagnostics; fi
  progress 'Cleaning disposable validation workloads'
  if test "$client_created" = true; then bounded 'Remove SQL client' 15 docker rm -f clc-sql-client || cleanup_result=1; fi
  if test "$sql_created" = true; then bounded 'Remove SQL Server' 15 docker rm -f clc-sql || cleanup_result=1; fi
  if test "$probe_created" = true; then bounded 'Remove Docker probe' 15 docker rm -f clc-probe || cleanup_result=1; fi
  if test "$compose_created" = true; then bounded 'Remove Compose workload' 20 docker compose -f "$work/compose.yml" down --timeout 5 || cleanup_result=1; fi
  if test "$image_created" = true; then bounded 'Remove probe image' 15 docker image rm clc-probe-image || cleanup_result=1; fi
  if test "$cleanup_result" -ne 0; then progress 'Validation cleanup failed'; diagnostics; result=1; fi
  # Keep the working directory available for diagnostic copies until all
  # container cleanup has finished. Remove only our exact mktemp allocation.
  case "$work" in '') ;; */clc-validate.*) rm -rf -- "$work" || result=1 ;; *) result=1 ;; esac
  # Drain redaction/tee before SSH returns and before destroying the VM.
  exec 1>&3 2>&3
  wait "$logger_pid" || logger_result=1
  if test "$logger_result" -ne 0; then echo 'Validation transcript capture failed' >&2; result=1; fi
  # Success is the final output, after checks, cleanup AND transcript capture.
  if test "$result" -eq 0 && test "$mode" != diagnostics; then
    if ! printf 'CLC_VALIDATION_COMPLETED:%s\nCLC_VALIDATION_PASSED\n' "$mode" | tee -a "$validation_home/.clc/validation.log"; then result=1; fi
  fi
  exit "$result"
}

if test "$mode" = full; then password="Clc$(openssl rand -hex 24 < /dev/null)Aa1!"; fi
mkdir -p "$validation_home/.clc"
if test "$mode" != diagnostics; then : > "$validation_home/.clc/validation.log"; fi
exec 3>&1
exec > >(redact_output | tee -a "$validation_home/.clc/validation.log" >&3) 2>&1
logger_pid=$!
trap finish EXIT
trap 'validation_result=$?; printf "CLC_VERIFICATION_FAILED exit=%s line=%s phase=%s command=%s\n" "$validation_result" "$LINENO" "$phase" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
work=$(mktemp -d -t clc-validate.XXXXXXXX)
if test "$mode" = diagnostics; then
  phase='interrupted validation recovery'
  diagnostics
  exit 0
fi

# Budgets include execution and sleeps. Every attempt also has an OS timeout.
sql_timeout=${CLC_SQL_READY_TIMEOUT_SECONDS:-180}
service_timeout=${CLC_SERVICE_READY_TIMEOUT_SECONDS:-30}
for setting in "$sql_timeout" "$service_timeout"; do
  if ! [[ "$setting" =~ ^[1-9][0-9]{0,2}$ ]] || test "$setting" -gt 180; then
    progress 'Readiness budgets must be between 1 and 180 seconds'; exit 2
  fi
done

container_running() {
  local container=$1 limit=$2 state
  if ! state=$(timeout --signal=TERM --kill-after=2 "${limit}s" docker inspect --format '{{.State.Status}}|{{.State.Running}}|{{.State.Restarting}}|{{.State.ExitCode}}|{{.State.OOMKilled}}|{{.State.Error}}' "$container" < /dev/null 2>&1); then
    progress "Cannot inspect $container: $state"; return 1
  fi
  case "$state" in running\|true\|false\|0\|false\|*) return 0 ;; esac
  progress "$container exited or failed startup: status|running|restarting|exitCode|OOMKilled|error=$state"
  return 1
}

wait_ready() {
  local label=$1 container=$2 budget=$3 deadline remaining limit attempt=0 result
  shift 3
  phase=$label
  deadline=$((SECONDS + budget))
  while test "$SECONDS" -lt "$deadline"; do
    remaining=$((deadline - SECONDS)); limit=$((remaining < 10 ? remaining : 10))
    if test -n "$container"; then container_running "$container" "$limit" || return 1; fi
    if test "$container" = clc-sql-client; then container_running clc-sql "$limit" || return 1; fi
    remaining=$((deadline - SECONDS))
    if test "$remaining" -le 0; then break; fi
    limit=$((remaining < 10 ? remaining : 10))
    attempt=$((attempt + 1))
    progress "$label: attempt $attempt, ${remaining}s remaining"
    if timeout --signal=TERM --kill-after=2 "${limit}s" "$@" < /dev/null > "$work/readiness.log" 2>&1; then
      # A query followed by process exit is not a ready service.
      if test -n "$container"; then container_running "$container" 2 || return 1; fi
      if test "$container" = clc-sql-client; then container_running clc-sql 2 || return 1; fi
      if test "$container" = clc-sql || test "$container" = clc-sql-client; then
        cat "$work/readiness.log"
        if ! grep -Eq '^[[:space:]]*42[[:space:]]*$' "$work/readiness.log"; then
          progress "$label returned an unexpected SQL result (expected 42)"; return 1
        fi
      fi
      progress "$label ready after $attempt attempt(s)"
      return 0
    else result=$?; fi
    if test -n "$container"; then container_running "$container" 2 || return 1; fi
    if test "$container" = clc-sql-client; then container_running clc-sql 2 || return 1; fi
    progress "$label not ready (exit $result)"
    remaining=$((deadline - SECONDS))
    if test "$remaining" -gt 0; then sleep "$((remaining < 2 ? remaining : 2))"; fi
  done
  progress "$label timed out after ${budget}s ($attempt attempts); last probe output:"
  if test -f "$work/readiness.log"; then tail -c 16384 "$work/readiness.log"; fi
  if test "$container" = clc-sql-client; then
    progress 'Internal SQL connectivity passed, but Ubuntu localhost:1433 failed; inspect port publication, loopback resolution and Docker firewall/NAT diagnostics'
  fi
  return 1
}

wait_ready 'Docker daemon' '' "$service_timeout" docker info
stage_passed docker-daemon
bounded 'Docker Compose version' 10 docker compose version
stage_passed docker-compose
bounded 'Docker Buildx version' 10 docker buildx version
stage_passed docker-buildx
phase='.NET SDK'
bounded "$phase" 15 dotnet --list-sdks | tee "$work/sdks"
grep '^10\.' "$work/sdks"
test -w /usr/share/dotnet
stage_passed dotnet-sdk
phase='Node version'
bounded "$phase" 10 node --version | tee "$work/node-version"
grep '^v24\.' "$work/node-version"
stage_passed node
bounded 'npm version' 10 npm --version
stage_passed npm
bounded 'PowerShell version' 15 pwsh -NoLogo -NoProfile -Command 'if ($PSVersionTable.PSVersion.Major -ne 7) { exit 1 }'
stage_passed powershell
bounded 'Python version' 10 python3 --version
stage_passed python
bounded 'Git version' 10 git --version
stage_passed git
test -x /home/runner/bin/Runner.Listener
stage_passed runner
bounded 'GitHub DNS' 15 getent hosts github.com
stage_passed github-dns
bounded 'GitHub HTTPS' 60 curl -fsSI --connect-timeout 5 --max-time 10 --retry 3 https://api.github.com
stage_passed github-https
bounded 'Docker hello-world pull/run' 300 docker run --rm hello-world
stage_passed docker-run
printf 'FROM alpine:3.23.2\nRUN echo built > /proof\nCMD ["sleep", "infinity"]\n' > "$work/Dockerfile"
image_created=true
bounded 'Docker build' 300 docker build -t clc-probe-image "$work"
stage_passed docker-build
probe_created=true
bounded 'Docker start' 30 docker run -d --name clc-probe clc-probe-image
wait_ready 'Docker exec proof' clc-probe "$service_timeout" docker exec clc-probe grep -qx built /proof
stage_passed docker-exec
printf 'services:\n  test:\n    image: clc-probe-image\n    healthcheck:\n      test: ["CMD", "grep", "-qx", "built", "/proof"]\n      interval: 1s\n      timeout: 2s\n      retries: 15\n' > "$work/compose.yml"
compose_created=true
bounded 'Compose health/readiness' 60 docker compose -f "$work/compose.yml" up -d --wait --wait-timeout 30
bounded 'Compose exec proof' 15 docker compose -f "$work/compose.yml" exec -T --interactive=false test grep -qx built /proof
# Finish this workload before long SQL/browser downloads, without an expiring sleep.
bounded 'Compose shutdown' 20 docker compose -f "$work/compose.yml" down --timeout 5
compose_created=false
stage_passed compose

if test "$mode" = full; then
  phase='SQL resource preflight'
  memory=$(timeout --kill-after=2 10s docker info --format '{{.MemTotal}}' < /dev/null)
  available=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
  progress "SQL resources: Docker memory=${memory} bytes; guest available=${available} KiB; container limit=4 GiB, SQL memory limit=3072 MiB"
  if ! [[ "$memory" =~ ^[0-9]+$ && "$available" =~ ^[0-9]+$ ]] || test "$memory" -lt 4294967296 || test "$available" -lt 3145728; then
    progress 'SQL validation requires at least 4 GiB Docker memory and 3 GiB available guest memory'; exit 1
  fi
  stage_passed sql-resources
  bounded 'SQL Server 2022 image download' 600 docker pull mcr.microsoft.com/mssql/server:2022-latest
  sql_created=true
  # Inherit credentials from the environment, never put them in argv. Keep
  # exited containers until diagnostics are captured; no --rm/restart loop.
  MSSQL_SA_PASSWORD="$password" bounded 'SQL Server container start' 30 docker run -d --name clc-sql \
    --memory 4g --memory-swap 4g -e ACCEPT_EULA=Y -e MSSQL_PID=Developer \
    -e MSSQL_MEMORY_LIMIT_MB=3072 -e MSSQL_SA_PASSWORD \
    -p 127.0.0.1:1433:1433/tcp mcr.microsoft.com/mssql/server:2022-latest
  container_running clc-sql 5
  stage_passed sql-server
  SQLCMDPASSWORD="$password" wait_ready 'SQL inside container' clc-sql "$sql_timeout" \
    docker exec -e SQLCMDPASSWORD clc-sql /opt/mssql-tools18/bin/sqlcmd \
    -S tcp:127.0.0.1,1433 -U sa -C -b -l 3 -t 5 -h -1 -W -Q "SET NOCOUNT ON; IF CONVERT(int, SERVERPROPERTY('ProductMajorVersion')) <> 16 THROW 51000, 'Expected SQL Server 2022', 1; SELECT 42"
  stage_passed sql-query-container
  phase='SQL port publication'
  ports=$(timeout --kill-after=2 5s docker port clc-sql 1433/tcp < /dev/null)
  progress "SQL published port: $ports"
  if test "$ports" != 127.0.0.1:1433; then progress 'Incorrect SQL networking: expected 127.0.0.1:1433'; exit 1; fi
  stage_passed sql-port
  client_created=true
  bounded 'Ubuntu host-network SQL client start' 30 docker run -d --name clc-sql-client --network host \
    --entrypoint /bin/sleep mcr.microsoft.com/mssql/server:2022-latest infinity
  # Namespace is Ubuntu's, not SQL's bridge. Explicit TCP IPv4 matches the
  # publication instead of accidentally trying localhost's IPv6 address ::1.
  SQLCMDPASSWORD="$password" wait_ready 'SQL Ubuntu localhost:1433 (127.0.0.1)' clc-sql-client "$sql_timeout" \
    docker exec -e SQLCMDPASSWORD clc-sql-client /opt/mssql-tools18/bin/sqlcmd \
    -S tcp:127.0.0.1,1433 -U sa -C -b -l 3 -t 5 -h -1 -W -Q "SET NOCOUNT ON; IF CONVERT(int, SERVERPROPERTY('ProductMajorVersion')) <> 16 THROW 51000, 'Expected SQL Server 2022', 1; SELECT 42"
  stage_passed sql-query-host
  bounded 'Ubuntu localhost IPv4 socket' 10 python3 -c 'import socket; s=socket.socket(socket.AF_INET,socket.SOCK_STREAM); s.settimeout(5); s.connect(("localhost",1433)); s.close()'
  stage_passed sql-localhost-socket
  bounded '.NET SQL project creation' 30 dotnet new console --framework net10.0 -o "$work/sql-probe"
  bounded 'SqlClient package restore' 180 dotnet add "$work/sql-probe" package Microsoft.Data.SqlClient --version 6.1.6
  cat > "$work/sql-probe/Program.cs" <<'CS'
using Microsoft.Data.SqlClient;
var password = Environment.GetEnvironmentVariable("CLC_SQL_PASSWORD") ?? throw new Exception("Missing ephemeral SQL credential");
var builder = new SqlConnectionStringBuilder { DataSource="localhost,1433", InitialCatalog="master", UserID="sa", Password=password, TrustServerCertificate=true, ConnectTimeout=3, ConnectRetryCount=0, Pooling=false };
using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(30));
for (var attempt = 1; ; attempt++) {
    deadline.Token.ThrowIfCancellationRequested();
    try {
        await using var connection = new SqlConnection(builder.ConnectionString);
        await connection.OpenAsync(deadline.Token);
        await using var command = new SqlCommand("SELECT 42", connection) { CommandTimeout=5 };
        if (Convert.ToInt32(await command.ExecuteScalarAsync(deadline.Token)) != 42) throw new Exception("Unexpected SQL result");
        break;
    } catch (SqlException) when (attempt < 10 && !deadline.IsCancellationRequested) {
        Console.WriteLine($".NET localhost SQL not ready, retry {attempt}/10");
        await Task.Delay(1000, deadline.Token);
    }
}
Console.WriteLine(".NET SQL localhost integration passed");
CS
  CLC_SQL_PASSWORD="$password" bounded '.NET SQL localhost integration' 180 dotnet run --project "$work/sql-probe" --configuration Release
  stage_passed dotnet-sql
  mkdir "$work/sql-tests"
  cat > "$work/sql-tests/Sql.Tests.csproj" <<'PROJECT'
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net10.0</TargetFramework><IsTestProject>true</IsTestProject><ImplicitUsings>enable</ImplicitUsings></PropertyGroup>
  <ItemGroup>
    <PackageReference Include="Microsoft.NET.Test.Sdk" Version="18.0.1" />
    <PackageReference Include="MSTest.TestAdapter" Version="4.5.2" />
    <PackageReference Include="MSTest.TestFramework" Version="4.5.2" />
    <ProjectReference Include="../sql-probe/sql-probe.csproj" />
  </ItemGroup>
</Project>
PROJECT
  cat > "$work/sql-tests/SqlTests.cs" <<'CS'
using Microsoft.Data.SqlClient;
using Microsoft.VisualStudio.TestTools.UnitTesting;
[TestClass]
public sealed class SqlTests {
    [TestMethod]
    public async Task UbuntuLocalhostQueriesSqlServer2022() {
        var builder = new SqlConnectionStringBuilder { DataSource="localhost,1433", InitialCatalog="master", UserID="sa", Password=Environment.GetEnvironmentVariable("CLC_SQL_PASSWORD"), TrustServerCertificate=true, ConnectTimeout=5, ConnectRetryCount=0, Pooling=false };
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(20));
        await using var connection = new SqlConnection(builder.ConnectionString);
        await connection.OpenAsync(deadline.Token);
        await using var command = new SqlCommand("SELECT CONVERT(int, SERVERPROPERTY('ProductMajorVersion'))", connection) { CommandTimeout=5 };
        Assert.AreEqual(16, Convert.ToInt32(await command.ExecuteScalarAsync(deadline.Token)));
        command.CommandText = "SELECT 42";
        Assert.AreEqual(42, Convert.ToInt32(await command.ExecuteScalarAsync(deadline.Token)));
    }
}
CS
  CLC_SQL_PASSWORD="$password" bounded '.NET SQL integration tests' 180 dotnet test "$work/sql-tests/Sql.Tests.csproj" --configuration Release --logger 'console;verbosity=normal'
  stage_passed dotnet-tests
  bounded 'Windows self-contained publish' 300 dotnet publish "$work/sql-probe" --configuration Release --runtime win-x64 --self-contained true --output "$work/windows-publish"
  test -f "$work/windows-publish/sql-probe.exe"
  stage_passed windows-publish
  bounded 'Windows ZIP package and integrity' 60 python3 -I -c 'import pathlib,sys,zipfile; root=pathlib.Path(sys.argv[1]); archive=root.parent/"windows-publish.zip"; files=sorted(p for p in root.rglob("*") if p.is_file()); assert files, "Empty publish output"; z=zipfile.ZipFile(archive,"w",zipfile.ZIP_DEFLATED); [z.write(p,p.relative_to(root)) for p in files]; z.close(); z=zipfile.ZipFile(archive); assert "sql-probe.exe" in z.namelist(); assert z.testzip() is None, "Corrupt package"; z.close(); print("Windows ZIP package integrity passed")' "$work/windows-publish"
  stage_passed windows-package
  mkdir "$work/browser"
  cd "$work/browser"
  bounded 'Browser project creation' 30 npm init -y
  bounded 'Playwright package download' 180 npm install --save-exact playwright@1.62.1
  bounded 'Chromium download' 300 npx playwright install chromium
  cat > probe.cjs <<'JS'
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const http = require('node:http');
// Host dependency validation must remain enabled as well as our ldd check.
delete process.env.PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS;
const { chromium } = require('playwright');
async function main() {
  if (process.argv[2] === 'dependencies') {
    const result = spawnSync('ldd', [chromium.executablePath()], { encoding: 'utf8', timeout: 10000 });
    console.log(result.stdout);
    if (result.stderr) console.error(result.stderr);
    assert.ifError(result.error);
    assert.equal(result.status, 0, 'Cannot inspect Chromium shared libraries');
    assert.doesNotMatch(result.stdout, /\bnot found\b/, 'Missing Chromium shared libraries');
    console.log('Chromium shared-library dependencies passed');
    return;
  }
  assert.equal(process.argv[2], 'browser');
  const server = http.createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/html' });
    res.end('<!doctype html><title>CLC browser probe</title><h1>CLC browser probe</h1><button onclick="document.querySelector(\'h1\').textContent=\'Browser assertion passed\'">Run assertion</button>');
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  let browser;
  try {
    // Match the exact Chromium binary checked by ldd (regular headless mode).
    browser = await chromium.launch({ channel: 'chromium', headless: true, timeout: 30000 });
    const page = await browser.newPage();
    page.setDefaultTimeout(10000);
    const response = await page.goto(`http://127.0.0.1:${server.address().port}`, { timeout: 10000 });
    assert.equal(response.status(), 200);
    assert.equal(await page.title(), 'CLC browser probe');
    assert.equal(await page.locator('h1').innerText(), 'CLC browser probe');
    await page.getByRole('button', { name: 'Run assertion' }).click();
    assert.equal(await page.locator('h1').innerText(), 'Browser assertion passed');
    console.log(`Chromium ${browser.version()} launch, HTTP page and browser assertion passed`);
  } finally {
    try { if (browser) await browser.close(); }
    finally { await new Promise(resolve => server.close(resolve)); }
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
JS
  bounded 'Chromium shared-library dependencies' 20 node probe.cjs dependencies
  stage_passed chromium-dependencies
  bounded 'Chromium launch and page assertion' 60 node probe.cjs browser
  stage_passed chromium-browser
  container_running clc-sql 5
  stage_passed sql-still-running
fi
sequence_completed=true
