//go:build windows

package main

import (
	"os"
	"syscall"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procFreeConsole   = kernel32.NewProc("FreeConsole")
	procGetFileType   = kernel32.NewProc("GetFileType")
)

const (
	attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS = 0xFFFFFFFF
	fileTypeUnknown     = 0x0000       // GetFileType 对无效句柄返回 0
)

// attachConsole 把 stdout/stderr 接到父进程的控制台上。
// 本程序以 GUI 子系统（-H=windowsgui）构建：双击启动没有控制台（不弹黑窗，
// 关闭前端也不会牵连控制台）；从 cmd/PowerShell 启动时若没有继承到有效的
// 输出句柄，则挂到父控制台上，保证 CLI 子命令的输出可见。
// stdout 已被重定向（管道/文件）或本身就是控制台句柄时不干预，避免破坏重定向
// （Git Bash / Windows Terminal 走管道，正好命中此分支）。
// 返回是否成功挂接到父控制台。
func attachConsole() bool {
	if t, _, _ := procGetFileType.Call(os.Stdout.Fd()); t != fileTypeUnknown {
		return false
	}
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 {
		return false // 没有父控制台（双击启动），保持静默
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	return true
}

// freeConsole 与本进程的控制台解绑。
// GUI 模式从 cmd 启动时调用：解绑后关闭 cmd 窗口不会终止前端进程
// （控制台关闭事件只发给仍挂接的进程）。
func freeConsole() {
	procFreeConsole.Call()
}
