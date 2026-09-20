//go:build !darwin && !linux

package process

import (
	"os"
	"os/exec"
)

// Other platforms retain their existing process lifecycle. Strong process-group
// completion is supported by the Darwin and Linux owners.
type sessionProcess struct {
	command *exec.Cmd
}

func (p *sessionProcess) signal(signal os.Signal) error {
	return signalProcessGroup(p.command.Process, signal)
}

func (p *sessionProcess) terminate() error {
	return terminateProcessGroup(p.command.Process)
}

func (p *sessionProcess) wait() error {
	return p.command.Wait()
}
