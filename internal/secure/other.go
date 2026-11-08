//go:build !windows

package secure

import (
	"errors"
	"time"
)

func ProcessRunning(int, time.Time, string) bool { return false }

func ReadPAT(string) (string, error) { return "", errors.New("DPAPI credentials require Windows") }
func Lock(string) (func(), error)    { return nil, errors.New("the controller requires Windows") }
