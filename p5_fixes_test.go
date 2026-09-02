package main

import (
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// ============ P5-1 后台协程 panic 防护 ============

func TestSafeRunRecoversPanic(t *testing.T) {
	reached := false
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped safeRun: %v", r)
		}
		if !reached {
			t.Fatal("code after panic not reached")
		}
	}()
	safeRun("test-panic", func() {
		panic("boom")
	})
	reached = true
}

func TestGuardTickSurvivesPanic(t *testing.T) {
	n := 0
	for i := 0; i < 3; i++ {
		guardTick("test-tick", func() {
			n++
			if i == 1 {
				panic("tick panic")
			}
		})
	}
	if n != 3 {
		t.Errorf("loop iterations after mid-loop panic = %d, want 3", n)
	}
}

func TestSafeGoRunsFn(t *testing.T) {
	done := make(chan struct{})
	safeGo("test-go", func() { close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("safeGo goroutine never ran")
	}
}

func TestDebouncedFlushPanicUnsticksRunning(t *testing.T) {
	d := newDebouncedFlush(10*time.Millisecond, func() {
		panic("flush boom")
	})
	d.schedule()
	drained := make(chan struct{})
	go func() {
		d.drain()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("drain blocked after panicking flush (running stuck)")
	}
	// drain 后实例永久停用；另起实例验证正常落盘路径不受影响
	var called atomic.Int32
	d2 := newDebouncedFlush(10*time.Millisecond, func() { called.Add(1) })
	d2.schedule()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && called.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if called.Load() == 0 {
		t.Error("normal flush never fired after panic-recovery instance")
	}
	d2.drain()
}

func TestRunCommandReapsChild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}
	if err := runCommand("go", "version"); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
}