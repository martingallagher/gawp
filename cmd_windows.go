package main

import "os/exec"

func cmd(c string) *exec.Cmd {
	return exec.Command("cmd", "/C", c)
}
