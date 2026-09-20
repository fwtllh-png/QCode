//go:build darwin || linux

package process

import (
	"errors"
	"os"
	"os/exec"
	"sync"
)

// sessionProcess serializes group signals with the final reap. The leader stays
// waitable until the group is terminated, so its PID/PGID cannot be reused.
type sessionProcess struct {
	command *exec.Cmd
	mu      sync.Mutex
	reaped  bool
}

func (p *sessionProcess) signal(signal os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped || p.command.Process == nil {
		return os.ErrProcessDone
	}
	return signalProcessGroup(p.command.Process, signal)
}

func (p *sessionProcess) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped || p.command.Process == nil {
		return os.ErrProcessDone
	}
	return terminateProcessGroup(p.command.Process)
}

func (p *sessionProcess) wait() error {
	waitErr := waitSessionLeader(p.command.Process)
	p.mu.Lock()
	// Even a failed exit observation must settle the still-owned process group
	// before releasing the leader's identity.
	terminateErr := terminateProcessGroup(p.command.Process)
	p.reaped = true
	p.mu.Unlock()
	if errors.Is(terminateErr, os.ErrProcessDone) {
		terminateErr = nil
	}
	return errors.Join(waitErr, terminateErr, p.command.Wait())
}
