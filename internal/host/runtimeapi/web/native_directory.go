package web

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type directoryPicker func(context.Context, string) (string, bool, error)

type directoryPickerCommand struct {
	name string
	args []string
}

func nativeDirectoryPicker() directoryPicker {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err == nil {
			return pickNativeDirectory
		}
	}
	return nil
}

func pickNativeDirectory(
	ctx context.Context,
	initialPath string,
) (string, bool, error) {
	command, err := nativeDirectoryPickerCommand(runtime.GOOS, initialPath)
	if err != nil {
		return "", false, err
	}
	output, err := exec.CommandContext(ctx, command.name, command.args...).CombinedOutput()
	selected := strings.TrimSpace(string(output))
	if err != nil {
		return "", false, fmt.Errorf(
			"native directory picker failed: %w: %s",
			err,
			selected,
		)
	}
	if selected == "" {
		return "", true, nil
	}
	return filepath.Clean(selected), false, nil
}

func nativeDirectoryPickerCommand(
	goos string,
	initialPath string,
) (directoryPickerCommand, error) {
	initialPath = filepath.Clean(initialPath)
	switch goos {
	case "darwin":
		const script = `on run argv
try
set selectedFolder to choose folder with prompt "Choose a workspace folder" default location (POSIX file (item 1 of argv))
return POSIX path of selectedFolder
on error number -128
return ""
end try
end run`
		return directoryPickerCommand{
			name: "osascript",
			args: []string{"-e", script, "--", initialPath},
		}, nil
	default:
		return directoryPickerCommand{}, errors.New(
			"native directory selection is unsupported on this platform",
		)
	}
}
