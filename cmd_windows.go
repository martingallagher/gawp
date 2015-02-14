package main

import "os/exec"

func cmd(c string) ([]byte, error) {
	return exec.Command("cmd", "/C", c).Output()
}
