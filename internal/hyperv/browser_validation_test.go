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

// Exercise the actual embedded JS and its assertions, rather than having the
// shell fixture's node stub accept any browser program. Browser APIs and ldd
// are simulated; the HTTP page, Node assertions and failure handling are real.
func TestEmbeddedBrowserAssertions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for embedded browser assertion regression tests")
	}
	script, err := guest.ReadFile("guest/verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, probe, ok := strings.Cut(strings.ReplaceAll(string(script), "\r\n", "\n"), "cat > probe.cjs <<'JS'\n")
	if !ok {
		t.Fatal("mandatory browser probe missing")
	}
	probe, _, ok = strings.Cut(probe, "\nJS\n")
	if !ok {
		t.Fatal("browser probe heredoc incomplete")
	}
	for _, scenario := range []string{"dependencies-ready", "dependencies-missing", "dependencies-unavailable", "browser-ready", "launch-failure", "wrong-title", "wrong-heading", "click-noop"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			moduleDir := filepath.Join(dir, "node_modules", "playwright")
			if err := os.MkdirAll(moduleDir, 0700); err != nil {
				t.Fatal(err)
			}
			for path, source := range map[string]string{
				filepath.Join(dir, "probe.cjs"):      probe,
				filepath.Join(dir, "preload.cjs"):    browserPreloadFixture,
				filepath.Join(moduleDir, "index.js"): browserAPIFixture,
			} {
				if err := os.WriteFile(path, []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			mode := "browser"
			if strings.HasPrefix(scenario, "dependencies-") {
				mode = "dependencies"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "--require", "./preload.cjs", "probe.cjs", mode)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "CLC_TEST_BROWSER_SCENARIO="+scenario, "PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1")
			output, runErr := cmd.CombinedOutput()
			if (runErr == nil) != strings.HasSuffix(scenario, "-ready") {
				t.Fatal("browser assertion or dependency failure was ignored", scenario, runErr, string(output))
			}
			if mode == "browser" && scenario != "launch-failure" && !strings.Contains(string(output), "fixture browser closed") {
				t.Fatal("browser leaked after assertion failure", string(output))
			}
		})
	}
}

const browserPreloadFixture = `
const assert = require('node:assert/strict');
require('node:child_process').spawnSync = (command, args, options) => {
  assert.equal(command, 'ldd');
  assert.deepEqual(args, ['/fixture/chromium']);
  assert.equal(options.timeout, 10000);
  const scenario = process.env.CLC_TEST_BROWSER_SCENARIO;
  return {status: scenario === 'dependencies-unavailable' ? 1 : 0,
    stdout: scenario === 'dependencies-missing' ? 'libfixture.so => not found' : 'libfixture.so => /lib/libfixture.so', stderr: ''};
};
`

const browserAPIFixture = `
const assert = require('node:assert/strict');
assert.equal(process.env.PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS, undefined);
const scenario = process.env.CLC_TEST_BROWSER_SCENARIO;
exports.chromium = {
  executablePath: () => '/fixture/chromium',
  launch: async options => {
    assert.equal(options.headless, true);
    assert.equal(options.channel, 'chromium');
    assert.equal(options.timeout, 30000);
    if (scenario === 'launch-failure') throw Error('Fixture browser failed to launch');
    return {
      version: () => 'fixture',
      close: async () => console.log('fixture browser closed'),
      newPage: async () => {
        let html = '', clicked = false;
        return {
          setDefaultTimeout: value => assert.equal(value, 10000),
          goto: async (url, options) => {
            assert.match(url, /^http:\/\/127\.0\.0\.1:\d+$/);
            assert.equal(options.timeout, 10000);
            const response = await fetch(url);
            html = await response.text();
            return {status: () => response.status};
          },
          title: async () => scenario === 'wrong-title' ? 'Unexpected title' : html.match(/<title>(.*?)<\/title>/)[1],
          locator: selector => {
            assert.equal(selector, 'h1');
            return {innerText: async () => scenario === 'wrong-heading' ? 'Unexpected heading' : clicked ? 'Browser assertion passed' : html.match(/<h1>(.*?)<\/h1>/)[1]};
          },
          getByRole: (role, options) => {
            assert.equal(role, 'button');
            assert.equal(options.name, 'Run assertion');
            assert.match(html, /onclick=/);
            return {click: async () => {clicked = scenario !== 'click-noop';}};
          },
        };
      },
    };
  },
};
`
