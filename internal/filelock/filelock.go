// Package filelock takes exclusive locks shared by every process that uses
// the same file: Vigie instances writing one pin file or one audit log.
package filelock

import (
	"fmt"
	"os"
	"path/filepath"
)

// Lock takes an exclusive lock on path+".lock", creating it (and its
// directory) if needed, and returns the function that releases it. It blocks
// until the lock is free.
func Lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockExclusive(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
	}
	return func() {
		_ = unlockFile(f)
		f.Close()
	}, nil
}
