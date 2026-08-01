package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type timestampedStderrTee struct {
	mu       sync.Mutex
	terminal io.Writer
	warnings io.Writer
	file     *os.File
	path     string
	mode     os.FileMode
	pending  []byte
	warned   bool
	now      func() time.Time
}

func newTimestampedStderrTee(terminal, warnings io.Writer, path string, mode os.FileMode) *timestampedStderrTee {
	t := &timestampedStderrTee{
		terminal: terminal,
		warnings: warnings,
		mode:     mode,
		now:      time.Now,
	}
	t.mu.Lock()
	t.openLocked(path)
	t.mu.Unlock()
	return t
}

func (t *timestampedStderrTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n, terminalErr := t.terminal.Write(p)
	if t.file != nil {
		t.pending = append(t.pending, p...)
		for {
			idx := bytes.IndexByte(t.pending, '\n')
			if idx < 0 {
				break
			}
			line := append([]byte(nil), t.pending[:idx]...)
			t.pending = t.pending[idx+1:]
			if err := t.writeLineLocked(line); err != nil {
				t.disableFileLocked(err)
				break
			}
		}
	}
	return n, terminalErr
}

func (t *timestampedStderrTee) setPath(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if path == t.path && t.file != nil {
		return nil
	}
	oldPath := t.path
	if err := t.closeFileLocked(); err != nil {
		t.warnLocked(err)
		return err
	}
	if oldPath != "" && oldPath != path {
		if err := agentMergeDebugLogFile(oldPath, path); err != nil {
			t.warnLocked(err)
			return err
		}
	}
	return t.openLocked(path)
}

func (t *timestampedStderrTee) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeFileLocked()
}

func (t *timestampedStderrTee) openLocked(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.warnLocked(err)
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, t.mode)
	if err != nil {
		t.warnLocked(err)
		return err
	}
	t.file = f
	t.path = path
	return nil
}

func (t *timestampedStderrTee) closeFileLocked() error {
	if t.file == nil {
		return nil
	}
	if len(t.pending) > 0 {
		if err := t.writeLineLocked(t.pending); err != nil {
			t.disableFileLocked(err)
			return err
		}
		t.pending = nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

func (t *timestampedStderrTee) writeLineLocked(line []byte) error {
	stamp := t.now().UTC().Format(time.RFC3339Nano)
	_, err := fmt.Fprintf(t.file, "%s %s\n", stamp, line)
	return err
}

func (t *timestampedStderrTee) disableFileLocked(err error) {
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
	t.pending = nil
	t.warnLocked(err)
}

func (t *timestampedStderrTee) warnLocked(err error) {
	if t.warned {
		return
	}
	t.warned = true
	_, _ = fmt.Fprintf(t.warnings, "aimebu: stderr file logging disabled: %v\n", err)
}
