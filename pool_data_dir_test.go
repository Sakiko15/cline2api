package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// resetDataDirCache 清空 resolveDataDir 的 sync.Once 缓存（CLINE_DATA_DIR 相关
// 用例共享全局缓存，必须逐用例重置；测试结束后再次清空，避免脏缓存外泄）。
func resetDataDirCache(t *testing.T) {
	t.Helper()
	reset := func() {
		resolveDataDirOnce = sync.Once{}
		resolvedDataDir = ""
	}
	reset()
	t.Cleanup(reset)
}

// TestResolveDataPathEnvDirFirst：CLINE_DATA_DIR 下已有数据文件时优先命中。
func TestResolveDataPathEnvDirFirst(t *testing.T) {
	resetDataDirCache(t)
	dir := t.TempDir()
	t.Setenv("CLINE_DATA_DIR", dir)
	name := "accounts-resolve-test.json"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveDataPath(name); got != filepath.Join(dir, name) {
		t.Fatalf("resolveDataPath(%q) = %q, want %q", name, got, filepath.Join(dir, name))
	}
}

// TestResolveDataDirPrefersEnvDir：env 目录无既有文件时也是新文件落点，
// 目录不存在时自动创建（probeDataDir 的 MkdirAll）。
func TestResolveDataDirPrefersEnvDir(t *testing.T) {
	resetDataDirCache(t)
	dir := filepath.Join(t.TempDir(), "nested-data-dir") // 故意不存在
	t.Setenv("CLINE_DATA_DIR", dir)
	if got := resolveDataDir(); got != dir {
		t.Fatalf("resolveDataDir() = %q, want %q", got, dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("env data dir not created: err=%v", err)
	}
}

// TestResolveDataPathFallsBackToEnvDir：所有候选位置都没有该文件时回退到
// CLINE_DATA_DIR（新文件落在挂载的数据目录）。用不常见文件名避免 exe/cwd/
// 主目录候选位置的真实文件干扰。
func TestResolveDataPathFallsBackToEnvDir(t *testing.T) {
	resetDataDirCache(t)
	dir := t.TempDir()
	t.Setenv("CLINE_DATA_DIR", dir)
	name := "no-such-data-file-anywhere-test.json"
	if got := resolveDataPath(name); got != filepath.Join(dir, name) {
		t.Fatalf("resolveDataPath(%q) = %q, want %q", name, got, filepath.Join(dir, name))
	}
}

// TestFileExistsRejectsDirectory：候选位置的同名路径是目录（Docker 单文件
// 挂载陷阱）时不能当作数据文件命中。
func TestFileExistsRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if fileExists(dir) {
		t.Fatal("directory must not count as an existing data file")
	}
	p := filepath.Join(dir, "real-file.json")
	if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if !fileExists(p) {
		t.Fatal("regular file must count as existing")
	}
}

// TestWriteFileAtomicHealsEmptyDirTrap：目标是空目录（目录陷阱）时自愈删除
// 并以原子写落盘。
func TestWriteFileAtomicHealsEmptyDirTrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heal-trap.json")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("data"), 0600); err != nil {
		t.Fatalf("empty-directory self-heal failed: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("trap dir not replaced by file: err=%v isDir=%v", err, info != nil && info.IsDir())
	}
}

// TestWriteFileAtomicNonEmptyDirTarget：目标是非空目录（陷阱目录已被污染）
// 时报错并提示，不得静默覆盖目录内容。
func TestWriteFileAtomicNonEmptyDirTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonempty-dir-trap.json")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	err := writeFileAtomic(path, []byte("data"), 0600)
	if err == nil || !strings.Contains(err.Error(), "non-empty directory") {
		t.Fatalf("want non-empty-directory error, got %v", err)
	}
}

// TestWriteFileAtomicInPlaceFallback：rename 失败（单文件 bind mount 的 EBUSY
// 场景）时兜底原地写，数据不丢、不留 .tmp 残留。
func TestWriteFileAtomicInPlaceFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic-fallback.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	osRenameMu.Lock()
	oldRename := osRenameFn
	osRenameFn = func(_, _ string) error { return os.ErrPermission }
	osRenameMu.Unlock()
	t.Cleanup(func() {
		osRenameMu.Lock()
		osRenameFn = oldRename
		osRenameMu.Unlock()
	})

	if err := writeFileAtomic(path, []byte("new-content"), 0600); err != nil {
		t.Fatalf("in-place fallback failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new-content" {
		t.Fatalf("in-place write not persisted: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}
}
