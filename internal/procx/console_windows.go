package procx

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsole keeps a console child from opening a window of its own. The
// Windows binary is linked for the GUI subsystem, so it has no console to
// lend and Windows allocates a fresh one per console child: an ffmpeg run
// flashes a window up and takes the focus with it. The child is left with
// no console at all, which costs nothing while every caller reads it
// through a pipe.
func HideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
