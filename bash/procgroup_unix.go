//go:build !windows

package bash

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup runs sh as the leader of its own process group, and
// makes cancellation (timeout or parent cancel) SIGKILL that whole group. A
// background job or a foreground child sh has not exec'd into then dies with
// sh, instead of holding the output pipe open until it finishes on its own.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// A negative pid addresses the process group whose id is sh's pid.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
