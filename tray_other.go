//go:build !windows

package main

import "fmt"

// 非 Windows 平台没有系统托盘，相关能力退化为纯 HTTP 服务模式。

type trayOptions struct {
	tip     string
	dataDir string
	onOpen  func()
	onQuit  func()
}

func startTray(opt trayOptions) error {
	return fmt.Errorf("当前平台不支持系统托盘")
}

func removeTray() {}
