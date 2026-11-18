//go:build windows

package providers

import "os"

// On Windows the console check is done by the runtime: a console handle is a
// character device, and a redirected or absent stdin is not.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
