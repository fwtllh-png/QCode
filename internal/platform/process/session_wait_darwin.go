package process

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// NOTE_EXIT observes termination without reaping the child. Darwin waitid has
// historically returned stopped children even with WEXITED, so use kqueue.
func waitSessionLeader(process *os.Process) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return os.NewSyscallError("kqueue", err)
	}
	defer unix.Close(queue)
	unix.CloseOnExec(queue)
	change := []unix.Kevent_t{{
		Ident: uint64(process.Pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT,
	}}
	events := make([]unix.Kevent_t, 1)
	for {
		count, err := unix.Kevent(queue, change, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		// An already exited child may no longer accept a process filter. It
		// remains unreaped here, so its PID still belongs to this command.
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return os.NewSyscallError("kevent", err)
		}
		change = nil
		if count == 0 {
			continue
		}
		event := events[0]
		if event.Flags&unix.EV_ERROR != 0 {
			err := unix.Errno(event.Data)
			if err == unix.ESRCH {
				return nil
			}
			return os.NewSyscallError("kevent", err)
		}
		if event.Fflags&unix.NOTE_EXIT != 0 {
			return nil
		}
	}
}
