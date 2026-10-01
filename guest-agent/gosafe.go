package main

import (
	"log"
	"runtime/debug"
	"time"
)

// The agent is PID 1: a panic in any goroutine ends it, the guest kernel
// panics, and the host cold-boots the VM. Background goroutines therefore
// recover, log the stack, and — for the long-lived loops — start again.

// goSafe runs fn in a goroutine that logs and swallows a panic.
func goSafe(name string, fn func()) {
	go func() {
		defer recoverAndLog(name)
		fn()
	}()
}

// goLoop runs a long-lived loop in a goroutine and restarts it (after a
// pause) whenever it panics. A loop that returns normally is not restarted.
func goLoop(name string, fn func()) {
	go func() {
		for {
			if !runRecovered(name, fn) {
				return
			}
			time.Sleep(time.Second)
		}
	}()
}

// runRecovered calls fn and reports whether it panicked.
func runRecovered(name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[panic] %s: %v\n%s", name, r, debug.Stack())
			panicked = true
		}
	}()
	fn()
	return false
}

func recoverAndLog(name string) {
	if r := recover(); r != nil {
		log.Printf("[panic] %s: %v\n%s", name, r, debug.Stack())
	}
}
