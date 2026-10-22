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
