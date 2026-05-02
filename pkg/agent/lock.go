package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrLocked is returned when another picolet process already holds the lock.
var ErrLocked = errors.New("picolet lock already held")

// AcquireLock takes an exclusive non-blocking process lock and keeps it until
// the returned release function is called.
func AcquireLock(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { //nolint:gosec // file descriptors are small non-negative ints from the OS.
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() error {
		var errs []error
		if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil { //nolint:gosec // file descriptors are small non-negative ints from the OS.
			errs = append(errs, fmt.Errorf("unlocking %s: %w", path, err))
		}
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing %s: %w", path, err))
		}
		return errors.Join(errs...)
	}, nil
}
