//go:build !darwin && !linux

package server

import "os/exec"

func ownGroup(*exec.Cmd) {}
