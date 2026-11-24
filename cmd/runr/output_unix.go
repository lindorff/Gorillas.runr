//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// readerGone reports whether f is a pipe (or socket) whose other end is
// closed: poll says so with POLLERR (Linux) or POLLHUP (macOS).
func readerGone(f *os.File) bool {
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	gone := false
	rc.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		if n, err := unix.Poll(fds, 0); err == nil && n > 0 {
			gone = fds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0
		}
	})
	return gone
}
