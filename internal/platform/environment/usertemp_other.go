//go:build !darwin && !linux && !windows

package environment

import (
	"fmt"
	"runtime"
)

func platformUserTempDir() (string, error) {
	return "", fmt.Errorf("user temp resolution is unsupported on %s", runtime.GOOS)
}
