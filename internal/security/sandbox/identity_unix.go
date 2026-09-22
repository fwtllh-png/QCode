//go:build darwin

package sandbox

import (
	"errors"
	"io/fs"
	"syscall"
)

type fileIdentity struct {
	device uint64
	inode  uint64
	links  uint64
}

func identityOf(_ string, info fs.FileInfo) (fileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, errors.New("filesystem identity is unavailable")
	}
	return fileIdentity{
		device: uint64(stat.Dev),
		inode:  uint64(stat.Ino),
		links:  uint64(stat.Nlink),
	}, nil
}
