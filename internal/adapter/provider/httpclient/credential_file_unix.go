//go:build darwin

package httpclient

import (
	"os"
	"syscall"
)

func credentialOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Uid == uint32(os.Geteuid())
}

func openCredentialFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
