//go:build !windows

package main

// attachConsole 非 Windows 平台无需处理（终端输出天然可见）。
func attachConsole() bool { return false }

// freeConsole 非 Windows 平台无需处理。
func freeConsole() {}
