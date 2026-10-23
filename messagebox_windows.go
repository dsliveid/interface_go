//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// messageBox 在双击 exe 且无控制台时，用原生弹窗提示致命错误。
func messageBox(title, text string) {
	const mbIconError = 0x00000010
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	t, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	c, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	_, _, _ = proc.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), uintptr(mbIconError))
}

// confirmBox 弹出「是 / 否」原生对话框，返回 true 表示用户点了「是」。
// 用于「程序已在后台运行」时让用户选择：是=打开界面，否=退出程序。
func confirmBox(title, text string) bool {
	const (
		mbYesNo       = 0x00000004
		mbIconQuestion = 0x00000020
		idYes         = 6
	)
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	t, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return false
	}
	c, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	r, _, _ := proc.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), mbYesNo|mbIconQuestion)
	return r == idYes
}
