package providers

// Native writes its working directory into the environment block it prepends to every
// request, so the child's cwd is part of the prompt every turn is compared against. A
// directory per request therefore moves the cached prefix on every turn: the entry is
// written under one path and read under another, and each turn pays the creation rate
// instead of the read rate. These tests pin the one property that prevents it — every
// child of this process runs in the same directory.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The directory handed to children must be stable, and it must be safe to share: a
// real directory this user owns, closed to everyone else.
func TestSharedWorkdirIsStableAndPrivate(t *testing.T) {
	path := sharedClaudeWorkdir()
	if path == "" {
		t.Fatalf("no shared workdir: the temp dir %q is not usable", os.TempDir())
	}
	if again := sharedClaudeWorkdir(); again != path {
		t.Fatalf("workdir moved between calls: %q then %q", path, again)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is a symlink", path)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("%s has mode %04o, want 0700", path, perm)
	}
	if !strings.HasPrefix(path, os.TempDir()) {
		t.Fatalf("%s is outside the temp dir %s", path, os.TempDir())
	}
}

// A directory whose mode came back open is tightened rather than refused: some
// filesystems hand a fresh 0700 mkdir back as 0770, and refusing our own directory
// would move the cwd on the next request — the failure this guards against.
func TestSharedWorkdirTightensALooseDirectory(t *testing.T) {
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o770); err != nil {
		t.Fatal(err)
	}
	if !prepareSharedDir(loose) {
		t.Fatalf("%s was refused instead of tightened", loose)
	}
	info, err := os.Stat(loose)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("%s left at %04o after preparation, want 0700", loose, perm)
	}
}

// A symlink and a plain file are refused: a symlink is something another user could
// have planted, and neither can be a working directory.
func TestSharedWorkdirRefusesWhatItCannotOwn(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if prepareSharedDir(link) {
		t.Fatalf("%s is a symlink and was accepted as a workdir", link)
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prepareSharedDir(file) {
		t.Fatalf("%s is a file and was accepted as a workdir", file)
	}
}

// The property the subscription bill depends on, exercised through the real request
// path: two consecutive requests start native in the same directory, so the
// environment block it builds is byte-identical.
func TestConsecutiveRequestsRunInTheSameDirectory(t *testing.T) {
	if sharedClaudeWorkdir() == "" {
		t.Skipf("no shared workdir in this environment (%q)", os.TempDir())
	}
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	dump := filepath.Join(t.TempDir(), "cwd.txt")
	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text", "dump_cwd": dump})
	for i := 0; i < 2; i++ {
		if rec := post(t, p, baseBody); rec.Code != 200 {
			t.Fatalf("request %d: status %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the fake CLI recorded no cwd: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two recorded cwds, got %d: %q", len(lines), raw)
	}
	if lines[0] != lines[1] {
		t.Fatalf("consecutive requests ran in different directories, so the cached prefix cannot repeat:\n  %s\n  %s", lines[0], lines[1])
	}
	if want := sharedClaudeWorkdir(); lines[0] != want {
		t.Fatalf("requests ran in %s, the shared workdir is %s", lines[0], want)
	}
}
