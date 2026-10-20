package usages

import (
	"os"
	"path/filepath"
	"syscall"
)

type FileLock struct {
	file *os.File
}

func lockPath(root string) string {
	return filepath.Join(root, ".lock")
}

func acquireLock(root string) (*FileLock, error) {
	return acquireNamedLock(root, ".lock")
}

func acquireProviderLock(root, provider string) (*FileLock, error) {
	return acquireNamedLock(root, ".fetch-"+provider+".lock")
}

func acquireNamedLock(root, name string) (*FileLock, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &FileLock{file: f}, nil
}

func (l *FileLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
