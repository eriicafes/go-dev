//go:build !unix

package dev

import (
	"os"
	"os/exec"
)

func prepareCommand(command *exec.Cmd) {}

func interruptProcess(command *exec.Cmd) error {
	return command.Process.Signal(os.Interrupt)
}

func killProcess(command *exec.Cmd) error {
	return command.Process.Kill()
}
