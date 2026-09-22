//go:build darwin

package memory

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type recordsLock struct {
	file *os.File
}

func acquireRecordsLock(path string) (*recordsLock, error) {
	descriptor, err := unix.Open(
		path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open memory records lock: %w", err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("open memory records lock")
	}
	info, err := file.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			err = errors.New("memory records lock is multiply linked or unverifiable")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock memory records: %w", err)
	}
	return &recordsLock{file: file}, nil
}

func (l *recordsLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return errors.Join(err, l.file.Close())
}
