//go:build darwin

package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"golang.org/x/sys/unix"
)

const cwdHelperArgument = "__qcode_internal_fchdir_exec_v1"

func init() {
	if len(os.Args) < 3 || os.Args[1] != cwdHelperArgument {
		return
	}
	if err := unix.Fchdir(3); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "qcode cwd helper: fchdir: %v\n", err)
		os.Exit(126)
	}
	path, err := exec.LookPath(os.Args[2])
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "qcode cwd helper: executable: %v\n", err)
		os.Exit(126)
	}
	arguments := append([]string{os.Args[2]}, os.Args[3:]...)
	if err := syscall.Exec(path, arguments, os.Environ()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "qcode cwd helper: exec: %v\n", err)
		os.Exit(126)
	}
}

func commandForSpec(
	ctx context.Context,
	spec sandbox.Command,
	directory *os.File,
) (*exec.Cmd, error) {
	if directory == nil {
		command := exec.CommandContext(ctx, spec.Path, spec.Args[1:]...)
		command.Dir = spec.Dir
		command.Env = spec.Env
		return command, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate cwd helper executable: %w", err)
	}
	arguments := []string{cwdHelperArgument, spec.Path}
	arguments = append(arguments, spec.Args[1:]...)
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = "/"
	command.Env = spec.Env
	command.ExtraFiles = []*os.File{directory}
	return command, nil
}
