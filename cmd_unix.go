// +build !windows

package main

import "os/exec"

func cmd(c string) ([]byte, error) {
	return exec.Command("sh", "-c", c).Output()
}
