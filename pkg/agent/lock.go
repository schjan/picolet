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
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() error {
		var errs []error
		if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
			errs = append(errs, fmt.Errorf("unlocking %s: %w", path, err))
		}
		if err := unix.Close(fd); err != nil {
			errs = append(errs, fmt.Errorf("closing %s: %w", path, err))
		}
		return errors.Join(errs...)
	}, nil
}
