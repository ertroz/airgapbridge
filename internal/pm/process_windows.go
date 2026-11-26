//go:build windows

package pm

import (
	"os/exec"
	"strconv"
)

func configureProcessGroup(_ *exec.Cmd) {
	// Windows uses taskkill /T /F on teardown
}

func KillProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return
	}
	_ = exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}
