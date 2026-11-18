//go:build linux

package providers

import "syscall"

// TCGETS on Linux; the BSDs and macOS use TIOCGETA.
const ioctlReadTermios = syscall.TCGETS
