package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ---- P2-14 原子写与坏文件隔离 ----

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := writeFileAtomic(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"a":1}` {
		t.Fatalf("content = %q (err=%v), want %q", data, err, `{"a":1}`)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover should not exist after rename")
	}

	// 覆盖写同样成功
	if err := writeFileAtomic(path, []byte(`{"a":2}`), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != `{"a":2}` {
		t.Fatalf("content after overwrite = %q", data)
	}
}

func TestQuarantineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	quarantineFile(path, os.ErrInvalid)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original file should be renamed away")
	}
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatalf(".bad quarantine file missing: %v", err)
	}
}

func TestZenConfigQuarantine(t *testing.T) {
	oldPath, oldCfg := zenConfigPath, zenConfig
	t.Cleanup(func() { zenConfigPath, zenConfig = oldPath, oldCfg })

	dir := t.TempDir()
	zenConfigPath = filepath.Join(dir, ".cline-zen.json")
	if err := os.WriteFile(zenConfigPath, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	zenConfigMu.Lock()
	zenConfig = nil
	zenConfigMu.Unlock()

	cfg := getZenConfig()
	if _, err := os.Stat(zenConfigPath + ".bad"); err != nil {
		t.Fatalf("corrupt zen config not quarantined: %v", err)
	}
	if cfg.Key != "public" || cfg.MaxConcurrency != 8 {
		t.Fatalf("corrupt zen config should fall back to defaults, got %+v", cfg)
	}
}

func TestProxyConfigQuarantine(t *testing.T) {
	oldFile := proxyConfigFile
	t.Cleanup(func() { proxyConfigFile = oldFile })

	dir := t.TempDir()
	proxyConfigFile = filepath.Join(dir, ".cline-config.json")
	if err := os.WriteFile(proxyConfigFile, []byte("[[[nope"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := loadProxyConfigFromDisk()
	if _, err := os.Stat(proxyConfigFile + ".bad"); err != nil {
		t.Fatalf("corrupt proxy config not quarantined: %v", err)
	}
	if cfg.Strategy != "round_robin" {
		t.Fatalf("corrupt proxy config should fall back to default strategy, got %q", cfg.Strategy)
	}
}