//go:build windows

package environment

import "os"

func platformUserTempDir() (string, error) {
	return os.TempDir(), nil
}
