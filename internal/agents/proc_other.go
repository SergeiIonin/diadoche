//go:build !unix

package agents

import "os/exec"

// configureProcessGroup is a no-op without Unix process groups: a
// cancelled call kills only the claude process itself.
func configureProcessGroup(*exec.Cmd) {}
