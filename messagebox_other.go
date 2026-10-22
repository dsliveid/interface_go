//go:build !windows

package main

import "log"

func messageBox(title, text string) {
	log.Printf("[%s] %s", title, text)
}
