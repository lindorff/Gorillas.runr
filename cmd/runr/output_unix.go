//go:build !windows

package main

import (
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// ignoreBrokenPipe makes a write to a pipe nobody reads fail with EPIPE,
// which outputError turns into a quiet end, rather than kill runr with
// SIGPIPE, as Go does for stdout.
func ignoreBrokenPipe() { signal.Ignore(unix.SIGPIPE) }

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
