//go:build !darwin && !linux

package main

import "os/exec"

func ownGroup(*exec.Cmd) {}
