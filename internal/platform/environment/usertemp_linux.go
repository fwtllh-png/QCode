//go:build linux

package environment

import (
	"os"
	"strings"
)

func platformUserTempDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("TMPDIR")); value != "" {
		return value, nil
	}
	return "/tmp", nil
}
