package server

import (
	"os"
	"syscall"
)

// rotateOutput moves the log aside and points this process's stdout and stderr at a new
// one, but only when they are that file (the login item's): a terminal run is left alone.
func rotateOutput(path string) {
	out, err1 := os.Stdout.Stat()
	cur, err2 := os.Stat(path)
	if err1 != nil || err2 != nil || !os.SameFile(out, cur) {
		return
	}
	if os.Rename(path, path+".1") != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	syscall.Dup2(int(f.Fd()), 1)
	syscall.Dup2(int(f.Fd()), 2)
}
