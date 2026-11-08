package hyperv

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExistingJobMarkerTimestampUsesLifecycleProbeAndToleratesMissingMetadata(t *testing.T) {
	c, f, image := fixture(t)
	name := c.newName()
	if _, err := c.Create(context.Background(), name, image.ID, 42); err != nil {
		t.Fatal(err)
	}
	seconds := time.Now().Add(-38 * time.Minute).Unix()
	for _, value := range []string{strconv.FormatInt(seconds, 10), "malformed", "", "-1", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)} {
		f.sshOutput = "guest-ready\njob-started\njob-start-time=" + value + "\ncloud-finished\ncloud-ok\n"
		life, err := c.Lifecycle(context.Background(), name)
		if err != nil || !life.Started {
			t.Fatal("telemetry disrupted lifecycle", life, err)
		}
		if value == strconv.FormatInt(seconds, 10) {
			if life.StartedAt.Unix() != seconds || life.StartedAt.Location() != time.UTC {
				t.Fatal("incorrect actual marker start time", life)
			}
		} else if !life.StartedAt.IsZero() {
			t.Fatal("invalid timestamp accepted", life)
		}
		command := f.args[len(f.args)-1]
		if !strings.Contains(command, "stat -c %Y /home/runner/.clc/job-started") || strings.Contains(command, "GITHUB_") {
			t.Fatal("timestamp was not read from existing marker", command)
		}
	}
}
