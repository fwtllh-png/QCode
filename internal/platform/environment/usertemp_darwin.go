//go:build darwin

package environment

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func platformUserTempDir() (string, error) {
	// getconf DARWIN_USER_TEMP_DIR is the documented userland for
	// confstr(_CS_DARWIN_USER_TEMP_DIR). Do not infer this path from mktemp.
	ctx, cancel := context.WithTimeout(context.Background(), sandbox.ToolchainProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/getconf", "DARWIN_USER_TEMP_DIR")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("confstr CS_DARWIN_USER_TEMP_DIR: %w", err)
	}
	value := strings.TrimSpace(output.String())
	if value == "" {
		return "", fmt.Errorf("confstr CS_DARWIN_USER_TEMP_DIR returned empty")
	}
	return value, nil
}
