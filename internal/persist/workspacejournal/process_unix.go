//go:build darwin

package workspacejournal

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid still names a running process. Signal 0 only
// checks permission to signal, so it answers without disturbing the process.
//
// A pid can be reused, in which case this answers "alive" for the wrong process
// and recovery is skipped. That is the safe direction: skipping leaves the
// workspace as the crash left it, while guessing wrong the other way would undo
// writes a live process is still making.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.EPERM):
		// Someone else's process: running, just not ours to signal.
		return true
	default:
		return false
	}
}
