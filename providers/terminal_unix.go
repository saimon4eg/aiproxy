//go:build !windows

package providers

import (
	"os"
	"syscall"
	"unsafe"
)

// stdinIsTerminal reports whether stdin is an interactive terminal.
//
// A plain os.ModeCharDevice check is not enough: /dev/null is a character device
// too, and a container without a TTY has exactly that on stdin — the login flow
// would then start against /dev/null and hang forever waiting for input.
func stdinIsTerminal() bool {
	fd := os.Stdin.Fd()
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL, fd, ioctlReadTermios,
		uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}
