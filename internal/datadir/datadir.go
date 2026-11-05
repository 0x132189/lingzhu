// Package datadir guards one data directory against concurrent core processes.
// Two cores sharing a data directory would race on the session database and on
// recovery, so the second one fails instead of corrupting state.
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
)

// Guard is an exclusive claim on a data directory.
type Guard struct {
	file *os.File
}

// Lock takes the claim for dir, creating it when needed.
func Lock(dir string) (*Guard, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "core.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("data directory %s is already in use: %w", dir, err)
	}
	return &Guard{file: file}, nil
}

// Release drops the claim. It is safe on a nil lock.
func (l *Guard) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	l.file = nil
	return err
}
