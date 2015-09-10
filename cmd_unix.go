// +build !windows

package main

import "os/exec"

func cmd(c string) *exec.Cmd {
	return exec.Command("sh", "-c", c)
}
