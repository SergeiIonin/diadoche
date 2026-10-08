//go:build !unix

package agents

import "os/exec"

// ConfigureProcessGroup is a no-op without Unix process groups: a
// cancelled call (or --on-task-done hook) kills only the process itself.
func ConfigureProcessGroup(*exec.Cmd) {}
