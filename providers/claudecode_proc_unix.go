//go:build !windows

package providers

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// sharedDirName namespaces the one cwd per OS user. The temp dir is shared, so a
// directory someone else created first must never be adopted.
func sharedDirName() string { return fmt.Sprintf("claude-cli-cwd-%d", os.Getuid()) }

// prepareSharedDir makes path a directory closed to other users and reports whether
// it is safe to hand a child as its cwd.
//
// Some filesystems hand back a fresh 0700 mkdir as 0770, and the check below would
// then refuse our own directory on the next request — moving the cwd, which is
// exactly what this directory exists to prevent. The mode changes through a
// descriptor opened without following links: a parent others can write lets them
// swap a symlink in after the mkdir, and a chmod by name would then hit whatever
// that points at.
func prepareSharedDir(path string) bool {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false
	}
	fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer func() { _ = fd.Close() }()
	if err := fd.Chmod(0o700); err != nil {
		return false
	}
	info, err := fd.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return info.IsDir() && int(stat.Uid) == os.Getuid() && info.Mode().Perm()&0o077 == 0
}

// setOwnProcessGroup puts the child in its own group so cancellation can signal
// the whole tree. The npm shim execs node; without a group, node would outlive a
// cancelled request and keep drawing the subscription.
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
