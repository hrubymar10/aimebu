// Package runtimeignore protects generated checkout state from ordinary Git
// cleanup and accidental staging.
package runtimeignore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Ensure creates the checkout-local ignore file when it is missing. Existing
// files are left untouched so an owner can keep their own rules.
func Ensure(checkout string) error {
	dir := filepath.Join(checkout, ".aimebu")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", dir)
	}
	path := filepath.Join(dir, ".gitignore")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file", path)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString("*\n"); err != nil {
		return err
	}
	return file.Sync()
}
