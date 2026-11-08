package secure

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogRetentionOnlyPrunesOwnedArchives(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().AddDate(0, 0, -60)
	for _, name := range []string{"controller-2000-01-01.jsonl", "controller-unrelated.jsonl"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	owned := "clc-linux-12345678-abcdef123456"
	foreign := "clc-linux-87654321-abcdef123456"
	for _, name := range []string{owned, foreign, "unrelated"} {
		path := filepath.Join(dir, "runners", name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	l := &DailyLog{Directory: dir, RetentionDays: 30, RunnerPrefix: "clc-linux-12345678-"}
	if _, err := l.Write([]byte("current\n")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "controller-2000-01-01.jsonl"), filepath.Join(dir, "runners", owned)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("expired owned log was retained")
		}
	}
	for _, path := range []string{filepath.Join(dir, "controller-unrelated.jsonl"), filepath.Join(dir, "runners", foreign), filepath.Join(dir, "runners", "unrelated"), filepath.Join(dir, "controller-"+time.Now().Format("2006-01-02")+".jsonl")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("current or unrelated log was removed", err)
		}
	}
}
