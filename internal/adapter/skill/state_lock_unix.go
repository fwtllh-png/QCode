//go:build darwin

package skill

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type stateFileLock struct {
	file *os.File
}

func acquireStateFileLock(path string) (*stateFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		err = rejectMultiplyLinkedState(path, info)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("lock skill enable state: %w", err)
	}
	return &stateFileLock{file: file}, nil
}

func (l *stateFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return errors.Join(err, l.file.Close())
}

func rejectMultiplyLinkedState(_ string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("skill enable state is multiply linked or unverifiable")
	}
	return nil
}
