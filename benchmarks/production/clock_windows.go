package main

import (
	"syscall"
	"time"
	"unsafe"
)

var qpcClock = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
var qpcFrequency = func() int64 {
	var frequency int64
	ok, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&frequency)))
	if ok == 0 || frequency <= 0 {
		panic(err)
	}
	return frequency
}()

// Windows 本机 time.Now 的约 0.5ms 粒度不能分辨小图；QPC 只用于评测计时。
func benchNow() int64 {
	var value int64
	if ok, _, err := qpcClock.Call(uintptr(unsafe.Pointer(&value))); ok == 0 {
		panic(err)
	}
	return value
}
func benchSince(start int64) time.Duration {
	ticks := benchNow() - start
	return time.Duration(ticks/qpcFrequency*1e9 + ticks%qpcFrequency*1e9/qpcFrequency)
}
