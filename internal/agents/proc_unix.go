//go:build unix

package agents

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs claude in its own process group. A terminal
// Ctrl-C goes to the whole foreground group; without this it would kill
// the very call diadoche wants to let finish (see --shutdown-timeout). A
// cancelled call is then killed together with its group, so the tool
// subprocesses claude started die with it.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
