//go:build windows

package bash

import "os/exec"

// isolateProcessGroup is a no-op on Windows: exec.CommandContext's default
// cancellation kills sh, and bashWaitDelay bounds how long a surviving
// descendant can keep the call open.
func isolateProcessGroup(*exec.Cmd) {}
