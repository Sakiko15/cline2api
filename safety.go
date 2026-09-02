package main

import (
	"log"
	"runtime/debug"
)

// safeRun 同步执行 fn 并兜底 recover：panic 记录（含堆栈）后返回，不拖垮进程。
// 本仓库后台 goroutine 均无 recover 兜底（net/http 只保护 handler 协程），
// 任一巡检循环 panic 即整个进程退出——所有后台 goroutine 一律经由 safeGo/guardTick 启动。
func safeRun(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("background goroutine %s panic: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
}

// safeGo 启动受 recover 保护的一次性后台协程：panic 记录后协程退出，进程不受影响。
func safeGo(name string, fn func()) {
	go safeRun(name, fn)
}

// guardTick 包裹长驻 ticker 循环的单轮执行：单轮 panic 只损失该轮，
// 循环本身不熄火。不用「panic 后重启整个循环」——重启逻辑自身的缺陷
// 可能造成双循环/副作用重复，逐轮 recover 机制最简且无副作用。
func guardTick(name string, fn func()) {
	safeRun(name, fn)
}