// A disposable integration fixture. It never contacts GitHub or Hyper-V.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/restart"
	"github.com/google/uuid"
)

func hash(path string) string { b, _ := os.ReadFile(path); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func main() {
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func execute() error {
	if len(os.Args) < 4 {
		return fmt.Errorf("fixture expects command --config path")
	}
	command, path := os.Args[1], os.Args[3]
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	switch command {
	case "validate", "check-auth", "check-hyperv":
		fmt.Println("fixture validated")
	case "image-status":
		fmt.Println(`{"current":true,"image":{"id":"fixture-image"}}`)
	case "setup":
		fmt.Println(`{"current":true}`)
	case "launch-helper":
		requestPath := filepath.Join(c.Controller.DataDirectory, "restart.json")
		s := restart.Read(requestPath)
		m, e := restart.ValidateManifest(s.Target.ManifestPath, s.Target.ManifestHash)
		if e != nil {
			return e
		}
		pid, e := restart.StartDetached("pwsh.exe", restart.HelperArguments(m, requestPath, false), s.Root)
		if e != nil {
			return e
		}
		fmt.Println(pid)
	case "run":
		if _, e := os.Stat(filepath.Join(c.Controller.DataDirectory, "fixture-start-fail")); e == nil {
			return fmt.Errorf("fixture startup failure")
		}
		executable, _ := os.Executable()
		started := time.Now().UTC()
		nonce := uuid.NewString()
		for time.Since(started) < 60*time.Second {
			if _, e := os.Stat(filepath.Join(c.Controller.DataDirectory, fmt.Sprintf("fixture-crash-%d", os.Getpid()))); e == nil {
				_ = os.WriteFile(filepath.Join(c.Controller.DataDirectory, fmt.Sprintf("fixture-crashed-%d", os.Getpid())), []byte("intentional os.Exit(23)"), 0600)
				os.Exit(23)
			}
			state := "listening"
			if _, e := os.Stat(filepath.Join(c.Controller.DataDirectory, "fixture-not-ready")); e == nil {
				state = "starting"
			}
			_ = controller.WriteJSON(filepath.Join(c.Controller.DataDirectory, "status.json"), map[string]any{"pid": os.Getpid(), "nonce": nonce, "startedAt": started, "updatedAt": time.Now().UTC(), "status": state, "configHash": hash(path), "patHash": hash(c.Controller.PATFile), "runtimeHash": hash(c.HyperV.Script), "binaryHash": hash(executable), "imageId": "fixture-image", "restartSupported": true})
			if _, e := os.Stat(filepath.Join(c.Controller.DataDirectory, fmt.Sprintf("fixture-exit-%d", os.Getpid()))); e == nil {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	default:
		return fmt.Errorf("unsupported fixture command")
	}
	return nil
}
