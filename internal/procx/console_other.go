//go:build !windows

package procx

import "os/exec"

func HideConsole(*exec.Cmd) {}
