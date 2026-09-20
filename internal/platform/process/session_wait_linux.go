package process

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func waitSessionLeader(process *os.Process) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return os.NewSyscallError("waitid", err)
	}
}
