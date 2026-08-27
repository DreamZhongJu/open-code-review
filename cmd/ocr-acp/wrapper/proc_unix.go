// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

//go:build !windows

package wrapper

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup isolates the child so a later kill reaches the whole tree,
// including any helpers ocr may spawn during a diff review.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree terminates the child's entire process group. The receiver may be
// nil if the child already exited; errors are ignored because the caller only
// cares that no process survives the cancellation path.
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
