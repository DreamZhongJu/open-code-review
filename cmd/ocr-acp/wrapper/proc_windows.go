// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

//go:build windows

package wrapper

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows where Job Objects would be required;
// direct child kill remains effective for the ocr CLI itself. Tracked as a
// documented prototype limitation in cmd/ocr-acp/PROTOTYPE.md.
func setProcessGroup(cmd *exec.Cmd) {}

// killTree kills the direct child process.
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Kill()
}
