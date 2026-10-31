//go:build !windows

package main

// watchParent is only needed where the client cannot reap its children.
func watchParent(onExit func()) {}
