//go:build !windows

package winutil

import "syscall"

// NoWindow 非 Windows 平台无需处理。
func NoWindow() *syscall.SysProcAttr { return nil }
