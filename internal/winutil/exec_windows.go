//go:build windows

// Package winutil 提供 Windows 平台的进程小工具。
package winutil

import "syscall"

// NoWindow 返回抑制子进程控制台窗口的 SysProcAttr。
// 本工具以 GUI 子系统构建（-H=windowsgui），主进程没有控制台：默认情况下
// Windows 会为每个控制台子进程（ffmpeg/ffprobe/tasklist/reg/taskkill/HLAE 等）
// 新建一个控制台窗口（表现为瞬闪黑窗）。设置 CREATE_NO_WINDOW 后子进程静默
// 运行，输出仍可通过管道捕获（Output/CombinedOutput 不受影响）。
func NoWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
