//go:build darwin

package host

import (
	"errors"
	"os"
	"syscall"
)

func tryLockOwnerLease(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func openOwnerLeaseFile(path string) (*os.File, error) {
	fd, err := syscall.Open(
		path,
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func unlockOwnerLease(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func ownerLeaseWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
