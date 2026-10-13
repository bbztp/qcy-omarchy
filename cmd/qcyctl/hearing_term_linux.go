//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// rawTerminal puts fd in non-canonical, no-echo mode (signals stay enabled, so
// Ctrl-C still raises SIGINT) and returns the function that restores it.
func rawTerminal(fd int) (func(), error) {
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&old))); e != 0 {
		return nil, errors.New("stdin is not a terminal (this command needs a keyboard)")
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&raw))); e != 0 {
		return nil, e
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
	}, nil
}

// shrinkPipe asks for the smallest pipe buffer (one page, ~23 ms of the tone
// stream) so a tone stops promptly: the fade-out queues behind at most that.
func shrinkPipe(f *os.File) {
	const fSetPipeSz = 1031
	syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fSetPipeSz, 4096)
}
