package server

import "golang.org/x/sys/unix"

func peerUID(fd int) int {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return -1
	}
	return int(cred.Uid)
}
