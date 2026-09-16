//go:build !windows

package procx

import "os/exec"

// HideConsole is a no-op off Windows, where a child process is not handed a
// window of its own to begin with.
func HideConsole(*exec.Cmd) {}
