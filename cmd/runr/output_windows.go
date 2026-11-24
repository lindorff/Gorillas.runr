package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// filePipeLocalInformation is FILE_PIPE_LOCAL_INFORMATION, which
// NtQueryInformationFile returns for the FilePipeLocalInformation class.
type filePipeLocalInformation struct {
	NamedPipeType, NamedPipeConfiguration, MaximumInstances, CurrentInstances uint32
	InboundQuota, ReadDataAvailable, OutboundQuota, WriteQuotaAvailable       uint32
	NamedPipeState, NamedPipeEnd                                              uint32
}

const (
	classPipeLocalInformation = 24 // FilePipeLocalInformation
	pipeDisconnected          = 1  // FILE_PIPE_DISCONNECTED_STATE
	pipeClosing               = 4  // FILE_PIPE_CLOSING_STATE: the other end is closed
)

// readerGone reports whether f is a pipe whose other end is closed. It asks
// for the pipe's state rather than writing, so it works while nothing is
// written; anonymous pipes (cmd.exe, PowerShell) and Git Bash's pipes are
// all pipes of the named pipe file system.
func readerGone(f *os.File) bool {
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	gone := false
	rc.Control(func(fd uintptr) {
		h := windows.Handle(fd)
		if t, err := windows.GetFileType(h); err != nil || t != windows.FILE_TYPE_PIPE {
			return
		}
		var info filePipeLocalInformation
		var iosb windows.IO_STATUS_BLOCK
		if windows.NtQueryInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), classPipeLocalInformation) != nil {
			return
		}
		gone = info.NamedPipeState == pipeClosing || info.NamedPipeState == pipeDisconnected
	})
	return gone
}
