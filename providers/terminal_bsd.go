//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package providers

import "syscall"

// The BSD family (and macOS) uses TIOCGETA instead of Linux's TCGETS.
const ioctlReadTermios = syscall.TIOCGETA
