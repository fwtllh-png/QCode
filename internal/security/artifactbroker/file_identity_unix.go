//go:build darwin

package artifactbroker

import (
	"os"
	"syscall"
)

func linkedFile(_ string, _ *os.File, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink != 1
}

func sameDevice(
	_, _ string,
	_ *os.File,
	left, right os.FileInfo,
) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev
}
