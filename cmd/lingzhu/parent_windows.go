//go:build windows

package main

import (
	"os"
	"strconv"
)

// watchParent stops the core when the desktop app that spawned it goes away,
// so a crashed client cannot leave orphan processes behind.
func watchParent(onExit func()) {
	value := os.Getenv("LINGZHU_PARENT_PID")
	if value == "" {
		return
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return
	}
	go func() {
		parent, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		if _, err := parent.Wait(); err != nil {
			return
		}
		onExit()
	}()
}
