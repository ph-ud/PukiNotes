//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach lets the new PukiNotes outlive this one and its terminal.
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
