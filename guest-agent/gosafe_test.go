package main

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestGoLoopRestartsAfterPanic(t *testing.T) {
	var runs atomic.Int32
	done := make(chan struct{})
	goLoop("test", func() {
		if runs.Add(1) == 1 {
			panic("boom")
		}
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop was not restarted after a panic")
	}
}

func TestGoSafeSwallowsPanic(t *testing.T) {
	done := make(chan struct{})
	goSafe("test", func() {
		defer close(done)
		panic("boom")
	})
	<-done
}
