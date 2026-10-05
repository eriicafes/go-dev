//go:build unix

package dev

import (
	"os/exec"
	"syscall"
)

// Put each managed process in its own process group so it and its children
// stop together.
func prepareCommand(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

func interruptProcess(command *exec.Cmd) error {
	return syscall.Kill(-command.Process.Pid, syscall.SIGINT)
}

func killProcess(command *exec.Cmd) error {
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
