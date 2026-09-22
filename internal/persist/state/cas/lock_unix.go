//go:build darwin

package cas

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const lockRetryInterval = 10 * time.Millisecond

func lockFile(ctx context.Context, file *os.File, exclusive bool) error {
	operation := unix.LOCK_SH
	if exclusive {
		operation = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(file.Fd()), operation|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(lockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
