package main

import (
	"sync"
	"time"
)

// debouncedFlush 把高频「改状态→立即落盘」合并为「改状态→置脏→≤delay 后一次落盘」。
//
// 选型：首次置脏启动 AfterFunc、pending 期间不重置——Reset 型 debounce 在持续
// 高频变更下永不触发（饿死写入）；固定 ticker 空闲时空转且最坏延迟 2×tick。
// 单发 AfterFunc 语义：静止后 ≤delay 落盘一次，持续负载下稳态约 1 次/delay。
//
// 硬性约定：fire() 必须在 d.mu 之外调用 fn。fn 通常会去取调用方自己的锁
// （如 poolMu），而 schedule() 在持调用方锁时被调；若 fn 在 d.mu 内执行，
// 将形成 d.mu→poolMu 与 poolMu→d.mu 的锁序倒置死锁。
type debouncedFlush struct {
	mu      sync.Mutex
	cond    *sync.Cond
	timer   *time.Timer
	running bool // fire 正在执行 fn，供 drain 等待
	stopped bool // drain 之后永久停用
	delay   time.Duration
	fn      func()
}

func newDebouncedFlush(delay time.Duration, fn func()) *debouncedFlush {
	d := &debouncedFlush{delay: delay, fn: fn}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// schedule 排程一次落盘；已有 pending timer 时不重置（防饿死）。
func (d *debouncedFlush) schedule() {
	d.mu.Lock()
	if d.timer != nil || d.stopped {
		d.mu.Unlock()
		return
	}
	d.timer = time.AfterFunc(d.delay, d.fire)
	d.mu.Unlock()
}

// fire 由 timer 触发：清掉 pending 标记后执行 fn（fn 在 d.mu 外执行，见类型注释）。
func (d *debouncedFlush) fire() {
	d.mu.Lock()
	d.timer = nil
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()
	d.fn()
	d.mu.Lock()
	d.running = false
	d.cond.Broadcast()
	d.mu.Unlock()
}

// drain 停止排程并等待 in-flight 的 fn 执行完毕；之后该实例不再执行 fn。
// 用于全局 teardown（测试 TestMain 恢复 poolPath 等路径变量）前静默化：
// AfterFunc 已弹出但 fire 尚未运行的窗口由 stopped 标记兜底——即便
// timer.Stop() 返回 false，fire 也会看到 stopped 直接返回，不执行 fn。
func (d *debouncedFlush) drain() {
	d.mu.Lock()
	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	for d.running {
		d.cond.Wait()
	}
	d.mu.Unlock()
}