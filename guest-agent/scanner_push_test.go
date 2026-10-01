package main

import "testing"

func TestPushCurrentStateKeepsLatest(t *testing.T) {
	s := newPortScanner()
	ch := s.subscribe()
	s.running = 1
	s.pushCurrentState()
	s.running = 2
	s.pushCurrentState()
	got := <-ch
	if got.RunningContainers != 2 {
		t.Fatalf("subscriber got stale state: running=%d, want 2", got.RunningContainers)
	}
	s.unsubscribe(ch)
	// A push after unsubscribe must not touch the closed channel.
	s.pushCurrentState()
}
