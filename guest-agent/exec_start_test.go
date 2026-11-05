package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Docker allows POST /exec/{id}/start once and answers 409 afterwards. The
// claim happens before the hijack, so two concurrent starts cannot create
// two processes for one exec (one orphaned, exit codes racing).
func TestExecStartOnlyOnce(t *testing.T) {
	spec := &execSpec{ID: "exec-1"}
	if !spec.beginStart() {
		t.Fatal("first start must be accepted")
	}
	if spec.beginStart() {
		t.Fatal("second start must be refused")
	}
	running, code, _ := spec.inspectState()
	if !running || code != nil {
		t.Fatalf("inspect after start = running:%v exit:%v, want running with no exit code", running, code)
	}

	spec.setExit(7)
	if spec.beginStart() {
		t.Fatal("a finished exec must not start again")
	}
	running, code, _ = spec.inspectState()
	if running || code == nil || *code != 7 {
		t.Fatalf("inspect after exit = running:%v exit:%v, want stopped with 7", running, code)
	}
}

func TestExecStartConcurrent(t *testing.T) {
	spec := &execSpec{ID: "exec-2"}
	const starts = 8
	var accepted atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < starts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if spec.beginStart() {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := accepted.Load(); got != 1 {
		t.Fatalf("accepted %d starts, want exactly 1", got)
	}
}
