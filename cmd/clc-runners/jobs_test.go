package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/secure"
)

func executeCaptured(t *testing.T, arguments ...string) (string, error) {
	t.Helper()
	previousArgs, previousOut := os.Args, os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args, os.Stdout = append([]string{"clc-runners"}, arguments...), writer
	defer func() { os.Args, os.Stdout = previousArgs, previousOut }()
	err = execute(&secure.Redactor{})
	writer.Close()
	data, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), err
}

func jobsFixture(t *testing.T) (config.Config, string, *controller.State) {
	t.Helper()
	c, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c.Controller.DataDirectory, c.Controller.LogDirectory, c.Controller.PATFile = filepath.Join(dir, "data"), filepath.Join(dir, "logs"), filepath.Join(dir, "missing.dpapi")
	c.HyperV.PowerShell = filepath.Join(dir, "no-runtime-operations.exe")
	path := filepath.Join(dir, "controller.json")
	if err := controller.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	s := &controller.State{Version: 2, Owner: "12345678-0000-4000-8000-000000000001", Scope: controller.Scope(c), ScaleSetID: 42, Runners: map[string]*controller.Runner{}}
	return c, path, s
}

func fileSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			files[path] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestJobsDispatchAndEmptyJSONAreReadOnly(t *testing.T) {
	c, path, _ := jobsFixture(t)
	before := fileSnapshot(t, filepath.Dir(path))
	for _, asJSON := range []bool{false, true} {
		arguments := []string{"jobs", "--config", path}
		if asJSON {
			arguments = append(arguments, "--json")
		}
		output, err := executeCaptured(t, arguments...)
		if err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var report map[string]any
			if json.Unmarshal([]byte(output), &report) != nil || report["schemaVersion"] != float64(1) || len(report["runners"].([]any)) != 0 || report["availableCapacity"] != float64(3) {
				t.Fatal("JSON contaminated or incorrect", output)
			}
		} else if !strings.Contains(output, "No active controller-managed runners.") {
			t.Fatal(output)
		}
	}
	if _, err := os.Stat(c.Controller.DataDirectory); !os.IsNotExist(err) {
		t.Fatal("Jobs initialized controller data")
	}
	if after := fileSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
		t.Fatal("Jobs modified files")
	}
}

func TestJobsReadsLegacyBusyStateAndRetainsLocallyKnownMetadataWithoutCredentials(t *testing.T) {
	c, path, s := jobsFixture(t)
	name := "clc-linux-12345678-fdd987dc2b5b"
	r := &controller.Runner{Name: name, RunnerID: 398, Phase: "busy", CreatedAt: time.Now().Add(-time.Hour)}
	s.Runners[name] = r
	for _, withMetadata := range []bool{false, true} {
		if withMetadata {
			r.Job = &controller.JobAssignment{Repository: "clcdpc/current", DisplayName: "Run tests", WorkflowRunID: 123, StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
		}
		if err := controller.SaveState(filepath.Join(c.Controller.DataDirectory, "state.json"), s); err != nil {
			t.Fatal(err)
		}
		before := fileSnapshot(t, filepath.Dir(path))
		output, err := executeCaptured(t, "jobs", "--config", path, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Runners []struct {
				Name, State, LookupError, JobDisplayName string
				ElapsedSeconds                           *int64
			}
		}
		if err = json.Unmarshal([]byte(output), &report); err != nil {
			t.Fatal(err, output)
		}
		row := report.Runners[0]
		if row.Name != name || row.State != "Unknown" || row.ElapsedSeconds != nil {
			t.Fatal("legacy busy state falsely reported running", output)
		}
		if withMetadata && (row.LookupError == "" || row.JobDisplayName != "Run tests") {
			t.Fatal("credentials failure lost assignment", output)
		}
		if !reflect.DeepEqual(before, fileSnapshot(t, filepath.Dir(path))) {
			t.Fatal("Jobs modified journal, credentials or runtime")
		}
	}
}

func TestStatusStillReturnsItsOriginalDiagnosticJSON(t *testing.T) {
	c, path, s := jobsFixture(t)
	output, err := executeCaptured(t, "status", "--config", path)
	if err != nil || strings.TrimSpace(output) != `{"initialized":false}` {
		t.Fatal("uninitialized Status changed", err, output)
	}
	if err := controller.SaveState(filepath.Join(c.Controller.DataDirectory, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	output, err = executeCaptured(t, "status", "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err = json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"runtime", "state", "vms", "inventoryAvailable", "configuredResources", "configurationCurrent", "runtimeFresh"} {
		if _, ok := report[key]; !ok {
			t.Fatal("Status property removed", key, output)
		}
	}
	if _, ok := report["schemaVersion"]; ok {
		t.Fatal("Jobs replaced Status diagnostics")
	}
}

func TestJobsFlagsAndCorruptStateFailWithoutWriting(t *testing.T) {
	c, path, _ := jobsFixture(t)
	if output, err := executeCaptured(t, "status", "--config", path, "--json"); err == nil || output != "" {
		t.Fatal("Jobs flag changed Status")
	}
	if err := os.MkdirAll(c.Controller.DataDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Controller.DataDirectory, "state.json"), []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	before := fileSnapshot(t, filepath.Dir(path))
	if output, err := executeCaptured(t, "jobs", "--config", path, "--json"); err == nil || output != "" {
		t.Fatal("corrupt journal was treated as empty", err, output)
	}
	if !reflect.DeepEqual(before, fileSnapshot(t, filepath.Dir(path))) {
		t.Fatal("failure changed durable data")
	}
}
