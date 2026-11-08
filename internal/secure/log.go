package secure

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type DailyLog struct {
	Directory     string
	RetentionDays int
	RunnerPrefix  string
	mu            sync.Mutex
	day           string
	file          *os.File
}

func (l *DailyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	if day != l.day {
		if l.file != nil {
			l.file.Close()
		}
		if err := os.MkdirAll(l.Directory, 0700); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(filepath.Join(l.Directory, "controller-"+day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		l.file = f
		l.day = day
		if err := l.prune(); err != nil {
			return 0, err
		}
	}
	return l.file.Write(p)
}
func (l *DailyLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
func (l *DailyLog) prune() error {
	before := time.Now().AddDate(0, 0, -l.RetentionDays)
	entries, err := os.ReadDir(l.Directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && controllerLogName.MatchString(entry.Name()) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().Before(before) {
				if err = os.Remove(filepath.Join(l.Directory, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	archiveRoot := filepath.Join(l.Directory, "runners")
	archives, err := os.ReadDir(archiveRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range archives {
		if entry.IsDir() && l.RunnerPrefix != "" && strings.HasPrefix(entry.Name(), l.RunnerPrefix) && runnerLogName.MatchString(entry.Name()) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().Before(before) {
				if err = os.RemoveAll(filepath.Join(archiveRoot, entry.Name())); err != nil {
					return fmt.Errorf("prune archived diagnostics: %w", err)
				}
			}
		}
	}
	return nil
}

var controllerLogName = regexp.MustCompile(`^controller-[0-9]{4}-[0-9]{2}-[0-9]{2}\.jsonl$`)
var runnerLogName = regexp.MustCompile(`^[A-Za-z0-9_.-]+-[a-f0-9]{8}-[a-f0-9]{12}$`)
