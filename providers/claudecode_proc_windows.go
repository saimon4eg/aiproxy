//go:build windows

package providers

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// sharedDirName namespaces the one cwd per OS user. There is no uid here, so the
// name is fixed; the directory still lives under the per-user temp dir.
func sharedDirName() string { return "claude-cli-cwd" }

// prepareSharedDir makes path a directory this process owns and reports whether it
// is safe to hand a child as its cwd.
func prepareSharedDir(path string) bool {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// CREATE_NEW_PROCESS_GROUP. taskkill /T is what actually reaps the node child
// behind the npm claude.cmd shim.
const createNewProcessGroup = 0x00000200

func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

func killOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}
