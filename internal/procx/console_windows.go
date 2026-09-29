package procx

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsole leaves the child no console, so read its output through
// pipes. The GUI-subsystem binary has none to lend, and Windows would open
// a window per console child that flashes up and takes the focus.
func HideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
