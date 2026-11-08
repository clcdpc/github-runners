package hyperv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/clcdpc/github-runners/internal/secure"
)

// Share one sink between stdout, stderr, heartbeats and lifecycle messages.
// Retain write failures even when a progress-only caller cannot return them.
type progressWriter struct {
	mu  sync.Mutex
	out io.Writer
	err error
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.out.Write(p)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

func (w *progressWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Native pipe writes can split a credential at ANY byte. Redact complete lines
// before streaming, and discard overlong lines instead of leaking fragments.
type redactedStream struct {
	out      io.Writer
	redactor *secure.Redactor
	pending  []byte
	dropping bool
}

func (w *redactedStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(w.pending)+len(part) > 64<<10 {
			w.pending = nil
			w.dropping = true
		}
		if !w.dropping {
			w.pending = append(w.pending, part...)
		}
		if i < 0 {
			break
		}
		if err := w.flush(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return n, nil
}

func (w *redactedStream) flush() error {
	var err error
	if w.dropping {
		_, err = io.WriteString(w.out, "[TestHyperV: output line exceeded 64 KiB; omitted]\n")
	} else if len(w.pending) > 0 {
		_, err = fmt.Fprintln(w.out, w.redactor.Text(string(w.pending)))
	}
	w.pending = nil
	w.dropping = false
	return err
}

func (c *Client) progress(format string, args ...any) error {
	if c.Progress != nil {
		operation := c.ProgressOperation
		if operation == "" {
			operation = "TestHyperV"
		}
		_, err := fmt.Fprintln(c.Progress, "["+operation+"] "+c.Redactor.Text(fmt.Sprintf(format, args...)))
		return err
	}
	return nil
}

func validationHeartbeat(ctx context.Context, out io.Writer) func() {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		start := time.Now()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(out, "[TestHyperV] Guest operation still running (%s elapsed; operation deadline remains in force)\n", time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() { close(done); <-stopped }
}
