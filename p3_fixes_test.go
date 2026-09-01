package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- P3-1：池/请求日志脏标记+防抖落盘 ----

func poolFileHasKey(t *testing.T, key string) bool {
	t.Helper()
	data, err := os.ReadFile(poolPath)
	if err != nil {
		return false
	}
	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		return false
	}
	for _, k := range p.Keys {
		if k == key {
			return true
		}
	}
	return false
}

func addKeyAndMarkDirty(key string) {
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	markPoolDirtyLocked()
	poolMu.Unlock()
}

func TestMarkPoolDirtyDebouncedFlush(t *testing.T) {
	old := poolDebouncer.delay
	poolDebouncer.delay = 10 * time.Millisecond
	t.Cleanup(func() { poolDebouncer.delay = old })

	_ = loadPool()
	// 清掉可能遗留的脏状态，避免上一测试的 pending timer 提前写出本测试的变更
	poolMu.Lock()
	poolDirty = false
	poolMu.Unlock()

	const key = "p3-debounce-key"
	addKeyAndMarkDirty(key)
	if poolFileHasKey(t, key) {
		t.Fatalf("dirty change must not be persisted before debounce fires")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if poolFileHasKey(t, key) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("debounced flush did not persist change within 2s")
}

func TestFlushPoolDirtyPersistsImmediately(t *testing.T) {
	_ = loadPool()
	const key = "p3-flush-now-key"
	addKeyAndMarkDirty(key)
	flushPoolDirty()
	if !poolFileHasKey(t, key) {
		t.Fatalf("flushPoolDirty must persist dirty change immediately")
	}
}

func TestSyncSaveClearsDirtyFlag(t *testing.T) {
	_ = loadPool()
	addKeyAndMarkDirty("p3-sync-clears-key")
	savePool()
	poolMu.Lock()
	dirty := poolDirty
	poolMu.Unlock()
	if dirty {
		t.Fatalf("synchronous savePool must clear dirty flag")
	}
}

func TestSaveFailureKeepsDirty(t *testing.T) {
	_ = loadPool()
	// 把 poolPath 指到一个「以文件为父目录」的非法路径，使 WriteFile 失败（跨平台）
	oldPath := poolPath
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	poolPath = filepath.Join(blocker, "sub", ".cline-accounts.json")
	t.Cleanup(func() { poolPath = oldPath })

	addKeyAndMarkDirty("p3-fail-keep-dirty")
	flushPoolDirty()

	poolMu.Lock()
	dirty := poolDirty
	poolMu.Unlock()
	if !dirty {
		t.Fatalf("failed write must keep dirty flag for retry")
	}
}

func TestAppendRequestLogDeferredPersist(t *testing.T) {
	oldDelay := requestLogsDebouncer.delay
	oldLogs := requestLogs
	requestLogsDebouncer.delay = 10 * time.Millisecond
	t.Cleanup(func() {
		requestLogsDebouncer.delay = oldDelay
		requestLogs = oldLogs
	})

	entry := RequestLog{ID: "p3_defer_1", StartedAt: time.Now(), Model: "m", Protocol: "openai"}
	appendRequestLog(entry)

	data, _ := os.ReadFile(requestLogsPath)
	if data != nil && len(data) > 0 {
		var entries []RequestLog
		if json.Unmarshal(data, &entries) == nil {
			for _, e := range entries {
				if e.ID == "p3_defer_1" {
					t.Fatalf("appended entry must not be persisted before debounce fires")
				}
			}
		}
	}

	flushRequestLogsDirty()
	data, err := os.ReadFile(requestLogsPath)
	if err != nil {
		t.Fatalf("flush did not write log file: %v", err)
	}
	var entries []RequestLog
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("flush wrote unparseable log file: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.ID == "p3_defer_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("flushRequestLogsDirty must persist appended entry")
	}
}