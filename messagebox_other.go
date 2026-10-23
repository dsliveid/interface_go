//go:build !windows

package main

import "log"

func messageBox(title, text string) {
	log.Printf("[%s] %s", title, text)
}

// confirmBox 在非 Windows 平台没有原生对话框，直接返回 false（不执行「是」分支）。
func confirmBox(title, text string) bool {
	log.Printf("[%s] %s（当前平台无法交互，按「否」处理）", title, text)
	return false
}
