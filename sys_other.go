//go:build !windows

package main

import "fmt"

// 非 Windows 平台的占位实现：保证 GOOS=linux/darwin 可交叉编译，
// 托盘与单实例等能力仅在 Windows 下可用。

func acquireSingleInstance(name string) (alreadyExists bool, release func()) {
	return false, func() {}
}

func attachJob(pid int) error {
	return fmt.Errorf("当前平台不支持 Job Object")
}

func terminateBrowserTree(pid int) {}

func shellOpen(target string) error {
	return fmt.Errorf("当前平台不支持 ShellExecute")
}

func iconFromICO(ico []byte) uintptr { return 0 }
