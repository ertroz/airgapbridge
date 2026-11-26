//go:build !windows

package pm

import (
	"os/exec"
	"syscall"
	"time"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func KillProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return
	}
	pid := cmd.Process.Pid
	// Negative PID sends signal to the entire process group
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		// Poll briefly for exit
		for i := 0; i < 30; i++ {
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		close(done)
	}()
	<-done
}
