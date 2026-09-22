//go:build darwin

package process

import (
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(command *exec.Cmd, pty bool) {
	if !pty {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

func terminateProcessGroup(process *os.Process) error {
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	return process.Kill()
}

func signalProcessGroup(process *os.Process, signal os.Signal) error {
	value, ok := signal.(syscall.Signal)
	if ok {
		if err := syscall.Kill(-process.Pid, value); err == nil {
			return nil
		}
	}
	return process.Signal(signal)
}
