package server

import "golang.org/x/sys/unix"

func peerUID(fd int) int {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return -1
	}
	return int(cred.Uid)
}
