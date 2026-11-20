//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach lets the new PukiNotes outlive this one and its console.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200 | 0x00000008} // NEW_PROCESS_GROUP | DETACHED_PROCESS
}
