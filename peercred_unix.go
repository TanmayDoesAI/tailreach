//go:build darwin || linux

package main

import (
	"net"
	"os"
	"syscall"
)

// listenControl creates the settings socket readable only by this user.
func listenControl(path string) (net.Listener, error) {
	os.Remove(path)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	ln, err := net.Listen("unix", path)
	if err == nil {
		os.Chmod(path, 0o600)
	}
	return ln, err
}

// sameUser checks the connecting process runs as the same OS user.
func sameUser(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	match := false
	raw.Control(func(fd uintptr) { match = peerUID(int(fd)) == os.Getuid() })
	return match
}
